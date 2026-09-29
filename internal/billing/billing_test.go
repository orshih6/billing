package billing

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orshih6/billing/internal/domain"
	"github.com/orshih6/billing/internal/models"
	"github.com/orshih6/billing/internal/providers"
	"github.com/orshih6/billing/internal/secretbox"
	"github.com/orshih6/billing/internal/testsupport"
)

func TestMain(m *testing.M) { os.Exit(testsupport.Run(m)) }

type harness struct {
	t      *testing.T
	s      *Service
	ctx    context.Context
	clock  time.Time
	tenant uuid.UUID
}

func newHarness(t *testing.T) *harness {
	db := testsupport.DB(t)
	h := &harness{t: t, ctx: context.Background(), clock: time.Date(2026, 1, 31, 12, 0, 0, 0, time.UTC)}
	h.s = New(db, providers.NewRegistry(providers.Mock{PublicURL: "http://test"}, providers.Manual{}), testsupport.Logger(), false)
	h.s.Now = func() time.Time { return h.clock }
	h.s.Secrets, _ = secretbox.New("test-secrets-key-0123456789abcdef-xyz")
	grace := 3
	out, err := h.s.CreateTenant(h.ctx, CreateTenantInput{Slug: "acme", Name: "Acme", Settings: &models.TenantSettings{GraceDays: &grace}})
	h.ok(err)
	h.tenant = out.Tenant.ID
	return h
}

func (h *harness) ok(err error) {
	h.t.Helper()
	if err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) advance(d time.Duration) EngineReport {
	h.t.Helper()
	h.clock = h.clock.Add(d)
	r, err := h.s.RunEngine(h.ctx)
	h.ok(err)
	if r.Errors > 0 {
		h.t.Fatalf("engine reported %d errors", r.Errors)
	}
	return r
}

func (h *harness) customer() *models.Customer {
	h.t.Helper()
	c, err := h.s.CreateCustomer(h.ctx, h.tenant, CustomerInput{Name: ptr("Bat"), ExternalID: ptr(uuid.NewString())})
	h.ok(err)
	return c
}

func (h *harness) plan(code string, amount int64, trial int) *models.Price {
	h.t.Helper()
	p, err := h.s.CreatePlan(h.ctx, h.tenant, PlanInput{Code: code, Name: code, Prices: []PriceInput{
		{Amount: amount, Interval: "month", TrialDays: trial},
	}})
	h.ok(err)
	return &p.Prices[0]
}

func (h *harness) sub(id uuid.UUID) *SubscriptionView {
	h.t.Helper()
	v, err := h.s.Subscription(h.ctx, h.tenant, id)
	h.ok(err)
	return v
}

func (h *harness) payMock(invoiceID uuid.UUID) *models.Payment {
	h.t.Helper()
	p, err := h.s.StartPayment(h.ctx, h.tenant, invoiceID, StartPaymentInput{Provider: "mock"}, nil)
	h.ok(err)
	p, err = h.s.MockOutcome(h.ctx, h.tenant, p.ID, "succeed")
	h.ok(err)
	return p
}

func (h *harness) reconciled() {
	h.t.Helper()
	r, err := h.s.Reconcile(h.ctx, h.tenant)
	h.ok(err)
	if !r.OK {
		h.t.Fatalf("books do not reconcile: %+v", r)
	}
}

func (h *harness) balance(customerID uuid.UUID) Balance {
	h.t.Helper()
	bs, err := h.s.CustomerBalances(h.ctx, h.tenant, customerID)
	h.ok(err)
	for _, b := range bs {
		if b.Currency == "MNT" {
			return b
		}
	}
	return Balance{Currency: "MNT"}
}

func (h *harness) renewalInvoice(sub *SubscriptionView) *models.Invoice {
	h.t.Helper()
	inv, err := h.s.periodInvoice(h.s.db, sub.ID, sub.CurrentPeriodEnd)
	h.ok(err)
	if inv == nil {
		h.t.Fatal("no renewal invoice for the next period")
	}
	return inv
}

