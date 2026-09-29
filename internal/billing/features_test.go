package billing

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orshih6/billing/internal/domain"
	"github.com/orshih6/billing/internal/ledger"
	"github.com/orshih6/billing/internal/models"
)

func (h *harness) addonPlan(code string, amount int64) *models.Price {
	h.t.Helper()
	p, err := h.s.CreatePlan(h.ctx, h.tenant, PlanInput{Code: code, Name: code, Kind: models.PlanAddon,
		Prices: []PriceInput{{Amount: amount, Interval: "month"}}})
	h.ok(err)
	return &p.Prices[0]
}

func (h *harness) invoicesOf(subID uuid.UUID, kind domain.InvoiceKind) []models.Invoice {
	h.t.Helper()
	list, err := h.s.ListInvoices(h.ctx, h.tenant, InvoiceFilter{SubscriptionID: &subID}, Page{Limit: 100})
	h.ok(err)
	var out []models.Invoice
	for _, inv := range list.Data {
		if inv.Kind == kind && inv.Status != domain.InvoiceVoid {
			out = append(out, inv)
		}
	}
	return out
}

func (h *harness) accountBalance(code string) int64 {
	var a models.LedgerAccount
	h.s.db.Where("tenant_id = ? AND code = ?", h.tenant, code).Take(&a)
	return a.Balance
}

func TestSeats(t *testing.T) {
	h := newHarness(t)
	c := h.customer()
	price := h.plan("team", 10000, 0)
	v, err := h.s.CreateSubscription(h.ctx, h.tenant, CreateSubscriptionInput{CustomerID: c.ID, PriceID: &price.ID, Quantity: 3})
	h.ok(err)
	if v.LatestInvoice.Total != 30000 || v.LatestInvoice.Lines[0].Quantity != 3 {
		t.Fatalf("3 seats: %+v", v.LatestInvoice)
	}
	h.payMock(v.LatestInvoice.ID)
	v = h.sub(v.ID)

	// Halfway through: +2 seats now, half a period each.
	h.clock = v.CurrentPeriodStart.Add(v.CurrentPeriodEnd.Sub(v.CurrentPeriodStart) / 2)
	v, err = h.s.SetQuantity(h.ctx, h.tenant, v.ID, 5)
	h.ok(err)
	if v.Quantity != 5 {
		t.Fatalf("quantity %d", v.Quantity)
	}
	pr := h.invoicesOf(v.ID, domain.InvoiceProration)
	if len(pr) != 1 || pr[0].Total != 10000 {
		t.Fatalf("proration for 2 seats × half a period: %+v", pr)
	}

	// Down to 1: scheduled, still 5 until renewal.
	v, err = h.s.SetQuantity(h.ctx, h.tenant, v.ID, 1)
	h.ok(err)
	if v.Quantity != 5 || v.PendingQuantity == nil || *v.PendingQuantity != 1 {
		t.Fatalf("decrease must wait: %d pending %v", v.Quantity, v.PendingQuantity)
	}
	h.clock = v.CurrentPeriodEnd.AddDate(0, 0, -1)
	h.advance(0)
	renewal := h.renewalInvoice(v)
	if renewal.Total != 10000 {
		t.Fatalf("renewal must bill 1 seat, billed %d", renewal.Total)
	}
	h.payMock(renewal.ID)
	h.advance(2 * 24 * time.Hour)
	if v = h.sub(v.ID); v.Quantity != 1 || v.PendingQuantity != nil {
		t.Fatalf("after renewal: %d seats, pending %v", v.Quantity, v.PendingQuantity)
	}
	ents, _ := h.s.Entitlements(h.ctx, h.tenant, c.ID)
	if len(ents) != 1 || ents[0].Quantity != 1 {
		t.Fatalf("entitlement seats: %+v", ents)
	}
	h.reconciled()
}

