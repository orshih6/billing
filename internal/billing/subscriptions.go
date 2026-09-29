package billing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	"github.com/orshih6/billing/internal/domain"
	"github.com/orshih6/billing/internal/events"
	"github.com/orshih6/billing/internal/models"
)

// SubscriptionView is a subscription plus what an integrator needs next.
type SubscriptionView struct {
	models.Subscription
	PlanCode string `json:"plan_code"`
	PlanName string `json:"plan_name"`
	// Entitled: should the customer have access right now?
	Entitled bool `json:"entitled"`
	// AddOns are the active add-on items.
	AddOns   []AddOnView   `json:"add_ons"`
	Discount *DiscountView `json:"discount,omitempty"`
	// LatestInvoice is the most recent invoice (for an incomplete
	// subscription, the one to pay to activate it).
	LatestInvoice *models.Invoice `json:"latest_invoice,omitempty"`
}

// AddOnView is an add-on item with its plan and price.
type AddOnView struct {
	models.SubscriptionItem
	PlanCode   string `json:"plan_code"`
	PlanName   string `json:"plan_name"`
	UnitAmount int64  `json:"unit_amount"`
	Currency   string `json:"currency"`
}

// AddOnInput attaches an add-on price.
type AddOnInput struct {
	PriceID  uuid.UUID `json:"price_id"`
	Quantity int       `json:"quantity"`
}

// CreateSubscriptionInput subscribes a customer.
type CreateSubscriptionInput struct {
	CustomerID uuid.UUID  `json:"customer_id"`
	PriceID    *uuid.UUID `json:"price_id"`
	// PlanCode picks the plan's newest active recurring price (in Currency, or
	// the tenant default) when PriceID is not given.
	PlanCode string `json:"plan_code"`
	Currency string `json:"currency"`
	// Quantity is the number of seats (default 1).
	Quantity int          `json:"quantity"`
	AddOns   []AddOnInput `json:"add_ons"`
	// PromoCode applies a coupon from the first invoice.
	PromoCode string `json:"promo_code"`
	// TrialDays overrides the price's trial (0 = no trial).
	TrialDays *int           `json:"trial_days"`
	Metadata  map[string]any `json:"metadata"`
}

// lineSpec is one thing a subscription bills for in a period.
type lineSpec struct {
	PriceID     *uuid.UUID
	PlanID      uuid.UUID
	ItemID      *uuid.UUID
	Quantity    int64
	UnitAmount  int64
	Currency    string
	Description string
}

func (l lineSpec) amount() int64 { return l.Quantity * l.UnitAmount }

func sumLines(ls []lineSpec) int64 {
	var n int64
	for _, l := range ls {
		n += l.amount()
	}
	return n
}

