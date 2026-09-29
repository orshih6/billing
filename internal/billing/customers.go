package billing

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	"github.com/orshih6/billing/internal/database"
	"github.com/orshih6/billing/internal/domain"
	"github.com/orshih6/billing/internal/events"
	"github.com/orshih6/billing/internal/ledger"
	"github.com/orshih6/billing/internal/models"
)

// CustomerInput creates or updates a customer.
type CustomerInput struct {
	ExternalID *string        `json:"external_id"`
	Name       *string        `json:"name"`
	Email      *string        `json:"email"`
	Phone      *string        `json:"phone"`
	Metadata   map[string]any `json:"metadata"`
	TaxExempt  *bool          `json:"tax_exempt"`
	// TaxRateBps overrides the tenant tax rate; send -1 to clear it.
	TaxRateBps *int `json:"tax_rate_bps"`
}

func applyTax(c *models.Customer, in CustomerInput) error {
	if in.TaxExempt != nil {
		c.TaxExempt = *in.TaxExempt
	}
	if in.TaxRateBps != nil {
		switch r := *in.TaxRateBps; {
		case r == -1:
			c.TaxRateBps = nil
		case r < 0 || r > 10000:
			return invalid("invalid_tax_rate", "tax_rate_bps must be 0–10000 (basis points), or -1 to clear")
		default:
			c.TaxRateBps = &r
		}
	}
	return nil
}

// CreateCustomer adds a customer. ExternalID is the integrator's own user id
// and is unique per tenant, which makes "create if missing" safe to retry.
func (s *Service) CreateCustomer(ctx context.Context, tenantID uuid.UUID, in CustomerInput) (*models.Customer, error) {
	c := &models.Customer{TenantID: tenantID, Metadata: in.Metadata}
	if in.ExternalID != nil && strings.TrimSpace(*in.ExternalID) != "" {
		c.ExternalID = ptr(strings.TrimSpace(*in.ExternalID))
	}
	if in.Name != nil {
		c.Name = strings.TrimSpace(*in.Name)
	}
	if c.Name == "" {
		return nil, invalid("invalid_name", "name is required")
	}
	if in.Email != nil {
		c.Email = strings.TrimSpace(*in.Email)
	}
	if in.Phone != nil {
		c.Phone = strings.TrimSpace(*in.Phone)
	}
	if err := applyTax(c, in); err != nil {
		return nil, err
	}
	err := s.tx(ctx, func(tx *gorm.DB) error {
		if err := tx.Create(c).Error; err != nil {
			if database.IsUniqueViolation(err) {
				return conflict("external_id_taken", "a customer with external_id %q already exists", *c.ExternalID)
			}
			return err
		}
		return events.Emit(tx, tenantID, events.CustomerCreated, c)
	})
	if err != nil {
		return nil, err
	}
	return c, nil
}

// UpdateCustomer changes the given fields.
func (s *Service) UpdateCustomer(ctx context.Context, tenantID, id uuid.UUID, in CustomerInput) (*models.Customer, error) {
	var c *models.Customer
	err := s.tx(ctx, func(tx *gorm.DB) error {
		var err error
		if c, err = lock[models.Customer](tx, tenantID, id, "customer"); err != nil {
			return err
		}
		if in.ExternalID != nil {
			if v := strings.TrimSpace(*in.ExternalID); v != "" {
				c.ExternalID = &v
			} else {
				c.ExternalID = nil
			}
		}
		if in.Name != nil && strings.TrimSpace(*in.Name) != "" {
			c.Name = strings.TrimSpace(*in.Name)
		}
		if in.Email != nil {
			c.Email = strings.TrimSpace(*in.Email)
		}
		if in.Phone != nil {
			c.Phone = strings.TrimSpace(*in.Phone)
		}
		if in.Metadata != nil {
			c.Metadata = datatypes.JSONMap(in.Metadata)
		}
		if err := applyTax(c, in); err != nil {
			return err
		}
		if err := tx.Save(c).Error; err != nil {
			if database.IsUniqueViolation(err) {
				return conflict("external_id_taken", "another customer already has that external_id")
			}
			return err
		}
		return events.Emit(tx, tenantID, events.CustomerUpdated, c)
	})
	return c, err
}

// Customer loads one customer.
func (s *Service) Customer(ctx context.Context, tenantID, id uuid.UUID) (*models.Customer, error) {
	return take[models.Customer](s.db.WithContext(ctx), tenantID, id, "customer")
}

// CustomerByExternalID loads a customer by the integrator's id.
func (s *Service) CustomerByExternalID(ctx context.Context, tenantID uuid.UUID, externalID string) (*models.Customer, error) {
	var c models.Customer
	err := s.db.WithContext(ctx).Where("tenant_id = ? AND external_id = ?", tenantID, externalID).Take(&c).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, notFound("customer")
	}
	return &c, err
}

// CustomerFilter narrows ListCustomers.
type CustomerFilter struct {
	Query string // matches name, email, phone or external_id
	Common
}

// ListCustomers lists a tenant's customers.
func (s *Service) ListCustomers(ctx context.Context, tenantID uuid.UUID, f CustomerFilter, p Page) (List[models.Customer], error) {
	q := s.db.WithContext(ctx).Model(&models.Customer{}).Where("tenant_id = ?", tenantID)
	if f.Query != "" {
		like := likePattern(f.Query)
		q = q.Where("name ILIKE ? OR email ILIKE ? OR phone ILIKE ? OR external_id ILIKE ?", like, like, like, like)
	}
	q, err := f.Common.apply(q, "customers", true)
	if err != nil {
		return List[models.Customer]{}, err
	}
	return paginate[models.Customer](q, "customers", p)
}