func TestAddOns(t *testing.T) {
	h := newHarness(t)
	c := h.customer()
	base := h.plan("pro", 20000, 0)
	storage := h.addonPlan("storage", 5000)
	yearly, err := h.s.CreatePlan(h.ctx, h.tenant, PlanInput{Code: "support", Name: "Support", Kind: models.PlanAddon,
		Prices: []PriceInput{{Amount: 90000, Interval: "year"}}})
	h.ok(err)

	_, err = h.s.CreateSubscription(h.ctx, h.tenant, CreateSubscriptionInput{CustomerID: c.ID, PriceID: &base.ID,
		AddOns: []AddOnInput{{PriceID: yearly.Prices[0].ID}}})
	expectCode(t, err, "add_on_mismatch")
	_, err = h.s.CreateSubscription(h.ctx, h.tenant, CreateSubscriptionInput{CustomerID: c.ID, PriceID: &storage.ID})
	expectCode(t, err, "plan_is_addon")

	v, err := h.s.CreateSubscription(h.ctx, h.tenant, CreateSubscriptionInput{CustomerID: c.ID, PriceID: &base.ID,
		AddOns: []AddOnInput{{PriceID: storage.ID, Quantity: 2}}})
	h.ok(err)
	if v.LatestInvoice.Total != 30000 || len(v.LatestInvoice.Lines) != 2 {
		t.Fatalf("base + 2 × storage: %+v", v.LatestInvoice)
	}
	h.payMock(v.LatestInvoice.ID)
	ents, _ := h.s.Entitlements(h.ctx, h.tenant, c.ID)
	if len(ents) != 2 || ents[1].PlanCode != "storage" || ents[1].Kind != "addon" || ents[1].Quantity != 2 {
		t.Fatalf("entitlements with add-on: %+v", ents)
	}

	// Remove it: stays until the period ends, then gone and not billed.
	v, err = h.s.SetAddOn(h.ctx, h.tenant, v.ID, AddOnInput{PriceID: storage.ID, Quantity: 0})
	h.ok(err)
	if len(v.AddOns) != 1 || v.AddOns[0].PendingQuantity == nil || *v.AddOns[0].PendingQuantity != 0 {
		t.Fatalf("removal must be scheduled: %+v", v.AddOns)
	}
	h.clock = v.CurrentPeriodEnd.AddDate(0, 0, -2)
	h.advance(0)
	renewal := h.renewalInvoice(v)
	if renewal.Total != 20000 || len(renewal.Lines) != 1 {
		t.Fatalf("renewal without the add-on: %d, %d lines", renewal.Total, len(renewal.Lines))
	}
	h.payMock(renewal.ID)
	h.advance(3 * 24 * time.Hour)
	if v = h.sub(v.ID); len(v.AddOns) != 0 {
		t.Fatalf("add-on still active after renewal: %+v", v.AddOns)
	}
	ents, _ = h.s.Entitlements(h.ctx, h.tenant, c.ID)
	if len(ents) != 1 {
		t.Fatalf("add-on entitlement must end: %+v", ents)
	}
	h.reconciled()
}

// Adding something after the next period was already paid: the customer pays
// for the rest of this period and the difference for the paid next one.
func TestIncreaseAfterNextPeriodPaid(t *testing.T) {
	h := newHarness(t)
	c := h.customer()
	price := h.plan("team", 10000, 0)
	v, _ := h.s.CreateSubscription(h.ctx, h.tenant, CreateSubscriptionInput{CustomerID: c.ID, PriceID: &price.ID})
	h.payMock(v.LatestInvoice.ID)
	v = h.sub(v.ID)
	h.clock = v.CurrentPeriodEnd.AddDate(0, 0, -7)
	h.advance(0)
	h.payMock(h.renewalInvoice(v).ID)

	_, err := h.s.SetQuantity(h.ctx, h.tenant, v.ID, 2)
	h.ok(err)
	pr := h.invoicesOf(v.ID, domain.InvoiceProration)
	if len(pr) != 2 {
		t.Fatalf("want rest-of-period + next-period adjustments, got %d", len(pr))
	}
	var full int64
	for _, inv := range pr {
		if inv.Total == 10000 {
			full++
		}
	}
	if full != 1 {
		t.Fatalf("next period must be charged the full extra seat: %+v", pr)
	}
	// Decreasing now waits one more period, since the next is paid.
	v, err = h.s.SetQuantity(h.ctx, h.tenant, v.ID, 1)
	h.ok(err)
	h.advance(8 * 24 * time.Hour)
	if v = h.sub(v.ID); v.Quantity != 2 {
		t.Fatalf("a paid period keeps what was paid for: %d seats", v.Quantity)
	}
	h.reconciled()
}

