package billing

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	"github.com/orshih6/billing/internal/domain"
	"github.com/orshih6/billing/internal/events"
	"github.com/orshih6/billing/internal/ledger"
	"github.com/orshih6/billing/internal/models"
)

// LineInput is one charge. Either PriceID (amount and currency come from the
// price) or UnitAmount + Description.
type LineInput struct {
	PriceID     *uuid.UUID `json:"price_id"`
	Description string     `json:"description"`
	Quantity    int64      `json:"quantity"`
	UnitAmount  int64      `json:"unit_amount"`
}

// InvoiceInput creates a one-off invoice.
type InvoiceInput struct {
	CustomerID uuid.UUID      `json:"customer_id"`
	Currency   string         `json:"currency"`
	Lines      []LineInput    `json:"lines"`
	DueDays    *int           `json:"due_days"`
	Memo       string         `json:"memo"`
	Metadata   map[string]any `json:"metadata"`
	// Finalize issues the invoice immediately instead of leaving a draft.
	Finalize bool `json:"finalize"`
	// PromoCode applies a coupon to this invoice.
	PromoCode string `json:"promo_code"`
}

// CreateInvoice creates a one-off invoice (a draft unless Finalize).
func (s *Service) CreateInvoice(ctx context.Context, tenantID uuid.UUID, in InvoiceInput) (*models.Invoice, error) {
	var id uuid.UUID
	err := s.tx(ctx, func(tx *gorm.DB) error {
		t, set, err := s.settings(tx, tenantID)
		if err != nil {
			return err
		}
		if _, err := take[models.Customer](tx, tenantID, in.CustomerID, "customer"); err != nil {
			return err
		}
		if in.Currency == "" {
			in.Currency = t.DefaultCurrency
		}
		cur, err := domain.LookupCurrency(in.Currency)
		if err != nil {
			return invalid("invalid_currency", "%v", err)
		}
		inv := &models.Invoice{
			TenantID: tenantID, CustomerID: in.CustomerID, Kind: domain.InvoiceOneOff,
			Status: domain.InvoiceDraft, Currency: cur.Code, Memo: in.Memo, Metadata: in.Metadata,
		}
		days := set.InvoiceDueDays
		if in.DueDays != nil && *in.DueDays >= 0 {
			days = *in.DueDays
		}
		inv.DueAt = ptr(s.now().AddDate(0, 0, days))
		if err := tx.Omit("Lines").Create(inv).Error; err != nil {
			return err
		}
		for _, li := range in.Lines {
			if err := s.addLine(tx, inv, li); err != nil {
				return err
			}
		}
		if strings.TrimSpace(in.PromoCode) != "" {
			var plans []uuid.UUID
			if err := tx.Model(&models.InvoiceLine{}).Where("invoice_id = ? AND plan_id IS NOT NULL", inv.ID).
				Pluck("plan_id", &plans).Error; err != nil {
				return err
			}
			d, err := s.redeem(tx, tenantID, in.CustomerID, in.PromoCode, inv.Currency, plans, nil, &inv.ID)
			if err != nil {
				return err
			}
			inv.DiscountID = &d.ID
			if err := tx.Model(inv).Update("discount_id", d.ID).Error; err != nil {
				return err
			}
			if err := s.computeTotals(tx, inv); err != nil {
				return err
			}
		}
		id = inv.ID
		if err := events.Emit(tx, tenantID, events.InvoiceCreated, inv); err != nil {
			return err
		}
		if in.Finalize {
			return s.finalizeTx(tx, tenantID, inv.ID)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.Invoice(ctx, tenantID, id)
}

// AddInvoiceLine adds a line to a draft.
func (s *Service) AddInvoiceLine(ctx context.Context, tenantID, invoiceID uuid.UUID, li LineInput) (*models.Invoice, error) {
	err := s.tx(ctx, func(tx *gorm.DB) error {
		inv, err := lock[models.Invoice](tx, tenantID, invoiceID, "invoice")
		if err != nil {
			return err
		}
		if inv.Status != domain.InvoiceDraft {
			return conflict("invoice_not_draft", "lines can only be added to a draft invoice")
		}
		return s.addLine(tx, inv, li)
	})
	if err != nil {
		return nil, err
	}
	return s.Invoice(ctx, tenantID, invoiceID)
}

func (s *Service) addLine(tx *gorm.DB, inv *models.Invoice, li LineInput) error {
	if li.Quantity == 0 {
		li.Quantity = 1
	}
	if li.Quantity < 0 {
		return invalid("invalid_quantity", "quantity must be positive")
	}
	line := models.InvoiceLine{InvoiceID: inv.ID, Quantity: li.Quantity, Description: strings.TrimSpace(li.Description)}
	if li.PriceID != nil {
		pr, err := take[models.Price](tx, inv.TenantID, *li.PriceID, "price")
		if err != nil {
			return err
		}
		if pr.Currency != inv.Currency {
			return invalid("currency_mismatch", "price is %s, invoice is %s", pr.Currency, inv.Currency)
		}
		line.PriceID = &pr.ID
		line.PlanID = &pr.PlanID
		line.UnitAmount = pr.Amount
		if line.Description == "" {
			var plan models.Plan
			tx.Take(&plan, "id = ?", pr.PlanID)
			line.Description = plan.Name
		}
	} else {
		if li.UnitAmount < 0 {
			return invalid("invalid_amount", "unit_amount cannot be negative")
		}
		line.UnitAmount = li.UnitAmount
	}
	if line.Description == "" {
		return invalid("invalid_description", "a line needs a description")
	}
	line.Amount = line.Quantity * line.UnitAmount
	if err := tx.Create(&line).Error; err != nil {
		return err
	}
	return s.computeTotals(tx, inv)
}

// FinalizeInvoice issues a draft.
func (s *Service) FinalizeInvoice(ctx context.Context, tenantID, id uuid.UUID) (*models.Invoice, error) {
	if err := s.tx(ctx, func(tx *gorm.DB) error { return s.finalizeTx(tx, tenantID, id) }); err != nil {
		return nil, err
	}
	return s.Invoice(ctx, tenantID, id)
}

// finalizeTx turns a draft into an open invoice:
//
//  1. compute discount and tax with the settings in force now;
//  2. assign the next invoice number and a short payment reference;
//  3. post it: receivable (total) + discounts ← revenue (net of tax) + tax payable;
//  4. apply any credit the customer holds;
//  5. if nothing is left to pay, the invoice is paid right away.
func (s *Service) finalizeTx(tx *gorm.DB, tenantID, id uuid.UUID) error {
	inv, err := lock[models.Invoice](tx, tenantID, id, "invoice")
	if err != nil {
		return err
	}
	if inv.Status != domain.InvoiceDraft {
		return conflict("invoice_not_draft", "only a draft can be finalized (status is %s)", inv.Status)
	}
	if err := s.computeTotals(tx, inv); err != nil {
		return err
	}
	var seq int64
	if err := tx.Raw(`UPDATE tenants SET invoice_seq = invoice_seq + 1 WHERE id = ? RETURNING invoice_seq`, tenantID).
		Scan(&seq).Error; err != nil {
		return err
	}
	_, set, err := s.settings(tx, tenantID)
	if err != nil {
		return err
	}
	now := s.now()
	inv.Number = ptr(fmt.Sprintf("%s-%06d", set.InvoicePrefix, seq))
	inv.Reference, err = uniqueReference(tx, tenantID)
	if err != nil {
		return err
	}
	inv.Status = domain.InvoiceOpen
	inv.FinalizedAt = &now
	inv.AmountDue = inv.Total - inv.AmountPaid - inv.CreditApplied
	if inv.DueAt == nil {
		inv.DueAt = ptr(now.AddDate(0, 0, set.InvoiceDueDays))
	}
	if err := tx.Omit("Lines").Save(inv).Error; err != nil {
		return err
	}

	if inv.Subtotal > 0 {
		accts, err := s.invoiceAccounts(tx, inv)
		if err != nil {
			return err
		}
		revenue, discount, tax := revenueAndTax(inv)
		if _, _, err := ledger.Post(tx, ledger.Posting{
			TenantID: tenantID, Kind: "invoice_finalized", IdempotencyKey: "invoice-finalize:" + inv.ID.String(),
			Currency: inv.Currency, InvoiceID: &inv.ID, CustomerID: &inv.CustomerID,
			Description: "Invoice " + *inv.Number, EffectiveAt: now,
			Lines: []ledger.Line{
				ledger.Debit(accts.recv, inv.Total),
				ledger.Debit(accts.discounts, discount),
				ledger.Credit(accts.revenue, revenue),
				ledger.Credit(accts.tax, tax),
			},
		}); err != nil {
			return err
		}
	}
	if err := events.Emit(tx, tenantID, events.InvoiceFinalized, inv); err != nil {
		return err
	}
	if inv.Total > 0 {
		if err := s.applyCredit(tx, inv); err != nil {
			return err
		}
	}
	if inv.AmountDue == 0 {
		return s.markPaid(tx, inv)
	}
	return nil
}

func uniqueReference(tx *gorm.DB, tenantID uuid.UUID) (*string, error) {
	for range 10 {
		ref := domain.NewReference(6)
		var n int64
		if err := tx.Model(&models.Invoice{}).Where("tenant_id = ? AND reference = ?", tenantID, ref).Count(&n).Error; err != nil {
			return nil, err
		}
		if n == 0 {
			return &ref, nil
		}
	}
	return nil, fmt.Errorf("billing: could not allocate a unique invoice reference")
}

// applyCredit moves as much of the customer's credit balance as possible onto
// an open invoice (locked by the caller). It does not mark the invoice paid.
func (s *Service) applyCredit(tx *gorm.DB, inv *models.Invoice) error {
	if inv.Status != domain.InvoiceOpen || inv.AmountDue == 0 {
		return nil
	}
	credit, err := ledger.CustomerCredit(tx, inv.TenantID, inv.CustomerID, inv.Currency)
	if err != nil {
		return err
	}
	apply := min(credit.Balance, inv.AmountDue)
	if apply <= 0 {
		return nil
	}
	recv, err := ledger.Receivable(tx, inv.TenantID, inv.CustomerID, inv.Currency)
	if err != nil {
		return err
	}
	// The key includes how much was settled before, so a second application
	// to the same invoice (after a later grant) is a distinct posting while a
	// retry of this one is not.
	key := fmt.Sprintf("invoice-credit:%s:%d:%d", inv.ID, inv.AmountPaid, inv.CreditApplied)
	if _, _, err := ledger.Post(tx, ledger.Posting{
		TenantID: inv.TenantID, Kind: "credit_applied", IdempotencyKey: key,
		Currency: inv.Currency, InvoiceID: &inv.ID, CustomerID: &inv.CustomerID,
		Description: "Credit applied to " + deref(inv.Number), EffectiveAt: s.now(),
		Lines: []ledger.Line{ledger.Debit(credit, apply), ledger.Credit(recv, apply)},
	}); err != nil {
		return err
	}
	inv.CreditApplied += apply
	inv.AmountDue -= apply
	return tx.Model(&models.Invoice{}).Where("id = ?", inv.ID).
		Updates(map[string]any{"credit_applied": inv.CreditApplied, "amount_due": inv.AmountDue}).Error
}

// applyCreditToOpenInvoices spends a customer's credit on their open invoices,
// oldest due first, marking any that become fully covered as paid.
func (s *Service) applyCreditToOpenInvoices(tx *gorm.DB, tenantID, customerID uuid.UUID, currency string) error {
	var ids []uuid.UUID
	if err := tx.Model(&models.Invoice{}).
		Where("tenant_id = ? AND customer_id = ? AND currency = ? AND status = ? AND amount_due > 0",
			tenantID, customerID, currency, domain.InvoiceOpen).
		Order("due_at NULLS LAST").Order("created_at").Pluck("id", &ids).Error; err != nil {
		return err
	}
	for _, id := range ids {
		inv, err := lock[models.Invoice](tx, tenantID, id, "invoice")
		if err != nil {
			return err
		}
		if err := s.applyCredit(tx, inv); err != nil {
			return err
		}
		if inv.Status == domain.InvoiceOpen && inv.AmountDue == 0 {
			if err := s.markPaid(tx, inv); err != nil {
				return err
			}
		}
	}
	return nil
}

// markPaid closes a fully settled invoice and lets its subscription react.
func (s *Service) markPaid(tx *gorm.DB, inv *models.Invoice) error {
	now := s.now()
	inv.Status = domain.InvoicePaid
	inv.PaidAt = &now
	if err := tx.Model(&models.Invoice{}).Where("id = ?", inv.ID).
		Updates(map[string]any{"status": inv.Status, "paid_at": now, "amount_due": 0}).Error; err != nil {
		return err
	}
	inv.AmountDue = 0
	if err := events.Emit(tx, inv.TenantID, events.InvoicePaid, inv); err != nil {
		return err
	}
	if inv.SubscriptionID != nil {
		return s.onSubscriptionInvoicePaid(tx, inv)
	}
	return nil
}

// VoidInvoice cancels an invoice that should never have been issued. For an
// open invoice the revenue is reversed; anything already paid on it (money or
// credit) is returned to the customer's credit balance, never lost.
func (s *Service) VoidInvoice(ctx context.Context, tenantID, id uuid.UUID, reason string) (*models.Invoice, error) {
	if err := s.tx(ctx, func(tx *gorm.DB) error { return s.voidTx(tx, tenantID, id, reason) }); err != nil {
		return nil, err
	}
	return s.Invoice(ctx, tenantID, id)
}

func (s *Service) voidTx(tx *gorm.DB, tenantID, id uuid.UUID, reason string) error {
	inv, err := lock[models.Invoice](tx, tenantID, id, "invoice")
	if err != nil {
		return err
	}
	switch inv.Status {
	case domain.InvoiceDraft:
	case domain.InvoiceOpen:
		if inv.Subtotal > 0 {
			accts, err := s.invoiceAccounts(tx, inv)
			if err != nil {
				return err
			}
			revenue, discount, tax := revenueAndTax(inv)
			settled := inv.Total - inv.AmountDue
			if _, _, err := ledger.Post(tx, ledger.Posting{
				TenantID: tenantID, Kind: "invoice_voided", IdempotencyKey: "invoice-void:" + inv.ID.String(),
				Currency: inv.Currency, InvoiceID: &inv.ID, CustomerID: &inv.CustomerID,
				Description: "Void " + deref(inv.Number), EffectiveAt: s.now(),
				Lines: []ledger.Line{
					ledger.Debit(accts.revenue, revenue),
					ledger.Debit(accts.tax, tax),
					ledger.Credit(accts.discounts, discount),
					ledger.Credit(accts.recv, inv.AmountDue),
					ledger.Credit(accts.credit, settled),
				},
			}); err != nil {
				return err
			}
		}
	default:
		return conflict("invoice_not_voidable", "a %s invoice cannot be voided; refund its payments instead", inv.Status)
	}
	now := s.now()
	meta := map[string]any{}
	for k, v := range inv.Metadata {
		meta[k] = v
	}
	if reason != "" {
		meta["void_reason"] = reason
	}
	if err := tx.Model(&models.Invoice{}).Where("id = ?", inv.ID).
		Updates(map[string]any{"status": domain.InvoiceVoid, "voided_at": now, "metadata": toJSONMap(meta)}).Error; err != nil {
		return err
	}
	inv.Status = domain.InvoiceVoid
	inv.VoidedAt = &now
	if err := s.cancelPendingPayments(tx, inv.ID, "invoice voided"); err != nil {
		return err
	}
	if err := events.Emit(tx, tenantID, events.InvoiceVoided, inv); err != nil {
		return err
	}
	// Money that was sitting on the voided invoice is now credit; let it pay
	// anything else the customer owes.
	if inv.Total-inv.AmountDue > 0 {
		if err := s.applyCreditToOpenInvoices(tx, tenantID, inv.CustomerID, inv.Currency); err != nil {
			return err
		}
	}
	// A new subscription whose first invoice is voided can never activate.
	if inv.SubscriptionID != nil && inv.Kind == domain.InvoiceSubscriptionCreate {
		return s.expireIncompleteTx(tx, tenantID, *inv.SubscriptionID, false)
	}
	return nil
}

// MarkUncollectible writes off what is still owed as bad debt. What was
// already paid stays revenue.
func (s *Service) MarkUncollectible(ctx context.Context, tenantID, id uuid.UUID) (*models.Invoice, error) {
	if err := s.tx(ctx, func(tx *gorm.DB) error { return s.uncollectibleTx(tx, tenantID, id) }); err != nil {
		return nil, err
	}
	return s.Invoice(ctx, tenantID, id)
}

func (s *Service) uncollectibleTx(tx *gorm.DB, tenantID, id uuid.UUID) error {
	inv, err := lock[models.Invoice](tx, tenantID, id, "invoice")
	if err != nil {
		return err
	}
	if inv.Status != domain.InvoiceOpen {
		return conflict("invoice_not_open", "only an open invoice can be marked uncollectible")
	}
	if inv.AmountDue > 0 {
		bad, err := ledger.System(tx, tenantID, ledger.CodeBadDebt, inv.Currency)
		if err != nil {
			return err
		}
		recv, err := ledger.Receivable(tx, tenantID, inv.CustomerID, inv.Currency)
		if err != nil {
			return err
		}
		if _, _, err := ledger.Post(tx, ledger.Posting{
			TenantID: tenantID, Kind: "invoice_uncollectible", IdempotencyKey: "invoice-uncollectible:" + inv.ID.String(),
			Currency: inv.Currency, InvoiceID: &inv.ID, CustomerID: &inv.CustomerID,
			Description: "Write off " + deref(inv.Number), EffectiveAt: s.now(),
			Lines: []ledger.Line{ledger.Debit(bad, inv.AmountDue), ledger.Credit(recv, inv.AmountDue)},
		}); err != nil {
			return err
		}
	}
	inv.Status = domain.InvoiceUncollectible
	if err := tx.Model(&models.Invoice{}).Where("id = ?", inv.ID).Update("status", inv.Status).Error; err != nil {
		return err
	}
	if err := s.cancelPendingPayments(tx, inv.ID, "invoice written off"); err != nil {
		return err
	}
	return events.Emit(tx, tenantID, events.InvoiceUncollectible, inv)
}

// Invoice loads an invoice with its lines.
func (s *Service) Invoice(ctx context.Context, tenantID, id uuid.UUID) (*models.Invoice, error) {
	return take[models.Invoice](s.db.WithContext(ctx).Preload("Lines", func(db *gorm.DB) *gorm.DB {
		return db.Order("created_at")
	}), tenantID, id, "invoice")
}

// InvoiceFilter narrows ListInvoices.
type InvoiceFilter struct {
	CustomerID     *uuid.UUID
	SubscriptionID *uuid.UUID
	Status         string
	// Query matches the invoice number or payment reference.
	Query string
	Common
}

// ListInvoices lists invoices, newest first.
func (s *Service) ListInvoices(ctx context.Context, tenantID uuid.UUID, f InvoiceFilter, p Page) (List[models.Invoice], error) {
	q := s.db.WithContext(ctx).Model(&models.Invoice{}).Where("tenant_id = ?", tenantID).Preload("Lines")
	if f.CustomerID != nil {
		q = q.Where("customer_id = ?", *f.CustomerID)
	}
	if f.SubscriptionID != nil {
		q = q.Where("subscription_id = ?", *f.SubscriptionID)
	}
	if f.Status != "" {
		q = q.Where("status = ?", f.Status)
	}
	if f.Query != "" {
		like := likePattern(f.Query)
		q = q.Where("number ILIKE ? OR reference ILIKE ?", like, like)
	}
	q, err := f.Common.apply(q, "invoices", true)
	if err != nil {
		return List[models.Invoice]{}, err
	}
	return paginate[models.Invoice](q, "invoices", p)
}

// newSubscriptionInvoice builds and finalizes the invoice for one period of a
// subscription, one line per base plan / add-on.
func (s *Service) newSubscriptionInvoice(tx *gorm.DB, sub *models.Subscription, lines []lineSpec, kind domain.InvoiceKind, start, end, due time.Time) (*models.Invoice, error) {
	if len(lines) == 0 {
		return nil, fmt.Errorf("billing: subscription %s has nothing to bill", sub.ID)
	}
	// The caller holds the subscription's row lock, so checking first is
	// race-free; ux_invoice_subscription_period is the backstop. (Letting the
	// insert fail instead would abort the caller's whole transaction.)
	if existing, err := s.periodInvoice(tx, sub.ID, start); err != nil {
		return nil, err
	} else if existing != nil {
		return nil, errAlreadyInvoiced
	}
	discountID, err := s.subscriptionDiscount(tx, sub, kind, start)
	if err != nil {
		return nil, err
	}
	inv := &models.Invoice{
		TenantID: sub.TenantID, CustomerID: sub.CustomerID, SubscriptionID: &sub.ID,
		Kind: kind, Status: domain.InvoiceDraft, Currency: lines[0].Currency,
		PeriodStart: &start, PeriodEnd: &end, DueAt: &due, DiscountID: discountID,
	}
	if err := tx.Omit("Lines").Create(inv).Error; err != nil {
		return nil, err
	}
	period := fmt.Sprintf(" (%s – %s)", start.Format("2006-01-02"), end.Format("2006-01-02"))
	for _, l := range lines {
		line := models.InvoiceLine{
			InvoiceID: inv.ID, PriceID: l.PriceID, PlanID: &l.PlanID, SubscriptionItemID: l.ItemID,
			Quantity: l.Quantity, UnitAmount: l.UnitAmount, Amount: l.Quantity * l.UnitAmount,
			Description: l.Description + period, PeriodStart: &start, PeriodEnd: &end,
		}
		if err := tx.Create(&line).Error; err != nil {
			return nil, err
		}
	}
	if err := events.Emit(tx, sub.TenantID, events.InvoiceCreated, inv); err != nil {
		return nil, err
	}
	if err := s.finalizeTx(tx, sub.TenantID, inv.ID); err != nil {
		return nil, err
	}
	return take[models.Invoice](tx, sub.TenantID, inv.ID, "invoice")
}

// periodInvoice finds the live (draft, open or paid) invoice for the
// subscription period starting at start.
func (s *Service) periodInvoice(tx *gorm.DB, subID uuid.UUID, start time.Time) (*models.Invoice, error) {
	var inv models.Invoice
	err := tx.Preload("Lines").Where("subscription_id = ? AND period_start = ? AND kind IN ? AND status IN ?", subID, start,
		[]domain.InvoiceKind{domain.InvoiceSubscriptionCreate, domain.InvoiceSubscriptionCycle},
		[]domain.InvoiceStatus{domain.InvoiceDraft, domain.InvoiceOpen, domain.InvoicePaid}).
		Take(&inv).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &inv, nil
}

// invoiceAccounts are the ledger accounts an invoice posts to.
type invoiceAccts struct {
	recv, credit, revenue, discounts, tax *models.LedgerAccount
}

func (s *Service) invoiceAccounts(tx *gorm.DB, inv *models.Invoice) (*invoiceAccts, error) {
	var a invoiceAccts
	var err error
	if a.recv, err = ledger.Receivable(tx, inv.TenantID, inv.CustomerID, inv.Currency); err != nil {
		return nil, err
	}
	if a.credit, err = ledger.CustomerCredit(tx, inv.TenantID, inv.CustomerID, inv.Currency); err != nil {
		return nil, err
	}
	if a.revenue, err = ledger.System(tx, inv.TenantID, ledger.CodeRevenue, inv.Currency); err != nil {
		return nil, err
	}
	if a.discounts, err = ledger.System(tx, inv.TenantID, ledger.CodeDiscounts, inv.Currency); err != nil {
		return nil, err
	}
	if a.tax, err = ledger.System(tx, inv.TenantID, ledger.CodeTaxPayable, inv.Currency); err != nil {
		return nil, err
	}
	return &a, nil
}

var errAlreadyInvoiced = conflict("period_already_invoiced", "this subscription period already has an invoice")

func toJSONMap(m map[string]any) datatypes.JSONMap { return datatypes.JSONMap(m) }

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