func expectCode(t *testing.T, err error, code string) {
	t.Helper()
	e, ok := AsError(err)
	if !ok || e.Code != code {
		t.Fatalf("got %v, want error code %s", err, code)
	}
}

// Subscribe → pay → renew on schedule, with the month anchored on the 31st.
func TestSubscriptionLifecycle(t *testing.T) {
	h := newHarness(t)
	c := h.customer()
	price := h.plan("pro", 50000, 0)

	v, err := h.s.CreateSubscription(h.ctx, h.tenant, CreateSubscriptionInput{CustomerID: c.ID, PriceID: &price.ID})
	h.ok(err)
	if v.Status != domain.SubIncomplete || v.Entitled || v.LatestInvoice == nil || v.LatestInvoice.Status != domain.InvoiceOpen {
		t.Fatalf("new subscription: %+v", v)
	}
	if b := h.balance(c.ID); b.Owed != 50000 {
		t.Fatalf("owed %d, want 50000", b.Owed)
	}

	h.clock = h.clock.Add(time.Hour) // pays an hour later
	h.payMock(v.LatestInvoice.ID)
	v = h.sub(v.ID)
	if v.Status != domain.SubActive || !v.Entitled {
		t.Fatalf("after payment: %s", v.Status)
	}
	if !v.CurrentPeriodStart.Equal(h.clock) {
		t.Fatalf("period must start at payment time: %v vs %v", v.CurrentPeriodStart, h.clock)
	}
	// 31 Jan 13:00 + 1 month = 28 Feb 13:00
	if want := time.Date(2026, 2, 28, 13, 0, 0, 0, time.UTC); !v.CurrentPeriodEnd.Equal(want) {
		t.Fatalf("period end %v, want %v", v.CurrentPeriodEnd, want)
	}
	if b := h.balance(c.ID); b.Owed != 0 {
		t.Fatalf("owed %d after paying", b.Owed)
	}

	// Renewal invoice appears 7 days before the end, not earlier.
	if r := h.advance(20 * 24 * time.Hour); r.RenewalInvoices != 0 {
		t.Fatalf("renewal raised too early: %+v", r)
	}
	if r := h.advance(2 * 24 * time.Hour); r.RenewalInvoices != 1 {
		t.Fatalf("renewal not raised: %+v", r)
	}
	if r := h.advance(time.Hour); r.RenewalInvoices != 0 {
		t.Fatalf("renewal raised twice: %+v", r)
	}
	inv := h.renewalInvoice(v)
	h.payMock(inv.ID)
	if v2 := h.sub(v.ID); v2.Cycle != 1 {
		t.Fatal("paying early must not roll the period before it ends")
	}

	h.advance(7 * 24 * time.Hour) // past 28 Feb
	v = h.sub(v.ID)
	if v.Status != domain.SubActive || v.Cycle != 2 {
		t.Fatalf("after renewal: status %s cycle %d", v.Status, v.Cycle)
	}
	// Anchored on the 31st: second period ends 31 March, not 28 March.
	if want := time.Date(2026, 3, 31, 13, 0, 0, 0, time.UTC); !v.CurrentPeriodEnd.Equal(want) {
		t.Fatalf("second period end %v, want %v", v.CurrentPeriodEnd, want)
	}
	h.reconciled()
}