func TestCouponsOnSubscriptions(t *testing.T) {
	h := newHarness(t)
	price := h.plan("pro", 10000, 0)
	two := 2
	_, err := h.s.CreateCoupon(h.ctx, h.tenant, CouponInput{Code: "launch20", PercentOff: ptr(20), Duration: "repeating", DurationPeriods: &two})
	h.ok(err)

	c := h.customer()
	v, err := h.s.CreateSubscription(h.ctx, h.tenant, CreateSubscriptionInput{CustomerID: c.ID, PriceID: &price.ID, PromoCode: " Launch20 "})
	h.ok(err)
	if v.LatestInvoice.DiscountTotal != 2000 || v.LatestInvoice.Total != 8000 || v.Discount == nil {
		t.Fatalf("20%% off first period: %+v", v.LatestInvoice)
	}
	h.payMock(v.LatestInvoice.ID)
	totals := []int64{}
	for range 3 {
		v = h.sub(v.ID)
		h.clock = v.CurrentPeriodEnd.AddDate(0, 0, -1)
		h.advance(0)
		inv := h.renewalInvoice(v)
		totals = append(totals, inv.Total)
		h.payMock(inv.ID)
		h.advance(2 * 24 * time.Hour)
	}
	if totals[0] != 8000 || totals[1] != 10000 || totals[2] != 10000 {
		t.Fatalf("repeating 2 periods: renewals billed %v", totals)
	}

	// Unknown, used-up and expired codes are refused.
	_, err = h.s.CreateSubscription(h.ctx, h.tenant, CreateSubscriptionInput{CustomerID: h.customer().ID, PriceID: &price.ID, PromoCode: "NOPE"})
	expectCode(t, err, "coupon_not_found")
	one := 1
	_, err = h.s.CreateCoupon(h.ctx, h.tenant, CouponInput{Code: "ONLYONE", AmountOff: ptr(int64(500)), Currency: "MNT", MaxRedemptions: &one})
	h.ok(err)
	_, err = h.s.CreateSubscription(h.ctx, h.tenant, CreateSubscriptionInput{CustomerID: h.customer().ID, PriceID: &price.ID, PromoCode: "ONLYONE"})
	h.ok(err)
	_, err = h.s.CreateSubscription(h.ctx, h.tenant, CreateSubscriptionInput{CustomerID: h.customer().ID, PriceID: &price.ID, PromoCode: "onlyone"})
	expectCode(t, err, "coupon_used_up")
	past := h.clock.Add(-time.Hour)
	_, err = h.s.CreateCoupon(h.ctx, h.tenant, CouponInput{Code: "OLD", PercentOff: ptr(10), RedeemBy: &past})
	h.ok(err)
	_, err = h.s.CreateSubscription(h.ctx, h.tenant, CreateSubscriptionInput{CustomerID: h.customer().ID, PriceID: &price.ID, PromoCode: "OLD"})
	expectCode(t, err, "coupon_expired")
	h.reconciled()
}