// CreateSubscription starts a subscription.
//
//   - With a trial: status trialing until the trial ends; the first invoice is
//     raised before then like any renewal.
//   - Without: status incomplete, and the first invoice is issued now. The
//     subscription activates when it is paid, and its first period starts at
//     that moment. Unpaid, it expires after incomplete_expiry_hours.
//   - A free price (amount 0), or credit/discount that covers the invoice,
//     activates immediately.
func (s *Service) CreateSubscription(ctx context.Context, tenantID uuid.UUID, in CreateSubscriptionInput) (*SubscriptionView, error) {
	var subID uuid.UUID
	err := s.tx(ctx, func(tx *gorm.DB) error {
		t, set, err := s.settings(tx, tenantID)
		if err != nil {
			return err
		}
		cust, err := lock[models.Customer](tx, tenantID, in.CustomerID, "customer")
		if err != nil {
			return err
		}
		if cust.ArchivedAt != nil {
			return conflict("customer_archived", "customer is archived")
		}
		price, err := s.resolvePrice(tx, t, in)
		if err != nil {
			return err
		}
		if !price.Active || !price.Interval.Recurring() {
			return invalid("price_not_subscribable", "price must be active and recurring")
		}
		var plan models.Plan
		if err := tx.Take(&plan, "id = ?", price.PlanID).Error; err != nil {
			return err
		}
		if !plan.Active {
			return invalid("plan_inactive", "plan %s is not active", plan.Code)
		}
		if plan.Kind == models.PlanAddon {
			return invalid("plan_is_addon", "plan %s is an add-on; subscribe to a base plan and add it with add_ons", plan.Code)
		}
		if err := s.ensureNoLiveSubscription(tx, cust.ID, plan.ID, uuid.Nil); err != nil {
			return err
		}
		qty := in.Quantity
		if qty == 0 {
			qty = 1
		}
		if qty < 1 {
			return invalid("invalid_quantity", "quantity must be at least 1")
		}

		trial := price.TrialDays
		if in.TrialDays != nil {
			trial = max(*in.TrialDays, 0)
		}
		now := s.now()
		sub := &models.Subscription{
			TenantID: tenantID, CustomerID: cust.ID, PlanID: plan.ID, PriceID: price.ID, Quantity: qty,
			Metadata: datatypes.JSONMap(in.Metadata),
		}
		if trial > 0 {
			end := now.AddDate(0, 0, trial)
			sub.Status = domain.SubTrialing
			sub.CurrentPeriodStart, sub.CurrentPeriodEnd = now, end
			sub.TrialEnd = &end
			sub.BillingAnchor, sub.Cycle = end, 0
		} else {
			sub.Status = domain.SubIncomplete
			sub.BillingAnchor, sub.Cycle = now, 1
			sub.CurrentPeriodStart = now
			sub.CurrentPeriodEnd = domain.NthPeriodEnd(now, price.Interval, price.IntervalCount, 1)
		}
		if err := tx.Create(sub).Error; err != nil {
			return err
		}
		subID = sub.ID
		seen := map[uuid.UUID]bool{}
		for _, a := range in.AddOns {
			ap, err := s.addonPrice(tx, sub, price, a.PriceID)
			if err != nil {
				return err
			}
			if seen[ap.PlanID] {
				return invalid("duplicate_add_on", "add-on plan listed twice; use quantity instead")
			}
			seen[ap.PlanID] = true
			q := a.Quantity
			if q == 0 {
				q = 1
			}
			if q < 1 {
				return invalid("invalid_quantity", "add-on quantity must be at least 1")
			}
			item := models.SubscriptionItem{TenantID: tenantID, SubscriptionID: sub.ID, PlanID: ap.PlanID, PriceID: ap.ID, Quantity: q}
			if err := tx.Create(&item).Error; err != nil {
				return err
			}
		}
		if in.PromoCode != "" {
			plans, err := s.subscriptionPlanIDs(tx, sub)
			if err != nil {
				return err
			}
			d, err := s.redeem(tx, tenantID, cust.ID, in.PromoCode, price.Currency, plans, &sub.ID, nil)
			if err != nil {
				return err
			}
			sub.DiscountID = &d.ID
			if err := tx.Model(sub).Update("discount_id", d.ID).Error; err != nil {
				return err
			}
		}
		if err := events.Emit(tx, tenantID, events.SubscriptionCreated, sub); err != nil {
			return err
		}
		if sub.Status == domain.SubTrialing {
			return nil
		}
		lines, err := s.currentLines(tx, sub)
		if err != nil {
			return err
		}
		due := now.Add(time.Duration(set.IncompleteExpiryHours) * time.Hour)
		_, err = s.newSubscriptionInvoice(tx, sub, lines, domain.InvoiceSubscriptionCreate, sub.CurrentPeriodStart, sub.CurrentPeriodEnd, due)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.Subscription(ctx, tenantID, subID)
}

func (s *Service) resolvePrice(tx *gorm.DB, t *models.Tenant, in CreateSubscriptionInput) (*models.Price, error) {
	if in.PriceID != nil {
		return take[models.Price](tx, t.ID, *in.PriceID, "price")
	}
	if in.PlanCode == "" {
		return nil, invalid("price_required", "give price_id or plan_code")
	}
	cur := in.Currency
	if cur == "" {
		cur = t.DefaultCurrency
	}
	var price models.Price
	err := tx.Joins("JOIN plans ON plans.id = prices.plan_id").
		Where("prices.tenant_id = ? AND plans.code = ? AND prices.currency = ? AND prices.active AND prices.interval <> ?",
			t.ID, in.PlanCode, cur, domain.IntervalOneTime).
		Order("prices.created_at DESC").Take(&price).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, notFound("price")
	}
	return &price, err
}

// addonPrice validates an add-on price against the subscription's base price:
// an add-on plan, active, and billed on exactly the same interval and
// currency, so everything renews together on one invoice.
func (s *Service) addonPrice(tx *gorm.DB, sub *models.Subscription, base *models.Price, priceID uuid.UUID) (*models.Price, error) {
	p, err := take[models.Price](tx, sub.TenantID, priceID, "price")
	if err != nil {
		return nil, err
	}
	var plan models.Plan
	if err := tx.Take(&plan, "id = ?", p.PlanID).Error; err != nil {
		return nil, err
	}
	if plan.Kind != models.PlanAddon {
		return nil, invalid("not_an_add_on", "plan %s is not an add-on plan", plan.Code)
	}
	if !p.Active || !plan.Active {
		return nil, invalid("add_on_inactive", "add-on %s is not active", plan.Code)
	}
	if p.Interval != base.Interval || p.IntervalCount != base.IntervalCount || p.Currency != base.Currency {
		return nil, invalid("add_on_mismatch", "add-on %s must bill every %d %s in %s like the base plan",
			plan.Code, base.IntervalCount, base.Interval, base.Currency)
	}
	return p, nil
}

func (s *Service) ensureNoLiveSubscription(tx *gorm.DB, customerID, planID, except uuid.UUID) error {
	var n int64
	if err := tx.Model(&models.Subscription{}).
		Where("customer_id = ? AND plan_id = ? AND id <> ? AND status IN ?", customerID, planID, except, domain.LiveSubscriptionStatuses).
		Count(&n).Error; err != nil {
		return err
	}
	if n > 0 {
		return conflict("already_subscribed", "customer already has a live subscription to this plan")
	}
	return nil
}

func (s *Service) activeItems(tx *gorm.DB, subID uuid.UUID) ([]models.SubscriptionItem, error) {
	var items []models.SubscriptionItem
	err := tx.Where("subscription_id = ? AND removed_at IS NULL", subID).Order("created_at").Find(&items).Error
	return items, err
}

func (s *Service) subscriptionPlanIDs(tx *gorm.DB, sub *models.Subscription) ([]uuid.UUID, error) {
	ids := []uuid.UUID{sub.PlanID}
	items, err := s.activeItems(tx, sub.ID)
	for _, it := range items {
		ids = append(ids, it.PlanID)
	}
	return ids, err
}

func (s *Service) priceLine(tx *gorm.DB, priceID uuid.UUID, qty int, itemID *uuid.UUID) (lineSpec, error) {
	var p models.Price
	var plan models.Plan
	if err := tx.Take(&p, "id = ?", priceID).Error; err != nil {
		return lineSpec{}, err
	}
	if err := tx.Take(&plan, "id = ?", p.PlanID).Error; err != nil {
		return lineSpec{}, err
	}
	desc := plan.Name
	if qty > 1 {
		desc = fmt.Sprintf("%s × %d", plan.Name, qty)
	}
	return lineSpec{PriceID: &p.ID, PlanID: plan.ID, ItemID: itemID, Quantity: int64(qty), UnitAmount: p.Amount,
		Currency: p.Currency, Description: desc}, nil
}

// currentLines is what the subscription consists of right now.
func (s *Service) currentLines(tx *gorm.DB, sub *models.Subscription) ([]lineSpec, error) {
	base, err := s.priceLine(tx, sub.PriceID, sub.Quantity, nil)
	if err != nil {
		return nil, err
	}
	out := []lineSpec{base}
	items, err := s.activeItems(tx, sub.ID)
	if err != nil {
		return nil, err
	}
	for i := range items {
		l, err := s.priceLine(tx, items[i].PriceID, items[i].Quantity, &items[i].ID)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, nil
}

// due reports whether a pending change dated from applies to a period
// starting at start.
func due(from *time.Time, start time.Time) bool { return from != nil && !start.Before(*from) }

// nextLines is what the period starting at the current period end will bill:
// the current state with every pending change that is due by then.
func (s *Service) nextLines(tx *gorm.DB, sub *models.Subscription) ([]lineSpec, error) {
	start := sub.CurrentPeriodEnd
	priceID, qty := sub.PriceID, sub.Quantity
	if due(sub.PendingFrom, start) {
		if sub.PendingPriceID != nil {
			priceID = *sub.PendingPriceID
		}
		if sub.PendingQuantity != nil {
			qty = *sub.PendingQuantity
		}
	}
	base, err := s.priceLine(tx, priceID, qty, nil)
	if err != nil {
		return nil, err
	}
	out := []lineSpec{base}
	items, err := s.activeItems(tx, sub.ID)
	if err != nil {
		return nil, err
	}
	for i := range items {
		q := items[i].Quantity
		if due(items[i].PendingFrom, start) && items[i].PendingQuantity != nil {
			q = *items[i].PendingQuantity
		}
		if q == 0 {
			continue
		}
		l, err := s.priceLine(tx, items[i].PriceID, q, &items[i].ID)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, nil
}

// onSubscriptionInvoicePaid reacts to a subscription's invoice becoming paid.
func (s *Service) onSubscriptionInvoicePaid(tx *gorm.DB, inv *models.Invoice) error {
	sub, err := lock[models.Subscription](tx, inv.TenantID, *inv.SubscriptionID, "subscription")
	if err != nil {
		return err
	}
	switch inv.Kind {
	case domain.InvoiceSubscriptionCreate:
		if sub.Status != domain.SubIncomplete {
			return nil
		}
		var price models.Price
		if err := tx.Take(&price, "id = ?", sub.PriceID).Error; err != nil {
			return err
		}
		// The first period starts when the money arrives, not when the
		// subscription was requested.
		now := s.now()
		end := domain.NthPeriodEnd(now, price.Interval, price.IntervalCount, 1)
		sub.Status = domain.SubActive
		sub.BillingAnchor, sub.Cycle = now, 1
		sub.CurrentPeriodStart, sub.CurrentPeriodEnd = now, end
		if err := tx.Save(sub).Error; err != nil {
			return err
		}
		if err := tx.Model(&models.Invoice{}).Where("id = ?", inv.ID).
			Updates(map[string]any{"period_start": now, "period_end": end}).Error; err != nil {
			return err
		}
		if err := tx.Model(&models.InvoiceLine{}).Where("invoice_id = ?", inv.ID).
			Updates(map[string]any{"period_start": now, "period_end": end}).Error; err != nil {
			return err
		}
		return events.Emit(tx, sub.TenantID, events.SubscriptionActivated, sub)
	case domain.InvoiceSubscriptionCycle:
		if sub.Status.Live() {
			return s.rollIfDue(tx, sub)
		}
	}
	return nil
}

// rollIfDue advances a subscription whose period has ended into the next
// period, provided the invoice for that next period is paid. Paying early
// changes nothing until the period actually ends; paying late (past_due)
// rolls immediately, and the new period still starts at the old period end —
// a late payer does not get free days. Pending changes due by the new period
// (downgrades, fewer seats, removed add-ons) take effect here.
func (s *Service) rollIfDue(tx *gorm.DB, sub *models.Subscription) error {
	now := s.now()
	for sub.Status.Live() && sub.Status != domain.SubIncomplete && !now.Before(sub.CurrentPeriodEnd) {
		inv, err := s.periodInvoice(tx, sub.ID, sub.CurrentPeriodEnd)
		if err != nil {
			return err
		}
		if inv == nil || inv.Status != domain.InvoicePaid || inv.PeriodEnd == nil {
			return nil
		}
		prev := sub.Status
		start := sub.CurrentPeriodEnd
		priceChanged := false
		if due(sub.PendingFrom, start) {
			if sub.PendingPriceID != nil && *sub.PendingPriceID != sub.PriceID {
				var price models.Price
				if err := tx.Take(&price, "id = ?", *sub.PendingPriceID).Error; err != nil {
					return err
				}
				sub.PriceID, sub.PlanID = price.ID, price.PlanID
				priceChanged = true
			}
			if sub.PendingQuantity != nil {
				sub.Quantity = *sub.PendingQuantity
			}
			sub.PendingPriceID, sub.PendingQuantity, sub.PendingFrom = nil, nil, nil
		}
		if priceChanged {
			// The interval may have changed: restart the anchor.
			sub.BillingAnchor, sub.Cycle = start, 1
		} else {
			sub.Cycle++
		}
		items, err := s.activeItems(tx, sub.ID)
		if err != nil {
			return err
		}
		for i := range items {
			it := &items[i]
			if !due(it.PendingFrom, start) || it.PendingQuantity == nil {
				continue
			}
			if *it.PendingQuantity == 0 {
				it.RemovedAt = &start
			} else {
				it.Quantity = *it.PendingQuantity
			}
			it.PendingQuantity, it.PendingFrom = nil, nil
			if err := tx.Save(it).Error; err != nil {
				return err
			}
		}
		sub.CurrentPeriodStart, sub.CurrentPeriodEnd = start, *inv.PeriodEnd
		sub.Status = domain.SubActive
		if err := tx.Save(sub).Error; err != nil {
			return err
		}
		ev := events.SubscriptionRenewed
		if prev == domain.SubTrialing {
			ev = events.SubscriptionActivated
		}
		if err := events.Emit(tx, sub.TenantID, ev, sub); err != nil {
			return err
		}
	}
	return nil
}

// nextPeriodEnd computes when the period after the current one ends.
func (s *Service) nextPeriodEnd(tx *gorm.DB, sub *models.Subscription) (time.Time, error) {
	start := sub.CurrentPeriodEnd
	if due(sub.PendingFrom, start) && sub.PendingPriceID != nil && *sub.PendingPriceID != sub.PriceID {
		var p models.Price
		if err := tx.Take(&p, "id = ?", *sub.PendingPriceID).Error; err != nil {
			return time.Time{}, err
		}
		return domain.AddInterval(start, p.Interval, p.IntervalCount), nil
	}
	var p models.Price
	if err := tx.Take(&p, "id = ?", sub.PriceID).Error; err != nil {
		return time.Time{}, err
	}
	return domain.NthPeriodEnd(sub.BillingAnchor, p.Interval, p.IntervalCount, sub.Cycle+1), nil
}

// ensureRenewalInvoice raises the invoice for the next period if it does not
// exist yet. It returns the invoice either way.
func (s *Service) ensureRenewalInvoice(tx *gorm.DB, sub *models.Subscription) (*models.Invoice, error) {
	if inv, err := s.periodInvoice(tx, sub.ID, sub.CurrentPeriodEnd); err != nil || inv != nil {
		return inv, err
	}
	end, err := s.nextPeriodEnd(tx, sub)
	if err != nil {
		return nil, err
	}
	lines, err := s.nextLines(tx, sub)
	if err != nil {
		return nil, err
	}
	start := sub.CurrentPeriodEnd
	return s.newSubscriptionInvoice(tx, sub, lines, domain.InvoiceSubscriptionCycle, start, end, start)
}

// ---- changes --------------------------------------------------------------------
//
// Every change to what a subscription contains follows two rules:
//
//   - More (upgrade, seats, add-ons): applies now. The difference for the rest of
//     the current period is invoiced, prorated by time. If the next period is
//     already paid, the full difference for it is invoiced too; if it is raised
//     but unpaid, it is voided and reissued at the new size.
//   - Less (downgrade, fewer seats, removing an add-on): applies from the first
//     period not yet paid for. Nothing is refunded.

// changeable loads a subscription that may be changed.
func (s *Service) changeable(tx *gorm.DB, tenantID, id uuid.UUID) (*models.Subscription, *models.Price, error) {
	sub, err := lock[models.Subscription](tx, tenantID, id, "subscription")
	if err != nil {
		return nil, nil, err
	}
	if sub.Status != domain.SubActive && sub.Status != domain.SubTrialing {
		return nil, nil, conflict("subscription_not_changeable", "only active or trialing subscriptions can change (status is %s)", sub.Status)
	}
	if sub.CancelAtPeriodEnd {
		return nil, nil, conflict("subscription_canceling", "resume the subscription before changing it")
	}
	price, err := take[models.Price](tx, tenantID, sub.PriceID, "price")
	return sub, price, err
}

// chargeIncrease bills an immediate increase of delta per period.
func (s *Service) chargeIncrease(tx *gorm.DB, sub *models.Subscription, delta int64, planID uuid.UUID, currency, label string) error {
	if delta <= 0 {
		return nil
	}
	if sub.Status == domain.SubActive {
		now := s.now()
		if part := domain.Prorate(delta, sub.CurrentPeriodStart, sub.CurrentPeriodEnd, now); part > 0 {
			if err := s.adjustmentInvoice(tx, sub, part, planID, currency, label+" (rest of this period)", now, sub.CurrentPeriodEnd); err != nil {
				return err
			}
		}
	}
	next, err := s.periodInvoice(tx, sub.ID, sub.CurrentPeriodEnd)
	if err != nil || next == nil {
		return err
	}
	if next.Status != domain.InvoicePaid {
		return s.voidTx(tx, sub.TenantID, next.ID, "subscription changed")
	}
	return s.adjustmentInvoice(tx, sub, delta, planID, currency, label+" (next period)", *next.PeriodStart, *next.PeriodEnd)
}

// pendingFrom is when a decrease takes effect: the start of the first period
// not yet paid for. An unpaid renewal already raised is voided so it is
// reissued with the change.
func (s *Service) pendingFrom(tx *gorm.DB, sub *models.Subscription) (time.Time, error) {
	next, err := s.periodInvoice(tx, sub.ID, sub.CurrentPeriodEnd)
	if err != nil {
		return time.Time{}, err
	}
	if next == nil {
		return sub.CurrentPeriodEnd, nil
	}
	if next.Status == domain.InvoicePaid {
		return *next.PeriodEnd, nil
	}
	return sub.CurrentPeriodEnd, s.voidTx(tx, sub.TenantID, next.ID, "subscription changed")
}

func (s *Service) adjustmentInvoice(tx *gorm.DB, sub *models.Subscription, amount int64, planID uuid.UUID, currency, label string, start, end time.Time) error {
	_, set, err := s.settings(tx, sub.TenantID)
	if err != nil {
		return err
	}
	now := s.now()
	discountID, err := s.subscriptionDiscount(tx, sub, domain.InvoiceProration, start)
	if err != nil {
		return err
	}
	inv := &models.Invoice{
		TenantID: sub.TenantID, CustomerID: sub.CustomerID, SubscriptionID: &sub.ID,
		Kind: domain.InvoiceProration, Status: domain.InvoiceDraft, Currency: currency,
		PeriodStart: &start, PeriodEnd: &end, DueAt: ptr(now.AddDate(0, 0, set.InvoiceDueDays)), DiscountID: discountID,
	}
	if err := tx.Omit("Lines").Create(inv).Error; err != nil {
		return err
	}
	line := models.InvoiceLine{
		InvoiceID: inv.ID, PlanID: &planID, Quantity: 1, UnitAmount: amount, Amount: amount,
		Description: label, PeriodStart: &start, PeriodEnd: &end,
	}
	if err := tx.Create(&line).Error; err != nil {
		return err
	}
	if err := events.Emit(tx, sub.TenantID, events.InvoiceCreated, inv); err != nil {
		return err
	}
	return s.finalizeTx(tx, sub.TenantID, inv.ID)
}

func (s *Service) emitSubUpdated(tx *gorm.DB, sub *models.Subscription) error {
	return events.Emit(tx, sub.TenantID, events.SubscriptionUpdated, sub)
}

// ChangePriceInput moves a subscription to another price.
type ChangePriceInput struct {
	PriceID uuid.UUID `json:"price_id"`
	// When is "now" (upgrade: switch immediately and invoice the prorated
	// difference), "period_end" (switch at renewal) or "" to choose: an
	// increase on the same interval is immediate, anything else waits.
	When string `json:"when"`
}

// ChangePrice upgrades or downgrades the base plan.
func (s *Service) ChangePrice(ctx context.Context, tenantID, id uuid.UUID, in ChangePriceInput) (*SubscriptionView, error) {
	err := s.tx(ctx, func(tx *gorm.DB) error {
		sub, cur, err := s.changeable(tx, tenantID, id)
		if err != nil {
			return err
		}
		next, err := take[models.Price](tx, tenantID, in.PriceID, "price")
		if err != nil {
			return err
		}
		if next.ID == cur.ID {
			return invalid("same_price", "subscription is already on that price")
		}
		if !next.Active || !next.Interval.Recurring() {
			return invalid("price_not_subscribable", "price must be active and recurring")
		}
		var nextPlan models.Plan
		if err := tx.Take(&nextPlan, "id = ?", next.PlanID).Error; err != nil {
			return err
		}
		if nextPlan.Kind == models.PlanAddon {
			return invalid("plan_is_addon", "an add-on plan cannot be the base plan")
		}
		if next.Currency != cur.Currency {
			return invalid("currency_mismatch", "cannot change currency (%s → %s); cancel and resubscribe", cur.Currency, next.Currency)
		}
		if next.PlanID != sub.PlanID {
			if err := s.ensureNoLiveSubscription(tx, sub.CustomerID, next.PlanID, sub.ID); err != nil {
				return err
			}
		}
		sameCadence := next.Interval == cur.Interval && next.IntervalCount == cur.IntervalCount
		if !sameCadence {
			if items, err := s.activeItems(tx, sub.ID); err != nil {
				return err
			} else if len(items) > 0 {
				return invalid("add_ons_block_interval_change", "remove add-ons before changing the billing interval")
			}
		}
		when := in.When
		if when == "" {
			when = "period_end"
			if sameCadence && next.Amount > cur.Amount {
				when = "now"
			}
		}
		switch when {
		case "now":
			if !sameCadence || next.Amount < cur.Amount {
				return invalid("change_not_immediate", "only an increase on the same billing interval can apply now; use period_end")
			}
			sub.PriceID, sub.PlanID = next.ID, next.PlanID
			if sub.PendingPriceID != nil {
				sub.PendingPriceID = nil
			}
			if err := tx.Save(sub).Error; err != nil {
				return err
			}
			delta := (next.Amount - cur.Amount) * int64(sub.Quantity)
			if err := s.chargeIncrease(tx, sub, delta, next.PlanID, next.Currency, "Upgrade to "+nextPlan.Name); err != nil {
				return err
			}
		case "period_end":
			from, err := s.pendingFrom(tx, sub)
			if err != nil {
				return err
			}
			sub.PendingPriceID, sub.PendingFrom = &next.ID, &from
			if err := tx.Save(sub).Error; err != nil {
				return err
			}
		default:
			return invalid("invalid_when", "when must be now or period_end")
		}
		return s.emitSubUpdated(tx, sub)
	})
	if err != nil {
		return nil, err
	}
	return s.Subscription(ctx, tenantID, id)
}

// SetQuantity changes the number of seats. More seats apply now (prorated);
// fewer apply from the next unpaid period.
func (s *Service) SetQuantity(ctx context.Context, tenantID, id uuid.UUID, qty int) (*SubscriptionView, error) {
	if qty < 1 {
		return nil, invalid("invalid_quantity", "quantity must be at least 1")
	}
	err := s.tx(ctx, func(tx *gorm.DB) error {
		sub, price, err := s.changeable(tx, tenantID, id)
		if err != nil {
			return err
		}
		switch {
		case qty > sub.Quantity:
			added := qty - sub.Quantity
			sub.Quantity, sub.PendingQuantity = qty, nil
			if sub.PendingPriceID == nil {
				sub.PendingFrom = nil
			}
			if err := tx.Save(sub).Error; err != nil {
				return err
			}
			var plan models.Plan
			tx.Take(&plan, "id = ?", sub.PlanID)
			label := fmt.Sprintf("%s: %d more seat(s)", plan.Name, added)
			if err := s.chargeIncrease(tx, sub, price.Amount*int64(added), sub.PlanID, price.Currency, label); err != nil {
				return err
			}
		case qty < sub.Quantity:
			from, err := s.pendingFrom(tx, sub)
			if err != nil {
				return err
			}
			sub.PendingQuantity, sub.PendingFrom = &qty, &from
			if err := tx.Save(sub).Error; err != nil {
				return err
			}
		default:
			if sub.PendingQuantity == nil {
				return nil
			}
			// Back to the current number: drop the scheduled decrease.
			sub.PendingQuantity = nil
			if sub.PendingPriceID == nil {
				sub.PendingFrom = nil
			}
			if err := tx.Save(sub).Error; err != nil {
				return err
			}
			if err := s.voidUnpaidPeriodInvoice(tx, sub, "subscription changed"); err != nil {
				return err
			}
		}
		return s.emitSubUpdated(tx, sub)
	})
	if err != nil {
		return nil, err
	}
	return s.Subscription(ctx, tenantID, id)
}

// SetAddOn adds an add-on or changes its quantity; quantity 0 removes it. Like
// seats: more applies now (prorated), less from the next unpaid period.
func (s *Service) SetAddOn(ctx context.Context, tenantID, id uuid.UUID, in AddOnInput) (*SubscriptionView, error) {
	if in.Quantity < 0 {
		return nil, invalid("invalid_quantity", "quantity cannot be negative")
	}
	err := s.tx(ctx, func(tx *gorm.DB) error {
		sub, base, err := s.changeable(tx, tenantID, id)
		if err != nil {
			return err
		}
		ap, err := s.addonPrice(tx, sub, base, in.PriceID)
		if err != nil {
			return err
		}
		var plan models.Plan
		tx.Take(&plan, "id = ?", ap.PlanID)
		var item models.SubscriptionItem
		err = tx.Where("subscription_id = ? AND plan_id = ? AND removed_at IS NULL", sub.ID, ap.PlanID).Take(&item).Error
		exists := err == nil
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if exists && item.PriceID != ap.ID {
			return conflict("add_on_price_differs", "add-on %s is on another price; remove it first", plan.Code)
		}
		cur := 0
		if exists {
			cur = item.Quantity
		}
		switch {
		case in.Quantity > cur:
			if exists {
				item.Quantity, item.PendingQuantity, item.PendingFrom = in.Quantity, nil, nil
				if err := tx.Save(&item).Error; err != nil {
					return err
				}
			} else {
				item = models.SubscriptionItem{TenantID: tenantID, SubscriptionID: sub.ID, PlanID: ap.PlanID, PriceID: ap.ID, Quantity: in.Quantity}
				if err := tx.Create(&item).Error; err != nil {
					return err
				}
			}
			label := fmt.Sprintf("%s × %d", plan.Name, in.Quantity-cur)
			if err := s.chargeIncrease(tx, sub, ap.Amount*int64(in.Quantity-cur), ap.PlanID, ap.Currency, label); err != nil {
				return err
			}
		case in.Quantity < cur:
			from, err := s.pendingFrom(tx, sub)
			if err != nil {
				return err
			}
			q := in.Quantity
			item.PendingQuantity, item.PendingFrom = &q, &from
			if err := tx.Save(&item).Error; err != nil {
				return err
			}
		default:
			if !exists {
				return invalid("invalid_quantity", "quantity must be at least 1 to add an add-on")
			}
			if item.PendingQuantity == nil {
				return nil
			}
			item.PendingQuantity, item.PendingFrom = nil, nil
			if err := tx.Save(&item).Error; err != nil {
				return err
			}
			if err := s.voidUnpaidPeriodInvoice(tx, sub, "subscription changed"); err != nil {
				return err
			}
		}
		return s.emitSubUpdated(tx, sub)
	})
	if err != nil {
		return nil, err
	}
	return s.Subscription(ctx, tenantID, id)
}

// CancelSubscription ends a subscription, now or at the end of the period.
// Nothing is refunded automatically; refund payments explicitly if the
// tenant's policy says so.
func (s *Service) CancelSubscription(ctx context.Context, tenantID, id uuid.UUID, atPeriodEnd bool) (*SubscriptionView, error) {
	err := s.tx(ctx, func(tx *gorm.DB) error {
		sub, err := lock[models.Subscription](tx, tenantID, id, "subscription")
		if err != nil {
			return err
		}
		if !sub.Status.Live() {
			return conflict("subscription_ended", "subscription is already %s", sub.Status)
		}
		now := s.now()
		if atPeriodEnd && sub.Status != domain.SubIncomplete && sub.Status != domain.SubPastDue {
			sub.CancelAtPeriodEnd = true
			sub.CanceledAt = &now
			if err := tx.Save(sub).Error; err != nil {
				return err
			}
			// A renewal raised in advance is no longer wanted.
			if err := s.voidUnpaidPeriodInvoice(tx, sub, "subscription set to cancel at period end"); err != nil {
				return err
			}
			return events.Emit(tx, tenantID, events.SubscriptionUpdated, sub)
		}
		sub.Status = domain.SubCanceled
		sub.CanceledAt, sub.EndedAt = &now, &now
		if err := tx.Save(sub).Error; err != nil {
			return err
		}
		if err := s.voidOpenSubscriptionInvoices(tx, sub, "subscription canceled"); err != nil {
			return err
		}
		return events.Emit(tx, tenantID, events.SubscriptionCanceled, sub)
	})
	if err != nil {
		return nil, err
	}
	return s.Subscription(ctx, tenantID, id)
}

// ResumeSubscription undoes a pending cancel-at-period-end.
func (s *Service) ResumeSubscription(ctx context.Context, tenantID, id uuid.UUID) (*SubscriptionView, error) {
	err := s.tx(ctx, func(tx *gorm.DB) error {
		sub, err := lock[models.Subscription](tx, tenantID, id, "subscription")
		if err != nil {
			return err
		}
		if !sub.Status.Live() || !sub.CancelAtPeriodEnd {
			return conflict("not_canceling", "subscription is not scheduled to cancel")
		}
		sub.CancelAtPeriodEnd = false
		sub.CanceledAt = nil
		if err := tx.Save(sub).Error; err != nil {
			return err
		}
		return events.Emit(tx, tenantID, events.SubscriptionUpdated, sub)
	})
	if err != nil {
		return nil, err
	}
	return s.Subscription(ctx, tenantID, id)
}

// voidUnpaidPeriodInvoice voids the next period's invoice if it is not paid.
// A paid one is left alone: the customer bought that period.
func (s *Service) voidUnpaidPeriodInvoice(tx *gorm.DB, sub *models.Subscription, reason string) error {
	inv, err := s.periodInvoice(tx, sub.ID, sub.CurrentPeriodEnd)
	if err != nil || inv == nil || inv.Status == domain.InvoicePaid {
		return err
	}
	return s.voidTx(tx, sub.TenantID, inv.ID, reason)
}

func (s *Service) voidOpenSubscriptionInvoices(tx *gorm.DB, sub *models.Subscription, reason string) error {
	var ids []uuid.UUID
	if err := tx.Model(&models.Invoice{}).
		Where("subscription_id = ? AND status IN ? AND kind IN ?", sub.ID,
			[]domain.InvoiceStatus{domain.InvoiceDraft, domain.InvoiceOpen},
			[]domain.InvoiceKind{domain.InvoiceSubscriptionCreate, domain.InvoiceSubscriptionCycle}).
		Pluck("id", &ids).Error; err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.voidTx(tx, sub.TenantID, id, reason); err != nil {
			return err
		}
	}
	return nil
}

// expireIncompleteTx expires a subscription whose first invoice was never
// paid, optionally voiding that invoice.
func (s *Service) expireIncompleteTx(tx *gorm.DB, tenantID, id uuid.UUID, voidInvoice bool) error {
	sub, err := lock[models.Subscription](tx, tenantID, id, "subscription")
	if err != nil {
		return err
	}
	if sub.Status != domain.SubIncomplete {
		return nil
	}
	now := s.now()
	sub.Status = domain.SubExpired
	sub.EndedAt = &now
	if err := tx.Save(sub).Error; err != nil {
		return err
	}
	if err := events.Emit(tx, tenantID, events.SubscriptionExpired, sub); err != nil {
		return err
	}
	if voidInvoice {
		return s.voidOpenSubscriptionInvoices(tx, sub, "subscription was never paid")
	}
	return nil
}

// Subscription loads one subscription with its add-ons, discount and latest
// invoice.
func (s *Service) Subscription(ctx context.Context, tenantID, id uuid.UUID) (*SubscriptionView, error) {
	sub, err := take[models.Subscription](s.db.WithContext(ctx), tenantID, id, "subscription")
	if err != nil {
		return nil, err
	}
	v := s.view(ctx, *sub)
	var inv models.Invoice
	if err := s.db.WithContext(ctx).Preload("Lines").Where("subscription_id = ?", sub.ID).
		Order("created_at DESC").Take(&inv).Error; err == nil {
		v.LatestInvoice = &inv
	}
	return &v, nil
}

func (s *Service) view(ctx context.Context, sub models.Subscription) SubscriptionView {
	db := s.db.WithContext(ctx)
	v := SubscriptionView{Subscription: sub, Entitled: sub.Status.Entitled(), AddOns: []AddOnView{}}
	var plan models.Plan
	if db.Select("code, name").Take(&plan, "id = ?", sub.PlanID).Error == nil {
		v.PlanCode, v.PlanName = plan.Code, plan.Name
	}
	items, _ := s.activeItems(db, sub.ID)
	for _, it := range items {
		av := AddOnView{SubscriptionItem: it}
		var plan models.Plan
		var price models.Price
		if db.Take(&plan, "id = ?", it.PlanID).Error == nil {
			av.PlanCode, av.PlanName = plan.Code, plan.Name
		}
		if db.Take(&price, "id = ?", it.PriceID).Error == nil {
			av.UnitAmount, av.Currency = price.Amount, price.Currency
		}
		v.AddOns = append(v.AddOns, av)
	}
	if sub.DiscountID != nil {
		v.Discount = s.discountView(db, *sub.DiscountID)
	}
	return v
}

// SubscriptionFilter narrows ListSubscriptions.
type SubscriptionFilter struct {
	CustomerID *uuid.UUID
	PlanID     *uuid.UUID
	PlanCode   string
	Status     string
	Common
}

// ListSubscriptions lists subscriptions, newest first.
func (s *Service) ListSubscriptions(ctx context.Context, tenantID uuid.UUID, f SubscriptionFilter, p Page) (List[SubscriptionView], error) {
	q := s.db.WithContext(ctx).Model(&models.Subscription{}).Where("tenant_id = ?", tenantID)
	if f.CustomerID != nil {
		q = q.Where("customer_id = ?", *f.CustomerID)
	}
	if f.PlanID != nil {
		q = q.Where("plan_id = ?", *f.PlanID)
	}
	if f.Status != "" {
		q = q.Where("status = ?", f.Status)
	}
	if f.PlanCode != "" {
		q = q.Where("plan_id IN (SELECT id FROM plans WHERE tenant_id = ? AND code = ?)", tenantID, f.PlanCode)
	}
	q, err := f.Common.apply(q, "subscriptions", true)
	if err != nil {
		return List[SubscriptionView]{}, err
	}
	page, err := paginate[models.Subscription](q, "subscriptions", p)
	if err != nil {
		return List[SubscriptionView]{}, err
	}
	out := List[SubscriptionView]{HasMore: page.HasMore, Data: make([]SubscriptionView, len(page.Data))}
	for i, sub := range page.Data {
		out.Data[i] = s.view(ctx, sub)
	}
	return out, nil
}
