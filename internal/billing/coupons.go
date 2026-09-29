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
	"gorm.io/gorm/clause"

	"github.com/orshih6/billing/internal/database"
	"github.com/orshih6/billing/internal/domain"
	"github.com/orshih6/billing/internal/models"
)

// CouponInput creates a coupon.
type CouponInput struct {
	Code            string     `json:"code"`
	Name            string     `json:"name"`
	PercentOff      *int       `json:"percent_off"`
	AmountOff       *int64     `json:"amount_off"`
	Currency        string     `json:"currency"`
	Duration        string     `json:"duration"`
	DurationPeriods *int       `json:"duration_periods"`
	PlanIDs         []string   `json:"plan_ids"`
	MaxRedemptions  *int       `json:"max_redemptions"`
	RedeemBy        *time.Time `json:"redeem_by"`
}

// normalizeCode makes promo codes case-insensitive.
func normalizeCode(code string) string { return strings.ToUpper(strings.TrimSpace(code)) }

// CreateCoupon adds a coupon.
func (s *Service) CreateCoupon(ctx context.Context, tenantID uuid.UUID, in CouponInput) (*models.Coupon, error) {
	c := &models.Coupon{
		TenantID: tenantID, Code: normalizeCode(in.Code), Name: strings.TrimSpace(in.Name),
		Duration: in.Duration, DurationPeriods: in.DurationPeriods, MaxRedemptions: in.MaxRedemptions,
		RedeemBy: in.RedeemBy, Active: true, PlanIDs: datatypes.JSONSlice[string](nonNil(in.PlanIDs)),
	}
	if c.Code == "" || len(c.Code) > 64 || strings.ContainsAny(c.Code, " \t") {
		return nil, invalid("invalid_code", "code is required, without spaces, at most 64 characters")
	}
	if c.Name == "" {
		c.Name = c.Code
	}
	switch {
	case in.PercentOff != nil && in.AmountOff == nil:
		if *in.PercentOff < 1 || *in.PercentOff > 100 {
			return nil, invalid("invalid_percent_off", "percent_off must be 1–100")
		}
		c.PercentOff = in.PercentOff
	case in.AmountOff != nil && in.PercentOff == nil:
		if *in.AmountOff <= 0 {
			return nil, invalid("invalid_amount_off", "amount_off must be positive (minor units)")
		}
		cur, err := domain.LookupCurrency(in.Currency)
		if err != nil {
			return nil, invalid("invalid_currency", "amount_off needs a currency: %v", err)
		}
		c.AmountOff, c.Currency = in.AmountOff, cur.Code
	default:
		return nil, invalid("invalid_discount", "give exactly one of percent_off or amount_off")
	}
	switch c.Duration {
	case "":
		c.Duration = models.CouponOnce
	case models.CouponOnce, models.CouponForever:
		c.DurationPeriods = nil
	case models.CouponRepeating:
		if c.DurationPeriods == nil || *c.DurationPeriods < 1 {
			return nil, invalid("invalid_duration", "repeating coupons need duration_periods >= 1")
		}
	default:
		return nil, invalid("invalid_duration", "duration must be once, repeating or forever")
	}
	if c.MaxRedemptions != nil && *c.MaxRedemptions < 1 {
		return nil, invalid("invalid_max_redemptions", "max_redemptions must be >= 1")
	}
	for _, id := range c.PlanIDs {
		pid, err := uuid.Parse(id)
		if err != nil {
			return nil, invalid("invalid_plan_ids", "plan_ids must be plan ids")
		}
		if _, err := take[models.Plan](s.db.WithContext(ctx), tenantID, pid, "plan"); err != nil {
			return nil, err
		}
	}
	if err := s.db.WithContext(ctx).Create(c).Error; err != nil {
		if database.IsUniqueViolation(err) {
			return nil, conflict("code_taken", "a coupon with code %q already exists", c.Code)
		}
		return nil, err
	}
	return c, nil
}

// UpdateCouponInput changes what may change after creation. The discount
// itself never changes: create a new coupon instead.
type UpdateCouponInput struct {
	Name           *string    `json:"name"`
	Active         *bool      `json:"active"`
	MaxRedemptions *int       `json:"max_redemptions"`
	RedeemBy       *time.Time `json:"redeem_by"`
}

// UpdateCoupon changes a coupon.
func (s *Service) UpdateCoupon(ctx context.Context, tenantID, id uuid.UUID, in UpdateCouponInput) (*models.Coupon, error) {
	c, err := take[models.Coupon](s.db.WithContext(ctx), tenantID, id, "coupon")
	if err != nil {
		return nil, err
	}
	if in.Name != nil && strings.TrimSpace(*in.Name) != "" {
		c.Name = strings.TrimSpace(*in.Name)
	}
	if in.Active != nil {
		c.Active = *in.Active
	}
	if in.MaxRedemptions != nil {
		if *in.MaxRedemptions < c.TimesRedeemed || *in.MaxRedemptions < 1 {
			return nil, invalid("invalid_max_redemptions", "max_redemptions cannot be below times already redeemed (%d)", c.TimesRedeemed)
		}
		c.MaxRedemptions = in.MaxRedemptions
	}
	if in.RedeemBy != nil {
		c.RedeemBy = in.RedeemBy
	}
	return c, s.db.WithContext(ctx).Save(c).Error
}

