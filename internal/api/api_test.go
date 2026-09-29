package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/orshih6/billing/internal/auth"
	"github.com/orshih6/billing/internal/billing"
	"github.com/orshih6/billing/internal/config"
	"github.com/orshih6/billing/internal/providers"
	"github.com/orshih6/billing/internal/testsupport"
)

func TestMain(m *testing.M) { os.Exit(testsupport.Run(m)) }

const bootstrapKey = "test-bootstrap-platform-key-0123456789"

type env struct {
	t   *testing.T
	srv *httptest.Server
}

func newEnv(t *testing.T) *env {
	db := testsupport.DB(t)
	cfg := config.Load()
	log := testsupport.Logger()
	svc := billing.New(db, providers.NewRegistry(providers.Mock{PublicURL: "http://test"}, providers.Manual{}), log, false)
	h := NewHandler(svc, auth.NewAuthenticator(db, []string{bootstrapKey}, log), db, cfg, log)
	srv := httptest.NewServer(h.Routes())
	t.Cleanup(srv.Close)
	return &env{t: t, srv: srv}
}

type resp struct {
	Status int
	Body   map[string]any
	Header http.Header
}

func (e *env) do(method, path, key string, body any, headers ...string) resp {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set(HeaderAPIKey, key)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	out := resp{Status: res.StatusCode, Header: res.Header, Body: map[string]any{}}
	_ = json.Unmarshal(raw, &out.Body)
	return out
}

func (e *env) must(r resp, status int) resp {
	e.t.Helper()
	if r.Status != status {
		e.t.Fatalf("status %d, want %d: %v", r.Status, status, r.Body)
	}
	return r
}

func str(m map[string]any, path ...string) string {
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = mm[p]
	}
	s, _ := cur.(string)
	return s
}

func errCode(r resp) string { return str(r.Body, "error", "code") }

// tenant creates a tenant through the platform key and returns its admin key.
func (e *env) tenant(slug string) (id, key string) {
	e.t.Helper()
	r := e.must(e.do("POST", "/tenants", bootstrapKey, map[string]any{"slug": slug, "name": slug, "create_admin_key": true}), 201)
	return str(r.Body, "tenant", "id"), str(r.Body, "api_key", "token")
}

func TestOnlyXAPIKeyAuthenticates(t *testing.T) {
	e := newEnv(t)
	if r := e.do("GET", "/me", "", nil); r.Status != 401 {
		t.Fatalf("no key: %d", r.Status)
	}
	// Authorization: Bearer is deliberately ignored.
	if r := e.do("GET", "/me", "", nil, "Authorization", "Bearer "+bootstrapKey); r.Status != 401 {
		t.Fatalf("bearer header must not authenticate, got %d", r.Status)
	}
	e.must(e.do("GET", "/me", bootstrapKey, nil), 200)
	if r := e.do("GET", "/me", "bk_nottherightkeyatall000000000000", nil); errCode(r) != "unauthorized" {
		t.Fatalf("bad key: %v", r.Body)
	}
	// Probes need no key; routes are unversioned.
	e.must(e.do("GET", "/healthz", "", nil), 200)
	if r := e.do("GET", "/v1/customers", bootstrapKey, nil); r.Status != 404 {
		t.Fatalf("/v1 prefix must not exist, got %d", r.Status)
	}
}

