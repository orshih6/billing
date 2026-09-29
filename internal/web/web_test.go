package web

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/orshih6/billing/internal/auth"
	"github.com/orshih6/billing/internal/billing"
	"github.com/orshih6/billing/internal/config"
	"github.com/orshih6/billing/internal/models"
	"github.com/orshih6/billing/internal/providers"
	"github.com/orshih6/billing/internal/testsupport"
)

func TestMain(m *testing.M) { os.Exit(testsupport.Run(m)) }

var csrfRE = regexp.MustCompile(`name="_csrf" value="([0-9a-f]+)"`)

type browser struct {
	t    *testing.T
	c    *http.Client
	base string
	csrf string
}

func (b *browser) get(path string) string {
	b.t.Helper()
	res, err := b.c.Get(b.base + path)
	if err != nil {
		b.t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	s := string(body)
	if res.StatusCode != 200 {
		b.t.Fatalf("GET %s: %d\n%s", path, res.StatusCode, s)
	}
	// A template error aborts rendering mid-page.
	if !strings.Contains(s, "</html>") {
		b.t.Fatalf("GET %s: page did not render completely:\n%s", path, s)
	}
	if m := csrfRE.FindStringSubmatch(s); m != nil {
		b.csrf = m[1]
	}
	return s
}

func (b *browser) post(path string, form url.Values) string {
	b.t.Helper()
	form.Set("_csrf", b.csrf)
	res, err := b.c.PostForm(b.base+path, form)
	if err != nil {
		b.t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 {
		b.t.Fatalf("POST %s: %d\n%s", path, res.StatusCode, body)
	}
	return string(body)
}

func setup(t *testing.T) (*billing.Service, *httptest.Server) {
	db := testsupport.DB(t)
	log := testsupport.Logger()
	cfg := config.Load()
	svc := billing.New(db, providers.NewRegistry(providers.Mock{PublicURL: "http://test"}, providers.Manual{}), log, false)
	ui, err := New(svc, auth.NewAuthenticator(db, nil, log), cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	ui.Mount(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return svc, srv
}

func login(t *testing.T, srv *httptest.Server, key string) *browser {
	jar, _ := cookiejar.New(nil)
	b := &browser{t: t, c: &http.Client{Jar: jar}, base: srv.URL}
	body := b.post("/ui/login", url.Values{"api_key": {key}})
	if !strings.Contains(body, "Dashboard") {
		t.Fatalf("login failed:\n%s", body)
	}
	return b
}

// Every page renders for an admin key with real data behind it.
func TestEveryPageRenders(t *testing.T) {
	svc, srv := setup(t)
	ctx := context.Background()
	out, err := svc.CreateTenant(ctx, billing.CreateTenantInput{Slug: "acme", Name: "Acme", CreateAdminKey: true})
	if err != nil {
		t.Fatal(err)
	}
	tid := out.Tenant.ID
	name := "Bat"
	c, _ := svc.CreateCustomer(ctx, tid, billing.CustomerInput{Name: &name})
	p, _ := svc.CreatePlan(ctx, tid, billing.PlanInput{Code: "pro", Name: "Pro", Prices: []billing.PriceInput{{Amount: 50000, Interval: "month"}}})
	sub, err := svc.CreateSubscription(ctx, tid, billing.CreateSubscriptionInput{CustomerID: c.ID, PriceID: &p.Prices[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	pay, _ := svc.StartPayment(ctx, tid, sub.LatestInvoice.ID, billing.StartPaymentInput{Provider: "mock"}, nil)
	ep, _ := svc.CreateWebhookEndpoint(ctx, tid, billing.WebhookEndpointInput{URL: ptr("https://example.com/hook")})
	var acct models.LedgerAccount
	svc.DB().Where("tenant_id = ?", tid).Take(&acct)

	b := login(t, srv, out.APIKey.Token)
	for _, path := range []string{
		"/ui/", "/ui/customers", "/ui/customers/" + c.ID.String(), "/ui/plans", "/ui/plans/" + p.ID.String(),
		"/ui/subscriptions", "/ui/subscriptions?status=incomplete", "/ui/subscriptions/" + sub.ID.String(),
		"/ui/invoices", "/ui/invoices/" + sub.LatestInvoice.ID.String(), "/ui/payments",
		"/ui/ledger", "/ui/ledger?all=1", "/ui/ledger/accounts/" + acct.ID.String(),
		"/ui/keys", "/ui/coupons", "/ui/search?q=Bat", "/ui/webhooks", "/ui/webhooks/" + ep.Endpoint.ID.String(), "/ui/events", "/ui/settings",
		"/checkout/mock/" + pay.ID.String(),
	} {
		b.get(path)
	}

	// Pay through the public mock checkout, then the invoice page shows it.
	b.get("/ui/invoices/" + sub.LatestInvoice.ID.String())
	b.post("/checkout/mock/"+pay.ID.String(), url.Values{"outcome": {"succeed"}})
	if got := b.get("/ui/subscriptions/" + sub.ID.String()); !strings.Contains(got, "has access") {
		t.Fatal("subscription should be entitled after paying")
	}

	// A form round trip: create a customer from the UI.
	b.get("/ui/customers")
	if got := b.post("/ui/customers", url.Values{"name": {"From UI"}}); !strings.Contains(got, "From UI") {
		t.Fatalf("customer not created:\n%s", got)
	}
}

// A narrow key sees a 403 page, not data, and its forms need a CSRF token.
func TestScopedKeyAndCSRF(t *testing.T) {
	svc, srv := setup(t)
	ctx := context.Background()
	out, _ := svc.CreateTenant(ctx, billing.CreateTenantInput{Slug: "acme", Name: "Acme"})
	k, err := svc.CreateKey(ctx, nil, &out.Tenant.ID, billing.CreateKeyInput{Name: "ro", Scopes: []string{"customers:read"}})
	if err != nil {
		t.Fatal(err)
	}
	b := login(t, srv, k.Token)
	b.get("/ui/customers")
	b.get("/ui/settings") // read-only view

	res, _ := b.c.Get(srv.URL + "/ui/ledger")
	if res.StatusCode != 403 {
		t.Fatalf("ledger without ledger:read: %d", res.StatusCode)
	}
	res.Body.Close()

	res, _ = b.c.PostForm(srv.URL+"/ui/customers", url.Values{"name": {"x"}, "_csrf": {"wrong"}})
	if res.StatusCode != 403 {
		t.Fatalf("POST with a bad CSRF token: %d", res.StatusCode)
	}
	res.Body.Close()

	// Unknown keys cannot log in.
	jar, _ := cookiejar.New(nil)
	anon := &http.Client{Jar: jar}
	res, _ = anon.PostForm(srv.URL+"/ui/login", url.Values{"api_key": {"bk_" + uuid.NewString()}})
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(string(body), "not accepted") {
		t.Fatal("bad key logged in")
	}
}

func ptr[T any](v T) *T { return &v }
