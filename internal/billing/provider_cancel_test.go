package billing

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orshih6/billing/internal/domain"
	"github.com/orshih6/billing/internal/models"
	"github.com/orshih6/billing/internal/providers"
)

// fakeGateway is a provider that supports cancellation, with scripted answers.
type fakeGateway struct {
	mu        sync.Mutex
	cancelErr error
	check     providers.CheckResult
	canceled  []string
}

func (*fakeGateway) Name() string              { return "fakegw" }
func (*fakeGateway) Automatic() bool           { return true }
func (*fakeGateway) Fields() []providers.Field { return nil }
func (*fakeGateway) CreateIntent(_ context.Context, _ *providers.Account, in providers.IntentInput) (*providers.Intent, error) {
	return &providers.Intent{ProviderRef: "fake_" + in.PaymentID, PayURL: "https://gw/pay/" + in.PaymentID}, nil
}
func (f *fakeGateway) Check(context.Context, *providers.Account, string) (*providers.CheckResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.check
	return &c, nil
}
func (*fakeGateway) ParseWebhook(*http.Request, *providers.Account) (string, error) {
	return "", providers.ErrUnsupported
}
func (f *fakeGateway) CancelIntent(_ context.Context, _ *providers.Account, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.canceled = append(f.canceled, ref)
	return f.cancelErr
}
func (f *fakeGateway) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.canceled)
}

func withGateway(h *harness) *fakeGateway {
	gw := &fakeGateway{}
	h.s.providers = providers.NewRegistry(providers.Mock{PublicURL: "http://test"}, providers.Manual{}, gw)
	return gw
}

func (h *harness) openInvoice(amount int64) (*models.Customer, *models.Invoice) {
	h.t.Helper()
	c := h.customer()
	inv, err := h.s.CreateInvoice(h.ctx, h.tenant, InvoiceInput{CustomerID: c.ID, Finalize: true, Lines: []LineInput{{Description: "x", UnitAmount: amount}}})
	h.ok(err)
	return c, inv
}

func (h *harness) payment(id uuid.UUID) *models.Payment {
	h.t.Helper()
	p, err := h.s.Payment(h.ctx, h.tenant, id)
	h.ok(err)
	return p
}

// Paying the invoice one way cancels the other attempts; only the provider
// that can cancel is asked to, once, after the transaction.
func TestPaidElsewhereCancelsOtherIntents(t *testing.T) {
	h := newHarness(t)
	gw := withGateway(h)
	_, inv := h.openInvoice(10000)

	viaGateway, err := h.s.StartPayment(h.ctx, h.tenant, inv.ID, StartPaymentInput{Provider: "fakegw"}, nil)
	h.ok(err)
	viaManual, err := h.s.StartPayment(h.ctx, h.tenant, inv.ID, StartPaymentInput{Provider: "manual"}, nil)
	h.ok(err)

	// The customer pays with the mock checkout instead.
	h.payMock(inv.ID)

	g := h.payment(viaGateway.ID)
	if g.Status != domain.PaymentCanceled || g.CancelReason != "invoice paid by another payment" || g.ProviderCancel != models.ProviderCancelPending {
		t.Fatalf("gateway payment: %s %q %q", g.Status, g.CancelReason, g.ProviderCancel)
	}
	if gw.calls() != 0 {
		t.Fatal("the gateway must not be called inside the settling transaction")
	}
	m := h.payment(viaManual.ID)
	if m.Status != domain.PaymentCanceled || m.ProviderCancel != "" {
		t.Fatalf("manual has no gateway to cancel: %s %q", m.Status, m.ProviderCancel)
	}

	r := h.advance(0)
	if r.ProviderCancels.Done != 1 || gw.calls() != 1 || gw.canceled[0] != g.ProviderRef {
		t.Fatalf("provider cancels: %+v, calls %v", r.ProviderCancels, gw.canceled)
	}
	if g = h.payment(viaGateway.ID); g.ProviderCancel != models.ProviderCancelDone || g.ProviderCanceledAt == nil {
		t.Fatalf("after worker: %q", g.ProviderCancel)
	}
	h.advance(time.Hour)
	if gw.calls() != 1 {
		t.Fatal("a done cancel must not be sent again")
	}
	h.reconciled()
}