func TestTenantIsolationAcrossRoutes(t *testing.T) {
	e := newEnv(t)
	_, keyA := e.tenant("alpha")
	idB, keyB := e.tenant("beta")

	cust := e.must(e.do("POST", "/customers", keyA, map[string]any{"name": "Ann", "external_id": "u1"}), 201)
	custID := str(cust.Body, "id")
	plan := e.must(e.do("POST", "/plans", keyA, map[string]any{"code": "pro", "name": "Pro",
		"prices": []any{map[string]any{"amount": 1000, "interval": "month"}}}), 201)
	priceID := plan.Body["prices"].([]any)[0].(map[string]any)["id"].(string)
	sub := e.must(e.do("POST", "/subscriptions", keyA, map[string]any{"customer_id": custID, "price_id": priceID}), 201)
	subID := str(sub.Body, "id")
	invID := str(sub.Body, "latest_invoice", "id")

	for _, path := range []string{
		"/customers/" + custID, "/customers/" + custID + "/balance", "/customers/" + custID + "/entitlements",
		"/customers/by-external/u1", "/plans/" + str(plan.Body, "id"), "/prices/" + priceID,
		"/subscriptions/" + subID, "/invoices/" + invID,
	} {
		if r := e.do("GET", path, keyB, nil); r.Status != 404 {
			t.Errorf("tenant B read %s: %d %v", path, r.Status, r.Body)
		}
		e.must(e.do("GET", path, keyA, nil), 200)
	}
	for _, path := range []string{"/invoices/" + invID + "/void", "/subscriptions/" + subID + "/cancel", "/invoices/" + invID + "/payments"} {
		if r := e.do("POST", path, keyB, map[string]any{}); r.Status != 404 && r.Status != 422 {
			t.Errorf("tenant B wrote %s: %d %v", path, r.Status, r.Body)
		}
	}
	// A tenant key cannot point itself at another tenant.
	if r := e.do("GET", "/customers", keyA, nil, HeaderTenant, idB); errCode(r) != "tenant_mismatch" {
		t.Fatalf("X-Tenant-Id override: %v", r.Body)
	}
	// Lists never leak.
	r := e.must(e.do("GET", "/customers", keyB, nil), 200)
	if n := len(r.Body["data"].([]any)); n != 0 {
		t.Fatalf("tenant B sees %d customers", n)
	}
	// A platform key must say which tenant.
	if r := e.do("GET", "/customers", bootstrapKey, nil); errCode(r) != "tenant_required" {
		t.Fatalf("platform without tenant: %v", r.Body)
	}
	e.must(e.do("GET", "/customers", bootstrapKey, nil, HeaderTenant, idB), 200)
	// Tenant keys cannot reach platform routes.
	if r := e.do("GET", "/tenants", keyA, nil); errCode(r) != "platform_only" {
		t.Fatalf("tenant key on /tenants: %v", r.Body)
	}
}

func TestScopesAndEscalation(t *testing.T) {
	e := newEnv(t)
	_, admin := e.tenant("gamma")
	ro := e.must(e.do("POST", "/api-keys", admin, map[string]any{"name": "ro", "scopes": []string{"customers:read", "keys:write"}}), 201)
	roKey := str(ro.Body, "token")

	e.must(e.do("GET", "/customers", roKey, nil), 200)
	if r := e.do("POST", "/customers", roKey, map[string]any{"name": "x"}); errCode(r) != "insufficient_scope" {
		t.Fatalf("write with read key: %v", r.Body)
	}
	if r := e.do("POST", "/api-keys", roKey, map[string]any{"name": "x", "scopes": []string{"admin"}}); errCode(r) != "scope_escalation" {
		t.Fatalf("escalation: %v", r.Body)
	}
	// payments:write does not imply payments:manual.
	pw := str(e.must(e.do("POST", "/api-keys", admin, map[string]any{"name": "pw", "scopes": []string{"payments:write"}}), 201).Body, "token")
	if r := e.do("POST", "/payments/manual", pw, map[string]any{}); errCode(r) != "insufficient_scope" {
		t.Fatalf("manual with payments:write: %v", r.Body)
	}
	// Revoked keys stop working.
	id := str(ro.Body, "key", "id")
	e.must(e.do("POST", "/api-keys/"+id+"/revoke", admin, nil), 200)
	if r := e.do("GET", "/customers", roKey, nil); errCode(r) != "key_revoked" {
		t.Fatalf("revoked key: %v", r.Body)
	}
	// Unknown JSON fields are rejected, not ignored.
	if r := e.do("POST", "/customers", admin, map[string]any{"nme": "typo"}); errCode(r) != "invalid_json" {
		t.Fatalf("unknown field: %v", r.Body)
	}
}