func TestCouponRestrictedToPlanAndOneOff(t *testing.T) {
	h := newHarness(t)
	base := h.plan("pro", 10000, 0)
	storage := h.addonPlan("storage", 4000)
	_, err := h.s.CreateCoupon(h.ctx, h.tenant, CouponInput{Code: "STORAGEHALF", PercentOff: ptr(50), Duration: "forever",
		PlanIDs: []string{storage.PlanID.String()}})
	h.ok(err)
	v, err := h.s.CreateSubscription(h.ctx, h.tenant, CreateSubscriptionInput{CustomerID: h.customer().ID, PriceID: &base.ID,
		AddOns: []AddOnInput{{PriceID: storage.ID}}, PromoCode: "STORAGEHALF"})
	h.ok(err)
	if v.LatestInvoice.DiscountTotal != 2000 || v.LatestInvoice.Total != 12000 {
		t.Fatalf("only the add-on is discounted: %+v", v.LatestInvoice)
	}
	_, err = h.s.CreateSubscription(h.ctx, h.tenant, CreateSubscriptionInput{CustomerID: h.customer().ID, PriceID: &base.ID, PromoCode: "STORAGEHALF"})
	expectCode(t, err, "coupon_not_applicable")

	_, err = h.s.CreateCoupon(h.ctx, h.tenant, CouponInput{Code: "FLAT", AmountOff: ptr(int64(3000)), Currency: "MNT"})
	h.ok(err)
	c := h.customer()
	inv, err := h.s.CreateInvoice(h.ctx, h.tenant, InvoiceInput{CustomerID: c.ID, Finalize: true, PromoCode: "flat",
		Lines: []LineInput{{Description: "Setup", UnitAmount: 2000}}})
	h.ok(err)
	if inv.DiscountTotal != 2000 || inv.Total != 0 || inv.Status != domain.InvoicePaid {
		t.Fatalf("amount off is capped at the subtotal; a fully discounted invoice is paid: %+v", inv)
	}
	if h.accountBalance(ledger.CodeDiscounts) != 4000 {
		t.Fatalf("discounts account: %d", h.accountBalance(ledger.CodeDiscounts))
	}
	h.reconciled()
}

func TestTax(t *testing.T) {
	h := newHarness(t)
	set := func(inclusive bool) {
		tenant, _ := h.s.Tenant(h.ctx, h.tenant)
		st := tenant.Settings.Data()
		st.Tax = &models.TaxSettings{Enabled: true, Name: "VAT", RateBps: 1000, Inclusive: inclusive}
		_, err := h.s.UpdateTenant(h.ctx, h.tenant, UpdateTenantInput{Settings: &st})
		h.ok(err)
	}
	invoice := func(c *models.Customer, amount int64, promo string) *models.Invoice {
		inv, err := h.s.CreateInvoice(h.ctx, h.tenant, InvoiceInput{CustomerID: c.ID, Finalize: true, PromoCode: promo,
			Lines: []LineInput{{Description: "x", UnitAmount: amount}}})
		h.ok(err)
		return inv
	}

	set(false)
	c := h.customer()
	inv := invoice(c, 10000, "")
	if inv.TaxTotal != 1000 || inv.Total != 11000 || inv.TaxName != "VAT" {
		t.Fatalf("10%% exclusive: %+v", inv)
	}
	_, err := h.s.CreateCoupon(h.ctx, h.tenant, CouponInput{Code: "HALF", PercentOff: ptr(50), Duration: "forever"})
	h.ok(err)
	inv = invoice(c, 10000, "HALF")
	if inv.DiscountTotal != 5000 || inv.TaxTotal != 500 || inv.Total != 5500 {
		t.Fatalf("tax on the discounted amount: %+v", inv)
	}

	set(true)
	inv = invoice(c, 11000, "")
	if inv.TaxTotal != 1000 || inv.Total != 11000 {
		t.Fatalf("10%% inclusive: %+v", inv)
	}

	exempt := h.customer()
	_, err = h.s.UpdateCustomer(h.ctx, h.tenant, exempt.ID, CustomerInput{TaxExempt: ptr(true)})
	h.ok(err)
	if inv = invoice(exempt, 10000, ""); inv.TaxTotal != 0 || inv.Total != 10000 {
		t.Fatalf("exempt: %+v", inv)
	}
	other := h.customer()
	_, err = h.s.UpdateCustomer(h.ctx, h.tenant, other.ID, CustomerInput{TaxRateBps: ptr(2000)})
	h.ok(err)
	if inv = invoice(other, 12000, ""); inv.TaxTotal != 2000 || inv.Total != 12000 {
		t.Fatalf("override 20%% inclusive: %+v", inv)
	}

	// Tax collected sits in tax_payable; voiding an invoice gives it back.
	before := h.accountBalance(ledger.CodeTaxPayable)
	if before != 1000+500+1000+2000 {
		t.Fatalf("tax payable %d", before)
	}
	_, err = h.s.VoidInvoice(h.ctx, h.tenant, inv.ID, "")
	h.ok(err)
	if after := h.accountBalance(ledger.CodeTaxPayable); after != before-2000 {
		t.Fatalf("void must reverse its tax: %d → %d", before, after)
	}
	h.reconciled()
}