// Unpaid renewal → past_due (still entitled) → expired after grace; the
// unpaid invoice is voided and access ends.
func TestLapseAfterGrace(t *testing.T) {
	h := newHarness(t)
	c := h.customer()
	price := h.plan("pro", 10000, 0)
	v, _ := h.s.CreateSubscription(h.ctx, h.tenant, CreateSubscriptionInput{CustomerID: c.ID, PriceID: &price.ID})
	h.payMock(v.LatestInvoice.ID)
	v = h.sub(v.ID)

	h.clock = v.CurrentPeriodEnd.Add(-time.Minute)
	h.advance(0)
	inv := h.renewalInvoice(v)

	if r := h.advance(2 * time.Minute); r.PastDue != 1 {
		t.Fatalf("expected past_due: %+v", r)
	}
	v = h.sub(v.ID)
	if v.Status != domain.SubPastDue || !v.Entitled {
		t.Fatalf("past_due must keep access during grace: %s %v", v.Status, v.Entitled)
	}
	if r := h.advance(2 * 24 * time.Hour); r.Lapsed != 0 {
		t.Fatal("lapsed inside grace")
	}
	if r := h.advance(24 * time.Hour); r.Lapsed != 1 {
		t.Fatalf("expected lapse after 3 grace days: %+v", r)
	}
	v = h.sub(v.ID)
	if v.Status != domain.SubExpired || v.Entitled {
		t.Fatalf("after lapse: %s", v.Status)
	}
	got, _ := h.s.Invoice(h.ctx, h.tenant, inv.ID)
	if got.Status != domain.InvoiceVoid {
		t.Fatalf("unpaid renewal invoice is %s, want void", got.Status)
	}
	ents, _ := h.s.Entitlements(h.ctx, h.tenant, c.ID)
	if len(ents) != 0 {
		t.Fatalf("entitlements after lapse: %+v", ents)
	}
	if b := h.balance(c.ID); b.Owed != 0 {
		t.Fatalf("owed %d after void", b.Owed)
	}
	h.reconciled()
}

// Paying during grace renews from the OLD period end: no free days.
func TestLatePaymentKeepsAnchor(t *testing.T) {
	h := newHarness(t)
	c := h.customer()
	price := h.plan("pro", 10000, 0)
	v, _ := h.s.CreateSubscription(h.ctx, h.tenant, CreateSubscriptionInput{CustomerID: c.ID, PriceID: &price.ID})
	h.payMock(v.LatestInvoice.ID)
	v = h.sub(v.ID)
	oldEnd := v.CurrentPeriodEnd

	h.clock = oldEnd.Add(time.Hour)
	h.advance(0)
	if h.sub(v.ID).Status != domain.SubPastDue {
		t.Fatal("expected past_due")
	}
	h.clock = h.clock.Add(48 * time.Hour)
	h.payMock(h.renewalInvoice(v).ID)
	v = h.sub(v.ID)
	if v.Status != domain.SubActive || !v.CurrentPeriodStart.Equal(oldEnd) {
		t.Fatalf("late payment: status %s start %v, want start %v", v.Status, v.CurrentPeriodStart, oldEnd)
	}
	h.reconciled()
}

func TestTrialConvertsOrLapses(t *testing.T) {
	h := newHarness(t)
	c := h.customer()
	price := h.plan("trial-plan", 20000, 14)
	v, err := h.s.CreateSubscription(h.ctx, h.tenant, CreateSubscriptionInput{CustomerID: c.ID, PriceID: &price.ID})
	h.ok(err)
	if v.Status != domain.SubTrialing || !v.Entitled || v.LatestInvoice != nil {
		t.Fatalf("trial start: %+v", v)
	}
	h.advance(8 * 24 * time.Hour)
	inv := h.renewalInvoice(v)
	if inv.Kind != domain.InvoiceSubscriptionCycle || inv.Total != 20000 {
		t.Fatalf("first invoice after trial: %+v", inv)
	}
	h.payMock(inv.ID)
	h.advance(7 * 24 * time.Hour)
	v = h.sub(v.ID)
	if v.Status != domain.SubActive || v.Cycle != 1 || !v.CurrentPeriodStart.Equal(*v.TrialEnd) {
		t.Fatalf("after trial: %s cycle %d start %v", v.Status, v.Cycle, v.CurrentPeriodStart)
	}
	h.reconciled()
}