func TestIdempotencyKey(t *testing.T) {
	e := newEnv(t)
	_, key := e.tenant("delta")
	body := map[string]any{"name": "Once", "external_id": "once"}
	r1 := e.must(e.do("POST", "/customers", key, body, HeaderIdempotencyKey, "k-1"), 201)
	r2 := e.must(e.do("POST", "/customers", key, body, HeaderIdempotencyKey, "k-1"), 201)
	if str(r1.Body, "id") != str(r2.Body, "id") || r2.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay differs: %v vs %v", r1.Body, r2.Body)
	}
	if r := e.do("POST", "/customers", key, map[string]any{"name": "Other"}, HeaderIdempotencyKey, "k-1"); errCode(r) != "idempotency_key_reused" {
		t.Fatalf("reused key: %v", r.Body)
	}
	list := e.must(e.do("GET", "/customers", key, nil), 200)
	if n := len(list.Body["data"].([]any)); n != 1 {
		t.Fatalf("%d customers, want 1", n)
	}
}

// The integration path a SaaS actually follows, over HTTP.
func TestCheckoutFlowOverHTTP(t *testing.T) {
	e := newEnv(t)
	_, key := e.tenant("epsilon")
	cust := str(e.must(e.do("POST", "/customers", key, map[string]any{"name": "Bold", "external_id": "user-42"}), 201).Body, "id")
	e.must(e.do("POST", "/plans", key, map[string]any{"code": "team", "name": "Team",
		"prices": []any{map[string]any{"amount": 99000, "interval": "month"}}}), 201)
	sub := e.must(e.do("POST", "/subscriptions", key, map[string]any{"customer_id": cust, "plan_code": "team"}), 201)
	if str(sub.Body, "status") != "incomplete" {
		t.Fatalf("subscription: %v", sub.Body)
	}
	inv := str(sub.Body, "latest_invoice", "id")
	pay := e.must(e.do("POST", "/invoices/"+inv+"/payments", key, map[string]any{"provider": "mock", "return_url": "https://app/return"}), 201)
	if !strings.HasPrefix(str(pay.Body, "pay_url"), "http://test/checkout/mock/") {
		t.Fatalf("pay url: %v", pay.Body)
	}
	e.must(e.do("POST", "/payments/"+str(pay.Body, "id")+"/mock/succeed", key, nil), 200)

	ent := e.must(e.do("GET", "/customers/by-external/user-42/entitlements", key, nil), 200)
	list := ent.Body["entitlements"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["plan_code"] != "team" {
		t.Fatalf("entitlements: %v", ent.Body)
	}
	rec := e.must(e.do("GET", "/reports/ledger-reconciliation", key, nil), 200)
	if rec.Body["ok"] != true {
		t.Fatalf("books: %v", rec.Body)
	}
	evs := e.must(e.do("GET", "/events?type=subscription.activated", key, nil), 200)
	if len(evs.Body["data"].([]any)) != 1 {
		t.Fatalf("activation event missing: %v", evs.Body)
	}
}

// Responses and webhook payloads use the public wire types: every object
// names its type, and internal columns never leak.
func TestWireContract(t *testing.T) {
	e := newEnv(t)
	_, key := e.tenant("zeta")
	cust := e.must(e.do("POST", "/customers", key, map[string]any{"name": "Wire"}), 201)
	if cust.Body["object"] != "customer" {
		t.Fatalf("customer object tag: %v", cust.Body)
	}
	for _, internal := range []string{"tenant_id", "updated_at", "archived_at"} {
		if _, ok := cust.Body[internal]; ok {
			t.Errorf("customer leaks %s", internal)
		}
	}
	list := e.must(e.do("GET", "/customers", key, nil), 200)
	if list.Body["object"] != "list" || list.Body["data"].([]any)[0].(map[string]any)["object"] != "customer" {
		t.Fatalf("list shape: %v", list.Body)
	}
	e.must(e.do("POST", "/plans", key, map[string]any{"code": "p", "name": "P", "prices": []any{map[string]any{"amount": 100, "interval": "month"}}}), 201)
	sub := e.must(e.do("POST", "/subscriptions", key, map[string]any{"customer_id": str(cust.Body, "id"), "plan_code": "p"}), 201)
	if sub.Body["object"] != "subscription" || sub.Body["plan_code"] != "p" || str(sub.Body, "latest_invoice", "object") != "invoice" {
		t.Fatalf("subscription shape: %v", sub.Body)
	}
	if _, ok := sub.Body["billing_anchor"]; ok {
		t.Error("subscription leaks billing_anchor")
	}
	ev := e.must(e.do("GET", "/events?type=invoice.finalized", key, nil), 200)
	data := ev.Body["data"].([]any)[0].(map[string]any)["data"].(map[string]any)
	if data["object"] != "invoice" {
		t.Fatalf("event payload must be the wire invoice: %v", data)
	}
	if _, ok := data["tenant_id"]; ok {
		t.Error("event payload leaks tenant_id")
	}
}

func TestSearchAndFilters(t *testing.T) {
	e := newEnv(t)
	_, key := e.tenant("eta")
	c1 := str(e.must(e.do("POST", "/customers", key, map[string]any{"name": "Oyun Bold", "email": "oyun@example.mn", "external_id": "u-77",
		"metadata": map[string]any{"tier": "gold"}}), 201).Body, "id")
	e.must(e.do("POST", "/customers", key, map[string]any{"name": "Tuul 100%_real"}), 201)
	inv := e.must(e.do("POST", "/invoices", key, map[string]any{"customer_id": c1, "finalize": true,
		"lines": []any{map[string]any{"description": "Setup", "unit_amount": 500}}}), 201)
	number, ref := str(inv.Body, "number"), str(inv.Body, "reference")

	names := func(r resp, group string) []string {
		var out []string
		for _, x := range r.Body[group].([]any) {
			m := x.(map[string]any)
			if n, ok := m["name"].(string); ok {
				out = append(out, n)
			} else {
				out = append(out, m["number"].(string))
			}
		}
		return out
	}
	if got := names(e.must(e.do("GET", "/search?q=oyun", key, nil), 200), "customers"); len(got) != 1 || got[0] != "Oyun Bold" {
		t.Fatalf("search by name: %v", got)
	}
	if got := names(e.must(e.do("GET", "/search?q="+number, key, nil), 200), "invoices"); len(got) != 1 {
		t.Fatalf("search by invoice number: %v", got)
	}
	if got := names(e.must(e.do("GET", "/search?q="+strings.ToLower(ref), key, nil), 200), "invoices"); len(got) != 1 {
		t.Fatalf("search by reference, any case: %v", got)
	}
	if got := names(e.must(e.do("GET", "/search?q="+c1, key, nil), 200), "invoices"); len(got) != 1 {
		t.Fatalf("pasting a customer id finds its invoices: %v", got)
	}
	// % and _ are literal, not wildcards.
	if got := names(e.must(e.do("GET", "/search?q=%25_", key, nil), 200), "customers"); len(got) != 1 || got[0] != "Tuul 100%_real" {
		t.Fatalf("wildcards must be escaped: %v", got)
	}
	if r := e.do("GET", "/search?q=a", key, nil); errCode(r) != "query_too_short" {
		t.Fatalf("short query: %v", r.Body)
	}
	// A key only searches what it may read.
	ro := str(e.must(e.do("POST", "/api-keys", key, map[string]any{"name": "ro", "scopes": []string{"customers:read"}}), 201).Body, "token")
	res := e.must(e.do("GET", "/search?q="+number, ro, nil), 200)
	if len(res.Body["invoices"].([]any)) != 0 {
		t.Fatal("a customers-only key must not see invoices in search")
	}

	// List filters.
	if l := e.must(e.do("GET", "/invoices?q="+ref, key, nil), 200); len(l.Body["data"].([]any)) != 1 {
		t.Fatalf("invoice q filter: %v", l.Body)
	}
	if l := e.must(e.do("GET", "/customers?metadata[tier]=gold", key, nil), 200); len(l.Body["data"].([]any)) != 1 {
		t.Fatalf("metadata filter: %v", l.Body)
	}
	if l := e.must(e.do("GET", "/customers?created_after=2999-01-01", key, nil), 200); len(l.Body["data"].([]any)) != 0 {
		t.Fatalf("created_after filter: %v", l.Body)
	}
	if r := e.do("GET", "/customers?created_before=yesterday", key, nil); errCode(r) != "invalid_query" {
		t.Fatalf("bad date: %v", r.Body)
	}
	if r := e.do("GET", "/payments?metadata[x]=y", key, nil); errCode(r) != "invalid_filter" {
		t.Fatalf("payments have no metadata: %v", r.Body)
	}
}