// Coupon loads one coupon.
func (s *Service) Coupon(ctx context.Context, tenantID, id uuid.UUID) (*models.Coupon, error) {
	return take[models.Coupon](s.db.WithContext(ctx), tenantID, id, "coupon")
}

// ListCoupons lists coupons.
func (s *Service) ListCoupons(ctx context.Context, tenantID uuid.UUID, p Page) (List[models.Coupon], error) {
	return paginate[models.Coupon](s.db.WithContext(ctx).Model(&models.Coupon{}).Where("tenant_id = ?", tenantID), "coupons", p)
}

// CheckPromoCode tells a checkout page whether a code can be redeemed, without
// redeeming it.
func (s *Service) CheckPromoCode(ctx context.Context, tenantID uuid.UUID, code, currency string) (*models.Coupon, error) {
	var c models.Coupon
	if err := s.db.WithContext(ctx).Where("tenant_id = ? AND code = ?", tenantID, normalizeCode(code)).Take(&c).Error; err != nil {
		return nil, notFound("coupon")
	}
	if err := s.redeemable(&c, currency); err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *Service) redeemable(c *models.Coupon, currency string) error {
	switch {
	case !c.Active:
		return invalid("coupon_inactive", "promo code %s is no longer active", c.Code)
	case c.RedeemBy != nil && s.now().After(*c.RedeemBy):
		return invalid("coupon_expired", "promo code %s has expired", c.Code)
	case c.MaxRedemptions != nil && c.TimesRedeemed >= *c.MaxRedemptions:
		return invalid("coupon_used_up", "promo code %s has been used the maximum number of times", c.Code)
	case c.AmountOff != nil && currency != "" && c.Currency != currency:
		return invalid("coupon_currency", "promo code %s is for %s, not %s", c.Code, c.Currency, currency)
	}
	return nil
}

// redeem validates a code against what it would discount and records the
// redemption. planIDs are the plans being bought; a plan-restricted coupon
// must match at least one.
func (s *Service) redeem(tx *gorm.DB, tenantID, customerID uuid.UUID, code, currency string, planIDs []uuid.UUID, subID, invoiceID *uuid.UUID) (*models.Discount, error) {
	var c models.Coupon
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("tenant_id = ? AND code = ?", tenantID, normalizeCode(code)).Take(&c).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, invalid("coupon_not_found", "promo code %q does not exist", strings.TrimSpace(code))
	}
	if err != nil {
		return nil, err
	}
	if err := s.redeemable(&c, currency); err != nil {
		return nil, err
	}
	if len(c.PlanIDs) > 0 {
		ok := false
		for _, id := range planIDs {
			if c.AppliesTo(&id) {
				ok = true
			}
		}
		if !ok {
			return nil, invalid("coupon_not_applicable", "promo code %s does not apply to these plans", c.Code)
		}
	}
	if err := tx.Model(&c).Update("times_redeemed", gorm.Expr("times_redeemed + 1")).Error; err != nil {
		return nil, err
	}
	d := &models.Discount{TenantID: tenantID, CouponID: c.ID, CustomerID: customerID, SubscriptionID: subID, InvoiceID: invoiceID}
	return d, tx.Create(d).Error
}

// subscriptionDiscount returns the discount to put on a subscription invoice,
// or nil. A once/repeating coupon covers that many billing periods; periods are
// counted from the live invoices that carried it, so voiding and reissuing an
// invoice does not use a period up twice.
func (s *Service) subscriptionDiscount(tx *gorm.DB, sub *models.Subscription, kind domain.InvoiceKind, periodStart time.Time) (*uuid.UUID, error) {
	if sub.DiscountID == nil {
		return nil, nil
	}
	var d models.Discount
	if err := tx.Take(&d, "id = ?", *sub.DiscountID).Error; err != nil || d.EndedAt != nil {
		return nil, nil
	}
	var c models.Coupon
	if err := tx.Take(&c, "id = ?", d.CouponID).Error; err != nil {
		return nil, err
	}
	if c.Duration == models.CouponForever {
		return &d.ID, nil
	}
	periods := 1
	if c.Duration == models.CouponRepeating && c.DurationPeriods != nil {
		periods = *c.DurationPeriods
	}
	live := []domain.InvoiceStatus{domain.InvoiceDraft, domain.InvoiceOpen, domain.InvoicePaid}
	periodKinds := []domain.InvoiceKind{domain.InvoiceSubscriptionCreate, domain.InvoiceSubscriptionCycle}
	if kind == domain.InvoiceProration {
		// A mid-period charge is discounted only if the current period was.
		var n int64
		err := tx.Model(&models.Invoice{}).Where("discount_id = ? AND kind IN ? AND status IN ? AND period_start = ?",
			d.ID, periodKinds, live, sub.CurrentPeriodStart).Count(&n).Error
		if err != nil || n == 0 {
			return nil, err
		}
		return &d.ID, nil
	}
	var used int64
	if err := tx.Model(&models.Invoice{}).Where("discount_id = ? AND kind IN ? AND status IN ? AND period_start <> ?",
		d.ID, periodKinds, live, periodStart).Distinct("period_start").Count(&used).Error; err != nil {
		return nil, err
	}
	if int(used) >= periods {
		return nil, nil
	}
	return &d.ID, nil
}