// The customer paid the gateway intent before our cancel arrived: the money is
// recorded as credit, never lost.
func TestCancelRaceAlreadyPaidBecomesCredit(t *testing.T) {
	h := newHarness(t)
	gw := withGateway(h)
	c, inv := h.openInvoice(10000)
	viaGateway, _ := h.s.StartPayment(h.ctx, h.tenant, inv.ID, StartPaymentInput{Provider: "fakegw"}, nil)
	h.payMock(inv.ID)

	gw.cancelErr = providers.ErrAlreadyPaid
	gw.check = providers.CheckResult{Status: providers.CheckPaid, Amount: 10000}
	r := h.advance(0)
	if r.ProviderCancels.AlreadyPaid != 1 {
		t.Fatalf("report: %+v", r.ProviderCancels)
	}
	g := h.payment(viaGateway.ID)
	if g.Status != domain.PaymentSucceeded || g.ProviderCancel != models.ProviderCancelAlreadyPaid {
		t.Fatalf("late gateway payment: %s %q", g.Status, g.ProviderCancel)
	}
	if b := h.balance(c.ID); b.Credit != 10000 {
		t.Fatalf("double payment must become credit, got %+v", b)
	}
	h.reconciled()
}

// Failing cancels retry with backoff, then give up; an operator can re-queue.
func TestProviderCancelRetriesThenFails(t *testing.T) {
	h := newHarness(t)
	gw := withGateway(h)
	_, inv := h.openInvoice(500)
	p, _ := h.s.StartPayment(h.ctx, h.tenant, inv.ID, StartPaymentInput{Provider: "fakegw"}, nil)
	_, err := h.s.CancelPayment(h.ctx, h.tenant, p.ID)
	h.ok(err)

	gw.cancelErr = errors.New("gateway 503")
	if r := h.advance(0); r.ProviderCancels.Retrying != 1 {
		t.Fatalf("first attempt: %+v", r.ProviderCancels)
	}
	if r := h.advance(30 * time.Second); gw.calls() != 1 || r.ProviderCancels.Retrying != 0 {
		t.Fatal("retried before the backoff elapsed")
	}
	for range providerCancelMaxAttempts {
		h.advance(time.Hour)
	}
	got := h.payment(p.ID)
	if got.ProviderCancel != models.ProviderCancelFailed || got.ProviderCancelAttempts != providerCancelMaxAttempts || got.ProviderCancelError == "" {
		t.Fatalf("after retries: %q attempts %d err %q", got.ProviderCancel, got.ProviderCancelAttempts, got.ProviderCancelError)
	}

	gw.cancelErr = nil
	_, err = h.s.RetryProviderCancel(h.ctx, h.tenant, p.ID)
	h.ok(err)
	h.advance(0)
	if got = h.payment(p.ID); got.ProviderCancel != models.ProviderCancelDone {
		t.Fatalf("after retry: %q", got.ProviderCancel)
	}
}

// The mock behaves like a gateway: a canceled checkout can be paid only until
// the cancel has reached it.
func TestMockCheckoutClosesAfterCancel(t *testing.T) {
	h := newHarness(t)
	c, inv := h.openInvoice(700)
	first, _ := h.s.StartPayment(h.ctx, h.tenant, inv.ID, StartPaymentInput{Provider: "mock"}, nil)
	_, err := h.s.RecordManualPayment(h.ctx, h.tenant, ManualPaymentInput{InvoiceID: inv.ID, Amount: 700}, nil)
	h.ok(err)
	if h.payment(first.ID).ProviderCancel != models.ProviderCancelPending {
		t.Fatal("mock intent should be queued for cancel")
	}

	// Race window: still payable → recorded as credit.
	p, err := h.s.MockOutcome(h.ctx, h.tenant, first.ID, "succeed")
	h.ok(err)
	if p.Status != domain.PaymentSucceeded || h.balance(c.ID).Credit != 700 {
		t.Fatalf("late mock payment: %s credit %d", p.Status, h.balance(c.ID).Credit)
	}

	// After the cancel went through, the checkout refuses.
	_, inv2 := h.openInvoice(300)
	second, _ := h.s.StartPayment(h.ctx, h.tenant, inv2.ID, StartPaymentInput{Provider: "mock"}, nil)
	_, err = h.s.CancelPayment(h.ctx, h.tenant, second.ID)
	h.ok(err)
	h.advance(0)
	_, err = h.s.MockOutcome(h.ctx, h.tenant, second.ID, "succeed")
	expectCode(t, err, "payment_canceled")
	h.reconciled()
}
