package client_test

import (
	"context"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/orshih6/billing/client"
	"github.com/orshih6/billing/internal/api"
	"github.com/orshih6/billing/internal/auth"
	"github.com/orshih6/billing/internal/billing"
	"github.com/orshih6/billing/internal/config"
	"github.com/orshih6/billing/internal/events"
	"github.com/orshih6/billing/internal/providers"
	"github.com/orshih6/billing/internal/testsupport"
)

func TestMain(m *testing.M) { os.Exit(testsupport.Run(m)) }

// The client against the real router: this is the contract test. If a wire
// field is renamed on the server, this breaks.
func TestClientAgainstServer(t *testing.T) {
	db := testsupport.DB(t)
	log := testsupport.Logger()
	svc := billing.New(db, providers.NewRegistry(providers.Mock{PublicURL: "http://test"}, providers.Manual{}), log, false)
	h := api.NewHandler(svc, auth.NewAuthenticator(db, nil, log), db, config.Load(), log)
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()
	ctx := context.Background()

	tenant, err := svc.CreateTenant(ctx, billing.CreateTenantInput{Slug: "acme", Name: "Acme", CreateAdminKey: true})
	if err != nil {
		t.Fatal(err)
	}
	c := client.New(srv.URL, tenant.APIKey.Token)

	plan, err := c.CreatePlan(ctx, client.PlanInput{Code: "pro", Name: "Pro", Prices: []client.PriceInput{{Amount: 49000, Interval: "month"}}})
	if err != nil || len(plan.Prices) != 1 || plan.Prices[0].Currency != "MNT" {
		t.Fatalf("plan: %+v %v", plan, err)
	}
	cust, err := c.EnsureCustomer(ctx, client.CustomerInput{ExternalID: "user-1", Name: "Ann"})
	if err != nil {
		t.Fatal(err)
	}
	again, err := c.EnsureCustomer(ctx, client.CustomerInput{ExternalID: "user-1", Name: "Ann"})
	if err != nil || again.ID != cust.ID {
		t.Fatalf("EnsureCustomer must be idempotent: %v %v", again, err)
	}

	sub, err := c.CreateSubscription(ctx, client.CreateSubscriptionInput{CustomerID: cust.ID, PlanCode: "pro"}, client.WithIdempotencyKey("sub-1"))
	if err != nil || sub.Status != "incomplete" || sub.LatestInvoice == nil || sub.LatestInvoice.AmountDue != 49000 {
		t.Fatalf("subscription: %+v %v", sub, err)
	}
	if _, err := c.CreateSubscription(ctx, client.CreateSubscriptionInput{CustomerID: cust.ID, PlanCode: "pro"}); !client.IsCode(err, "already_subscribed") {
		t.Fatalf("duplicate subscription: %v", err)
	}

	pay, err := c.StartPayment(ctx, sub.LatestInvoice.ID, client.StartPaymentInput{Provider: "mock"})
	if err != nil || pay.PayURL == "" {
		t.Fatalf("payment: %+v %v", pay, err)
	}
	if pay, err = c.SimulateMockPayment(ctx, pay.ID, "succeed"); err != nil || pay.Status != "succeeded" {
		t.Fatalf("mock: %+v %v", pay, err)
	}
	ents, err := c.EntitlementsByExternalID(ctx, "user-1")
	if err != nil || !client.HasPlan(ents, "pro") {
		t.Fatalf("entitlements: %+v %v", ents, err)
	}
	sub, _ = c.GetSubscription(ctx, sub.ID)
	if !sub.Entitled || sub.Status != "active" {
		t.Fatalf("after payment: %+v", sub)
	}

	inv, err := c.CreateInvoice(ctx, client.InvoiceInput{CustomerID: cust.ID, Finalize: true, Lines: []client.LineInput{{Description: "Setup", UnitAmount: 10000}}})
	if err != nil || inv.Status != "open" || inv.Number == nil {
		t.Fatalf("invoice: %+v %v", inv, err)
	}
	if _, err := c.RecordManualPayment(ctx, client.ManualPaymentInput{InvoiceID: inv.ID, Amount: 15000, Reference: "BANK-1"}); err != nil {
		t.Fatal(err)
	}
	bal, _ := c.CustomerBalances(ctx, cust.ID)
	if len(bal) != 1 || bal[0].Credit != 5000 || bal[0].Owed != 0 {
		t.Fatalf("balances: %+v", bal)
	}
	if sub, err = c.CancelSubscription(ctx, sub.ID, true); err != nil || !sub.CancelAtPeriodEnd {
		t.Fatalf("cancel: %+v %v", sub, err)
	}
	// Seats, add-ons and a promo code in one checkout.
	if _, err := c.CreatePlan(ctx, client.PlanInput{Code: "team", Name: "Team", Prices: []client.PriceInput{{Amount: 10000, Interval: "month"}}}); err != nil {
		t.Fatal(err)
	}
	extra, err := c.CreatePlan(ctx, client.PlanInput{Code: "storage", Name: "Storage", Kind: "addon", Prices: []client.PriceInput{{Amount: 3000, Interval: "month"}}})
	if err != nil {
		t.Fatal(err)
	}
	pct := 10
	if _, err := c.CreateCoupon(ctx, client.CouponInput{Code: "TEN", PercentOff: &pct, Duration: "forever"}); err != nil {
		t.Fatal(err)
	}
	if cp, err := c.CheckPromoCode(ctx, "ten", "MNT"); err != nil || cp.Code != "TEN" {
		t.Fatalf("check promo: %+v %v", cp, err)
	}
	team, err := c.CreateSubscription(ctx, client.CreateSubscriptionInput{CustomerID: cust.ID, PlanCode: "team", Quantity: 3,
		AddOns: []client.AddOnInput{{PriceID: extra.Prices[0].ID, Quantity: 2}}, PromoCode: "ten"})
	if err != nil {
		t.Fatal(err)
	}
	// 3×10000 + 2×3000 = 36000, 10% off = 32400, minus 5000 credit held.
	li := team.LatestInvoice
	if team.Quantity != 3 || len(team.AddOns) != 1 || team.Discount == nil || li.Subtotal != 36000 || li.DiscountTotal != 3600 || li.Total != 32400 {
		t.Fatalf("team subscription: %+v invoice %+v", team, li)
	}
	// Unpaid subscriptions can't change; pay first (the 5000 credit held covers part).
	if _, err := c.SetQuantity(ctx, team.ID, 2); !client.IsCode(err, "subscription_not_changeable") {
		t.Fatalf("incomplete subscription changed: %v", err)
	}
	tp, err := c.StartPayment(ctx, li.ID, client.StartPaymentInput{Provider: "mock"})
	if err != nil || tp.Amount != 27400 {
		t.Fatalf("remaining due after credit: %+v %v", tp, err)
	}
	if _, err := c.SimulateMockPayment(ctx, tp.ID, "succeed"); err != nil {
		t.Fatal(err)
	}
	if team, err = c.SetQuantity(ctx, team.ID, 2); err != nil || team.PendingQuantity == nil || *team.PendingQuantity != 2 {
		t.Fatalf("seat decrease: %+v %v", team, err)
	}
	if res, err := c.Search(ctx, "user-1"); err != nil || len(res.Customers) != 1 || res.Customers[0].ID != cust.ID {
		t.Fatalf("search: %+v %v", res, err)
	}
	if _, err := c.GetCustomer(ctx, "00000000-0000-0000-0000-000000000000"); !client.IsCode(err, "customer_not_found") {
		t.Fatalf("not found: %v", err)
	}
}

func TestVerifyWebhook(t *testing.T) {
	body := []byte(`{"id":"e1","type":"invoice.paid","tenant_id":"t","created_at":"2026-01-01T00:00:00Z","data":{}}`)
	sig := events.Sign("whsec_abc", time.Now(), body)
	ev, err := client.VerifyWebhook(body, sig, "whsec_abc", 5*time.Minute)
	if err != nil || ev.Type != "invoice.paid" {
		t.Fatalf("valid signature rejected: %v", err)
	}
	if _, err := client.VerifyWebhook(body, sig, "whsec_other", 5*time.Minute); err != client.ErrInvalidSignature {
		t.Fatal("wrong secret accepted")
	}
	if _, err := client.VerifyWebhook(append(body, ' '), sig, "whsec_abc", 5*time.Minute); err != client.ErrInvalidSignature {
		t.Fatal("tampered body accepted")
	}
	old := events.Sign("whsec_abc", time.Now().Add(-time.Hour), body)
	if _, err := client.VerifyWebhook(body, old, "whsec_abc", 5*time.Minute); err != client.ErrInvalidSignature {
		t.Fatal("stale signature accepted")
	}
}