// Overpay → credit → credit pays the next invoice automatically; a refund of
// the overpaying payment takes back only the unused credit.
func TestOverpaymentBecomesCredit(t *testing.T) {
	h := newHarness(t)
	c := h.customer()
	price := h.plan("pro", 10000, 0)
	v, _ := h.s.CreateSubscription(h.ctx, h.tenant, CreateSubscriptionInput{CustomerID: c.ID, PriceID: &price.ID})

	pay, err := h.s.RecordManualPayment(h.ctx, h.tenant, ManualPaymentInput{InvoiceID: v.LatestInvoice.ID, Amount: 25000, Reference: "STMT-1"}, nil)
	h.ok(err)
	if pay.Status != domain.PaymentSucceeded {
		t.Fatalf("manual payment %s", pay.Status)
	}
	if b := h.balance(c.ID); b.Credit != 15000 || b.Owed != 0 {
		t.Fatalf("balance after overpaying: %+v", b)
	}
	if h.sub(v.ID).Status != domain.SubActive {
		t.Fatal("subscription not activated")
	}

	// Same bank reference again is refused.
	_, err = h.s.RecordManualPayment(h.ctx, h.tenant, ManualPaymentInput{InvoiceID: v.LatestInvoice.ID, Amount: 1, Reference: "STMT-1"}, nil)
	expectCode(t, err, "reference_already_recorded")

	// Renewal is paid from credit the moment it is raised.
	v = h.sub(v.ID)
	h.clock = v.CurrentPeriodEnd.AddDate(0, 0, -7)
	h.advance(0)
	inv := h.renewalInvoice(v)
	if inv.Status != domain.InvoicePaid || inv.CreditApplied != 10000 {
		t.Fatalf("renewal from credit: %s applied %d", inv.Status, inv.CreditApplied)
	}
	if b := h.balance(c.ID); b.Credit != 5000 {
		t.Fatalf("credit left %d, want 5000", b.Credit)
	}

	refunded, err := h.s.RefundPayment(h.ctx, h.tenant, pay.ID, "customer asked")
	h.ok(err)
	if refunded.Status != domain.PaymentRefunded {
		t.Fatal("not refunded")
	}
	if b := h.balance(c.ID); b.Credit != 0 {
		t.Fatalf("refund must reclaim the unused credit, left %d", b.Credit)
	}
	h.reconciled()
}

func TestPartialPaymentsAndVoidReturnsMoney(t *testing.T) {
	h := newHarness(t)
	c := h.customer()
	inv, err := h.s.CreateInvoice(h.ctx, h.tenant, InvoiceInput{CustomerID: c.ID, Finalize: true, Lines: []LineInput{
		{Description: "Setup", UnitAmount: 3000, Quantity: 2}, {Description: "Consulting", UnitAmount: 4000},
	}})
	h.ok(err)
	if inv.Total != 10000 || inv.Number == nil || inv.Reference == nil || inv.Status != domain.InvoiceOpen {
		t.Fatalf("finalized invoice: %+v", inv)
	}
	_, err = h.s.RecordManualPayment(h.ctx, h.tenant, ManualPaymentInput{InvoiceID: inv.ID, Amount: 4000}, nil)
	h.ok(err)
	inv, _ = h.s.Invoice(h.ctx, h.tenant, inv.ID)
	if inv.Status != domain.InvoiceOpen || inv.AmountDue != 6000 || inv.AmountPaid != 4000 {
		t.Fatalf("after partial: %+v", inv)
	}
	inv, err = h.s.VoidInvoice(h.ctx, h.tenant, inv.ID, "wrong customer")
	h.ok(err)
	if inv.Status != domain.InvoiceVoid {
		t.Fatal("not void")
	}
	if b := h.balance(c.ID); b.Credit != 4000 || b.Owed != 0 {
		t.Fatalf("voiding must return the paid 4000 as credit: %+v", b)
	}
	_, err = h.s.VoidInvoice(h.ctx, h.tenant, inv.ID, "")
	expectCode(t, err, "invoice_not_voidable")
	h.reconciled()
}