// Balance is a customer's position in one currency.
type Balance struct {
	Currency string `json:"currency"`
	// Owed is the receivable: finalized, unpaid invoice amounts.
	Owed int64 `json:"owed"`
	// Credit is prepaid money / overpayments held for the customer; it is
	// applied automatically to their next invoices.
	Credit int64 `json:"credit"`
}

// CustomerBalances returns the balance in every currency the customer has
// touched.
func (s *Service) CustomerBalances(ctx context.Context, tenantID, id uuid.UUID) ([]Balance, error) {
	if _, err := s.Customer(ctx, tenantID, id); err != nil {
		return nil, err
	}
	var accounts []models.LedgerAccount
	if err := s.db.WithContext(ctx).Where("tenant_id = ? AND customer_id = ?", tenantID, id).
		Order("currency").Find(&accounts).Error; err != nil {
		return nil, err
	}
	idx := map[string]int{}
	out := []Balance{}
	for _, a := range accounts {
		i, ok := idx[a.Currency]
		if !ok {
			out = append(out, Balance{Currency: a.Currency})
			i = len(out) - 1
			idx[a.Currency] = i
		}
		switch a.Code {
		case ledger.ReceivableCode(id):
			out[i].Owed = a.Balance
		case ledger.CreditCode(id):
			out[i].Credit = a.Balance
		}
	}
	return out, nil
}

// GrantCreditInput gives a customer credit (a goodwill gesture, a prepaid
// top-up recorded elsewhere, a migration from another system).
type GrantCreditInput struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
	Reason   string `json:"reason"`
}

// GrantCredit posts credit_grants → customer credit and immediately applies
// the credit to the customer's open invoices in that currency.
func (s *Service) GrantCredit(ctx context.Context, tenantID, customerID uuid.UUID, in GrantCreditInput) ([]Balance, error) {
	if in.Amount <= 0 {
		return nil, invalid("invalid_amount", "amount must be positive (minor units)")
	}
	cur, err := domain.LookupCurrency(in.Currency)
	if err != nil {
		return nil, invalid("invalid_currency", "%v", err)
	}
	err = s.tx(ctx, func(tx *gorm.DB) error {
		c, err := lock[models.Customer](tx, tenantID, customerID, "customer")
		if err != nil {
			return err
		}
		grants, err := ledger.System(tx, tenantID, ledger.CodeCreditGrants, cur.Code)
		if err != nil {
			return err
		}
		credit, err := ledger.CustomerCredit(tx, tenantID, c.ID, cur.Code)
		if err != nil {
			return err
		}
		if _, _, err := ledger.Post(tx, ledger.Posting{
			TenantID: tenantID, Kind: "credit_grant", IdempotencyKey: "credit-grant:" + uuid.NewString(),
			Currency: cur.Code, CustomerID: &c.ID, Description: in.Reason, EffectiveAt: s.now(),
			Lines: []ledger.Line{ledger.Debit(grants, in.Amount), ledger.Credit(credit, in.Amount)},
		}); err != nil {
			return err
		}
		if err := events.Emit(tx, tenantID, events.CustomerCreditGranted, map[string]any{
			"customer_id": c.ID, "amount": in.Amount, "currency": cur.Code, "reason": in.Reason,
		}); err != nil {
			return err
		}
		return s.applyCreditToOpenInvoices(tx, tenantID, c.ID, cur.Code)
	})
	if err != nil {
		return nil, err
	}
	return s.CustomerBalances(ctx, tenantID, customerID)
}

// Entitlement says a customer currently has access to a plan: a base plan
// (Kind "base", Quantity = seats) or an add-on (Kind "addon").
type Entitlement struct {
	PlanID            uuid.UUID                 `json:"plan_id"`
	PlanCode          string                    `json:"plan_code"`
	Kind              string                    `json:"kind"`
	Quantity          int                       `json:"quantity"`
	SubscriptionID    uuid.UUID                 `json:"subscription_id"`
	Status            domain.SubscriptionStatus `json:"status"`
	CurrentPeriodEnd  time.Time                 `json:"current_period_end"`
	CancelAtPeriodEnd bool                      `json:"cancel_at_period_end"`
}

// Entitlements answers the question every integrator asks: what does this
// customer have access to right now? Trialing, active and past_due (grace)
// subscriptions are entitled.
func (s *Service) Entitlements(ctx context.Context, tenantID, customerID uuid.UUID) ([]Entitlement, error) {
	if _, err := s.Customer(ctx, tenantID, customerID); err != nil {
		return nil, err
	}
	out := []Entitlement{}
	entitled := []domain.SubscriptionStatus{domain.SubTrialing, domain.SubActive, domain.SubPastDue}
	err := s.db.WithContext(ctx).Raw(`
		SELECT s.plan_id, p.code AS plan_code, 'base' AS kind, s.quantity, s.id AS subscription_id,
		       s.status, s.current_period_end, s.cancel_at_period_end
		  FROM subscriptions s JOIN plans p ON p.id = s.plan_id
		 WHERE s.tenant_id = ? AND s.customer_id = ? AND s.status IN ?
		UNION ALL
		SELECT i.plan_id, p.code, 'addon', i.quantity, s.id, s.status, s.current_period_end, s.cancel_at_period_end
		  FROM subscription_items i JOIN subscriptions s ON s.id = i.subscription_id JOIN plans p ON p.id = i.plan_id
		 WHERE s.tenant_id = ? AND s.customer_id = ? AND s.status IN ? AND i.removed_at IS NULL
		 ORDER BY kind DESC, plan_code`,
		tenantID, customerID, entitled, tenantID, customerID, entitled).Scan(&out).Error
	return out, err
}