// DiscountView describes a subscription's discount.
type DiscountView struct {
	ID          uuid.UUID `json:"id"`
	CouponID    uuid.UUID `json:"coupon_id"`
	Code        string    `json:"code"`
	Name        string    `json:"name"`
	PercentOff  *int      `json:"percent_off"`
	AmountOff   *int64    `json:"amount_off"`
	Currency    string    `json:"currency,omitempty"`
	Duration    string    `json:"duration"`
	Periods     *int      `json:"duration_periods"`
	PeriodsUsed int64     `json:"periods_used"`
	Description string    `json:"description"`
}

func describeCoupon(c *models.Coupon) string {
	if c.PercentOff != nil {
		return fmt.Sprintf("%s (%d%% off)", c.Name, *c.PercentOff)
	}
	return fmt.Sprintf("%s (%s off)", c.Name, domain.FormatAmount(*c.AmountOff, c.Currency))
}

// ApplyPromoCode redeems a code on a subscription, replacing any discount it
// had. An already-raised, unpaid renewal is reissued with the discount.
func (s *Service) ApplyPromoCode(ctx context.Context, tenantID, subID uuid.UUID, code string) (*SubscriptionView, error) {
	err := s.tx(ctx, func(tx *gorm.DB) error {
		sub, err := lock[models.Subscription](tx, tenantID, subID, "subscription")
		if err != nil {
			return err
		}
		if !sub.Status.Live() {
			return conflict("subscription_ended", "subscription is %s", sub.Status)
		}
		price, err := take[models.Price](tx, tenantID, sub.PriceID, "price")
		if err != nil {
			return err
		}
		plans, err := s.subscriptionPlanIDs(tx, sub)
		if err != nil {
			return err
		}
		d, err := s.redeem(tx, tenantID, sub.CustomerID, code, price.Currency, plans, &sub.ID, nil)
		if err != nil {
			return err
		}
		if err := s.endDiscount(tx, sub); err != nil {
			return err
		}
		sub.DiscountID = &d.ID
		if err := tx.Save(sub).Error; err != nil {
			return err
		}
		if sub.Status != domain.SubIncomplete {
			if err := s.voidUnpaidPeriodInvoice(tx, sub, "discount applied"); err != nil {
				return err
			}
		}
		return s.emitSubUpdated(tx, sub)
	})
	if err != nil {
		return nil, err
	}
	return s.Subscription(ctx, tenantID, subID)
}

// RemoveDiscount ends a subscription's discount from its next invoice on.
func (s *Service) RemoveDiscount(ctx context.Context, tenantID, subID uuid.UUID) (*SubscriptionView, error) {
	err := s.tx(ctx, func(tx *gorm.DB) error {
		sub, err := lock[models.Subscription](tx, tenantID, subID, "subscription")
		if err != nil {
			return err
		}
		if sub.DiscountID == nil {
			return conflict("no_discount", "subscription has no discount")
		}
		if err := s.endDiscount(tx, sub); err != nil {
			return err
		}
		sub.DiscountID = nil
		if err := tx.Save(sub).Error; err != nil {
			return err
		}
		if err := s.voidUnpaidPeriodInvoice(tx, sub, "discount removed"); err != nil {
			return err
		}
		return s.emitSubUpdated(tx, sub)
	})
	if err != nil {
		return nil, err
	}
	return s.Subscription(ctx, tenantID, subID)
}

func (s *Service) endDiscount(tx *gorm.DB, sub *models.Subscription) error {
	if sub.DiscountID == nil {
		return nil
	}
	return tx.Model(&models.Discount{}).Where("id = ? AND ended_at IS NULL", *sub.DiscountID).Update("ended_at", s.now()).Error
}

func (s *Service) discountView(tx *gorm.DB, id uuid.UUID) *DiscountView {
	var d models.Discount
	var c models.Coupon
	if tx.Take(&d, "id = ?", id).Error != nil || tx.Take(&c, "id = ?", d.CouponID).Error != nil {
		return nil
	}
	v := &DiscountView{ID: d.ID, CouponID: c.ID, Code: c.Code, Name: c.Name, PercentOff: c.PercentOff, AmountOff: c.AmountOff,
		Currency: c.Currency, Duration: c.Duration, Periods: c.DurationPeriods, Description: describeCoupon(&c)}
	tx.Model(&models.Invoice{}).Where("discount_id = ? AND kind IN ? AND status IN ?", d.ID,
		[]domain.InvoiceKind{domain.InvoiceSubscriptionCreate, domain.InvoiceSubscriptionCycle},
		[]domain.InvoiceStatus{domain.InvoiceDraft, domain.InvoiceOpen, domain.InvoicePaid}).
		Distinct("period_start").Count(&v.PeriodsUsed)
	return v
}