func TestCancelAtPeriodEnd(t *testing.T) {
	h := newHarness(t)
	c := h.customer()
	price := h.plan("pro", 10000, 0)
	v, _ := h.s.CreateSubscription(h.ctx, h.tenant, CreateSubscriptionInput{CustomerID: c.ID, PriceID: &price.ID})
	h.payMock(v.LatestInvoice.ID)
	v = h.sub(v.ID)
	h.clock = v.CurrentPeriodEnd.AddDate(0, 0, -3)
	h.advance(0)
	renewal := h.renewalInvoice(v)

	v, err := h.s.CancelSubscription(h.ctx, h.tenant, v.ID, true)
	h.ok(err)
	if !v.CancelAtPeriodEnd || v.Status != domain.SubActive {
		t.Fatalf("cancel at period end: %+v", v)
	}
	if got, _ := h.s.Invoice(h.ctx, h.tenant, renewal.ID); got.Status != domain.InvoiceVoid {
		t.Fatal("pending renewal must be voided on cancel")
	}
	if r := h.advance(4 * 24 * time.Hour); r.Canceled != 1 || r.RenewalInvoices != 0 {
		t.Fatalf("at period end: %+v", r)
	}
	if v = h.sub(v.ID); v.Status != domain.SubCanceled || v.Entitled {
		t.Fatalf("after period end: %s", v.Status)
	}
	h.reconciled()
}

func TestUpgradeNowAndDowngradeLater(t *testing.T) {
	h := newHarness(t)
	c := h.customer()
	basic := h.plan("basic", 30000, 0)
	proPlan, err := h.s.CreatePlan(h.ctx, h.tenant, PlanInput{Code: "pro", Name: "Pro", Prices: []PriceInput{{Amount: 60000, Interval: "month"}}})
	h.ok(err)
	pro := proPlan.Prices[0]

	v, _ := h.s.CreateSubscription(h.ctx, h.tenant, CreateSubscriptionInput{CustomerID: c.ID, PriceID: &basic.ID})
	h.payMock(v.LatestInvoice.ID)
	v = h.sub(v.ID)
	// Halfway through the 28-day February period.
	h.clock = v.CurrentPeriodStart.Add(v.CurrentPeriodEnd.Sub(v.CurrentPeriodStart) / 2)
	v, err = h.s.ChangePrice(h.ctx, h.tenant, v.ID, ChangePriceInput{PriceID: pro.ID})
	h.ok(err)
	if v.PriceID != pro.ID || v.PlanCode != "pro" {
		t.Fatalf("upgrade not immediate: %+v", v)
	}
	if v.LatestInvoice == nil || v.LatestInvoice.Kind != domain.InvoiceProration || v.LatestInvoice.Total != 15000 {
		t.Fatalf("proration invoice: %+v", v.LatestInvoice)
	}

	// Downgrade waits for the period end, and the renewal bills the new price.
	v, err = h.s.ChangePrice(h.ctx, h.tenant, v.ID, ChangePriceInput{PriceID: basic.ID})
	h.ok(err)
	if v.PriceID != pro.ID || v.PendingPriceID == nil || *v.PendingPriceID != basic.ID {
		t.Fatalf("downgrade must be scheduled: %+v", v)
	}
	h.clock = v.CurrentPeriodEnd.AddDate(0, 0, -1)
	h.advance(0)
	renewal := h.renewalInvoice(v)
	if renewal.Total != 30000 {
		t.Fatalf("renewal after downgrade billed %d", renewal.Total)
	}
	h.payMock(renewal.ID)
	h.advance(2 * 24 * time.Hour)
	v = h.sub(v.ID)
	if v.PriceID != basic.ID || v.PendingPriceID != nil || v.PlanCode != "basic" {
		t.Fatalf("downgrade not applied at renewal: %+v", v)
	}
	h.reconciled()
}

func TestIncompleteExpires(t *testing.T) {
	h := newHarness(t)
	c := h.customer()
	price := h.plan("pro", 10000, 0)
	v, _ := h.s.CreateSubscription(h.ctx, h.tenant, CreateSubscriptionInput{CustomerID: c.ID, PriceID: &price.ID})
	if r := h.advance(49 * time.Hour); r.IncompleteExpired != 1 {
		t.Fatalf("incomplete not expired: %+v", r)
	}
	if v2 := h.sub(v.ID); v2.Status != domain.SubExpired {
		t.Fatalf("status %s", v2.Status)
	}
	if inv, _ := h.s.Invoice(h.ctx, h.tenant, v.LatestInvoice.ID); inv.Status != domain.InvoiceVoid {
		t.Fatalf("first invoice %s, want void", inv.Status)
	}
	// Expired frees the slot: subscribing again works.
	_, err := h.s.CreateSubscription(h.ctx, h.tenant, CreateSubscriptionInput{CustomerID: c.ID, PlanCode: "pro"})
	h.ok(err)
	h.reconciled()
}

