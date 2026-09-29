package billing

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/orshih6/billing/internal/domain"
	"github.com/orshih6/billing/internal/providers"
	"github.com/orshih6/billing/internal/secretbox"
)

// fakePay needs a merchant id and a secret, and signs its callbacks with it.
type fakePay struct {
	seen []providers.Account
	paid map[string]int64
}

func (*fakePay) Name() string    { return "fakepay" }
func (*fakePay) Automatic() bool { return true }
func (*fakePay) Fields() []providers.Field {
	return []providers.Field{
		{Key: "merchant_id", Label: "Merchant ID", Required: true},
		{Key: "api_secret", Label: "API secret", Secret: true, Required: true},
	}
}
func (f *fakePay) CreateIntent(_ context.Context, a *providers.Account, in providers.IntentInput) (*providers.Intent, error) {
	f.seen = append(f.seen, *a)
	return &providers.Intent{ProviderRef: a.Get("merchant_id") + ":" + in.PaymentID}, nil
}
func (f *fakePay) Check(_ context.Context, _ *providers.Account, ref string) (*providers.CheckResult, error) {
	if amt, ok := f.paid[ref]; ok {
		return &providers.CheckResult{Status: providers.CheckPaid, Amount: amt}, nil
	}
	return &providers.CheckResult{Status: providers.CheckPending}, nil
}
func (*fakePay) ParseWebhook(r *http.Request, a *providers.Account) (string, error) {
	if r.Header.Get("X-Secret") != a.Get("api_secret") {
		return "", errors.New("bad signature")
	}
	var body struct{ Ref string }
	b, _ := io.ReadAll(r.Body)
	return body.Ref, json.Unmarshal(b, &body)
}

func TestProviderAccounts(t *testing.T) {
	h := newHarness(t)
	fp := &fakePay{paid: map[string]int64{}}
	h.s.providers = providers.NewRegistry(providers.Mock{}, providers.Manual{}, fp)
	_, inv := h.openInvoice(9000)

	_, err := h.s.StartPayment(h.ctx, h.tenant, inv.ID, StartPaymentInput{Provider: "fakepay"}, nil)
	expectCode(t, err, "provider_not_configured")

	_, err = h.s.ConfigureProvider(h.ctx, h.tenant, "fakepay", ProviderAccountInput{Config: map[string]string{"merchant_id": "M1"}})
	expectCode(t, err, "missing_field")
	_, err = h.s.ConfigureProvider(h.ctx, h.tenant, "fakepay", ProviderAccountInput{Config: map[string]string{"nope": "x"}})
	expectCode(t, err, "unknown_field")
	view, err := h.s.ConfigureProvider(h.ctx, h.tenant, "fakepay", ProviderAccountInput{Config: map[string]string{"merchant_id": "M1", "api_secret": "sk_live_abcd1234"}})
	h.ok(err)
	if view.Hints["api_secret"] != "••••1234" || view.Hints["merchant_id"] != "M1" || view.Mode != "test" {
		t.Fatalf("hints must mask secrets: %+v", view)
	}
	raw, _ := json.Marshal(view)
	if strings.Contains(string(raw), "sk_live") {
		t.Fatalf("secret leaked in view: %s", raw)
	}

	// Re-saving with an empty secret keeps the stored one.
	_, err = h.s.ConfigureProvider(h.ctx, h.tenant, "fakepay", ProviderAccountInput{Mode: "live", Config: map[string]string{"merchant_id": "M2", "api_secret": ""}})
	h.ok(err)
	pay, err := h.s.StartPayment(h.ctx, h.tenant, inv.ID, StartPaymentInput{Provider: "fakepay"}, nil)
	h.ok(err)
	got := fp.seen[len(fp.seen)-1]
	if got.Get("merchant_id") != "M2" || got.Get("api_secret") != "sk_live_abcd1234" || got.Mode != "live" || pay.ProviderAccountID == nil {
		t.Fatalf("provider got %+v, payment %+v", got, pay)
	}

	// Another tenant has no account of its own.
	other, _ := h.s.CreateTenant(h.ctx, CreateTenantInput{Slug: "other", Name: "Other"})
	name := "x"
	oc, _ := h.s.CreateCustomer(h.ctx, other.Tenant.ID, CustomerInput{Name: &name})
	oinv, _ := h.s.CreateInvoice(h.ctx, other.Tenant.ID, InvoiceInput{CustomerID: oc.ID, Finalize: true, Lines: []LineInput{{Description: "x", UnitAmount: 1}}})
	_, err = h.s.StartPayment(h.ctx, other.Tenant.ID, oinv.ID, StartPaymentInput{Provider: "fakepay"}, nil)
	expectCode(t, err, "provider_not_configured")

	// Webhook to the account's URL: signature checked with its secret, then
	// the provider is asked and the payment settles.
	fp.paid[pay.ProviderRef] = 9000
	hook := func(secret string) error {
		req := httptest.NewRequest("POST", "/webhooks/fakepay/"+pay.ProviderAccountID.String(), strings.NewReader(`{"Ref":"`+pay.ProviderRef+`"}`))
		req.Header.Set("X-Secret", secret)
		return h.s.HandleWebhook(h.ctx, "fakepay", *pay.ProviderAccountID, req)
	}
	expectCode(t, hook("wrong"), "invalid_webhook")
	h.ok(hook("sk_live_abcd1234"))
	if p := h.payment(pay.ID); p.Status != domain.PaymentSucceeded {
		t.Fatalf("webhook did not settle: %s", p.Status)
	}
	expectCode(t, h.s.HandleWebhook(h.ctx, "fakepay", uuid.New(), httptest.NewRequest("POST", "/", nil)), "provider_account_not_found")

	// A different SECRETS_KEY cannot read the stored credentials.
	h.s.Secrets, _ = secretbox.New("a-completely-different-secrets-key-000")
	_, inv2 := h.openInvoice(100)
	if _, err := h.s.StartPayment(h.ctx, h.tenant, inv2.ID, StartPaymentInput{Provider: "fakepay"}, nil); err == nil || !strings.Contains(err.Error(), "SECRETS_KEY") {
		t.Fatalf("wrong key must fail loudly, got %v", err)
	}
	h.reconciled()
}