func TestOneLiveSubscriptionPerPlan(t *testing.T) {
	h := newHarness(t)
	c := h.customer()
	price := h.plan("pro", 0, 0)
	v, err := h.s.CreateSubscription(h.ctx, h.tenant, CreateSubscriptionInput{CustomerID: c.ID, PriceID: &price.ID})
	h.ok(err)
	if v.Status != domain.SubActive {
		t.Fatalf("free plan must activate immediately, got %s", v.Status)
	}
	_, err = h.s.CreateSubscription(h.ctx, h.tenant, CreateSubscriptionInput{CustomerID: c.ID, PriceID: &price.ID})
	expectCode(t, err, "already_subscribed")
}

func TestTenantIsolation(t *testing.T) {
	h := newHarness(t)
	c := h.customer()
	other, err := h.s.CreateTenant(h.ctx, CreateTenantInput{Slug: "other", Name: "Other"})
	h.ok(err)
	_, err = h.s.Customer(h.ctx, other.Tenant.ID, c.ID)
	expectCode(t, err, "customer_not_found")
	_, err = h.s.CreateSubscription(h.ctx, other.Tenant.ID, CreateSubscriptionInput{CustomerID: c.ID, PlanCode: "x"})
	expectCode(t, err, "customer_not_found")
}

func TestPaymentExpiryAndFailure(t *testing.T) {
	h := newHarness(t)
	c := h.customer()
	inv, _ := h.s.CreateInvoice(h.ctx, h.tenant, InvoiceInput{CustomerID: c.ID, Finalize: true, Lines: []LineInput{{Description: "x", UnitAmount: 500}}})
	p1, err := h.s.StartPayment(h.ctx, h.tenant, inv.ID, StartPaymentInput{Provider: "mock"}, nil)
	h.ok(err)
	p2, _ := h.s.StartPayment(h.ctx, h.tenant, inv.ID, StartPaymentInput{Provider: "mock"}, nil)
	if p1.ID != p2.ID {
		t.Fatal("a pending payment must be reused")
	}
	p1, _ = h.s.MockOutcome(h.ctx, h.tenant, p1.ID, "fail")
	if p1.Status != domain.PaymentFailed {
		t.Fatal("not failed")
	}
	p3, _ := h.s.StartPayment(h.ctx, h.tenant, inv.ID, StartPaymentInput{Provider: "manual"}, nil)
	if p3.Instructions == "" || p3.Status != domain.PaymentPending {
		t.Fatalf("manual intent: %+v", p3)
	}
	if r := h.advance(25 * time.Hour); r.PaymentsExpired != 1 {
		t.Fatalf("pending payment not expired: %+v", r)
	}
	// A transfer that arrives after its payment expired is still money: it is
	// recorded, not refused.
	late, err := h.s.ConfirmManualPayment(h.ctx, h.tenant, p3.ID, 0, "arrived late")
	h.ok(err)
	if late.Status != domain.PaymentSucceeded {
		t.Fatalf("late manual transfer: %s", late.Status)
	}
	if got, _ := h.s.Invoice(h.ctx, h.tenant, inv.ID); got.Status != domain.InvoicePaid {
		t.Fatalf("invoice %s after late transfer", got.Status)
	}
	// A failed payment stays failed.
	_, err = h.s.MockOutcome(h.ctx, h.tenant, p1.ID, "succeed")
	var e *Error
	if !errors.As(err, &e) || e.Code != "payment_not_pending" {
		t.Fatalf("settling a failed payment: %v", err)
	}
	h.reconciled()
}
