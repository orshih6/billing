package web

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/orshih6/billing/internal/auth"
	"github.com/orshih6/billing/internal/billing"
	"github.com/orshih6/billing/internal/domain"
	"github.com/orshih6/billing/internal/events"
	"github.com/orshih6/billing/internal/models"
)

// Mount attaches the UI and the public mock checkout to the router.
func (u *UI) Mount(r chi.Router) {
	r.Handle("/ui/static/*", staticHandler())
	r.Get("/ui/login", u.loginPage)
	r.Post("/ui/login", u.login)
	r.Get("/", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/ui/", http.StatusSeeOther) })
	r.Get("/ui", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/ui/", http.StatusSeeOther) })

	// Public: the mock provider's fake checkout page.
	r.Get("/checkout/mock/{paymentID}", u.mockCheckout)
	r.Post("/checkout/mock/{paymentID}", u.mockCheckoutSubmit)

	r.Route("/ui/", func(r chi.Router) {
		r.Use(u.requireSession)
		r.Post("/logout", u.logout)
		r.Post("/tenant", u.switchTenant)
		r.Get("/", u.dashboard)
		r.Get("/search", u.search)

		r.Get("/tenants", u.tenants)
		r.Post("/tenants", u.createTenant)
		r.Post("/engine/run", u.runEngine)

		r.Get("/customers", u.customers)
		r.Post("/customers", u.createCustomer)
		r.Get("/customers/{id}", u.customer)
		r.Post("/customers/{id}", u.updateCustomer)
		r.Post("/customers/{id}/credit", u.grantCredit)
		r.Post("/customers/{id}/subscribe", u.subscribe)
		r.Post("/customers/{id}/invoice", u.createInvoice)

		r.Get("/plans", u.plans)
		r.Post("/plans", u.createPlan)
		r.Get("/plans/{id}", u.plan)
		r.Post("/plans/{id}", u.updatePlan)
		r.Post("/plans/{id}/prices", u.addPrice)
		r.Post("/prices/{id}/toggle", u.togglePrice)

		r.Get("/subscriptions", u.subscriptions)
		r.Get("/subscriptions/{id}", u.subscription)
		r.Post("/subscriptions/{id}/cancel", u.cancelSubscription)
		r.Post("/subscriptions/{id}/resume", u.resumeSubscription)
		r.Post("/subscriptions/{id}/change-price", u.changePrice)
		r.Post("/subscriptions/{id}/quantity", u.setQuantity)
		r.Post("/subscriptions/{id}/add-ons", u.setAddOn)
		r.Post("/subscriptions/{id}/discount", u.applyPromo)
		r.Post("/subscriptions/{id}/discount/remove", u.removeDiscount)

		r.Get("/coupons", u.coupons)
		r.Post("/coupons", u.createCoupon)
		r.Post("/coupons/{id}/toggle", u.toggleCoupon)

		r.Get("/invoices", u.invoices)
		r.Get("/invoices/{id}", u.invoice)
		r.Post("/invoices/{id}/lines", u.addLine)
		r.Post("/invoices/{id}/finalize", u.finalize)
		r.Post("/invoices/{id}/void", u.void)
		r.Post("/invoices/{id}/uncollectible", u.uncollectible)
		r.Post("/invoices/{id}/pay", u.startPayment)
		r.Post("/invoices/{id}/manual", u.recordManual)

		r.Get("/payments", u.payments)
		r.Post("/payments/{id}/mock/{outcome}", u.mockOutcome)
		r.Post("/payments/{id}/confirm", u.confirmPayment)
		r.Post("/payments/{id}/cancel", u.cancelPayment)
		r.Post("/payments/{id}/refund", u.refund)
		r.Post("/payments/{id}/retry-provider-cancel", u.retryProviderCancel)

		r.Get("/ledger", u.ledger)
		r.Get("/ledger/accounts/{id}", u.ledgerAccount)

		r.Get("/keys", u.keys)
		r.Post("/keys", u.createKey)
		r.Post("/keys/{id}/revoke", u.revokeKey)

		r.Get("/webhooks", u.webhooks)
		r.Post("/webhooks", u.createWebhook)
		r.Get("/webhooks/{id}", u.webhook)
		r.Post("/webhooks/{id}/toggle", u.toggleWebhook)
		r.Post("/webhooks/{id}/delete", u.deleteWebhook)
		r.Post("/deliveries/{id}/retry", u.retryDelivery)
		r.Get("/events", u.events)

		r.Get("/settings", u.settingsPage)
		r.Post("/providers/{name}", u.configureProvider)
		r.Post("/settings", u.saveSettings)
	})
}

// ---- auth ------------------------------------------------------------------------

func (u *UI) loginPage(w http.ResponseWriter, r *http.Request) {
	u.render(w, r, "login", "Log in", "", nil)
}

func (u *UI) login(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.FormValue("api_key"))
	if _, err := u.auth.Authenticate(r.Context(), key); err != nil {
		u.flash(w, "error", "That API key was not accepted.")
		http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
		return
	}
	v, err := u.seal(session{Key: key, Expires: time.Now().Add(sessionTTL)})
	if err != nil {
		http.Error(w, "could not start a session", 500)
		return
	}
	u.setCookie(w, sessionCookie, v, int(sessionTTL.Seconds()))
	u.setCookie(w, tenantCookie, "", -1)
	http.Redirect(w, r, "/ui/", http.StatusSeeOther)
}

func (u *UI) logout(w http.ResponseWriter, r *http.Request) {
	u.setCookie(w, sessionCookie, "", -1)
	u.setCookie(w, tenantCookie, "", -1)
	http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
}

func (u *UI) switchTenant(w http.ResponseWriter, r *http.Request) {
	if !rcFrom(r).P.IsPlatform() {
		u.fail(w, r, http.StatusForbidden, "Only platform keys can switch tenants.")
		return
	}
	id, err := uuid.Parse(r.FormValue("tenant_id"))
	if err != nil {
		u.done(w, r, "/ui/tenants", nil, "")
		return
	}
	u.setCookie(w, tenantCookie, id.String(), int(sessionTTL.Seconds()))
	http.Redirect(w, r, "/ui/", http.StatusSeeOther)
}

func (u *UI) search(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok {
		return
	}
	p := rcFrom(r).P
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	d := map[string]any{"Q": q}
	if len(q) >= 2 {
		res, err := u.svc.Search(r.Context(), tid, q, billing.SearchIn{
			Customers: p.Has(auth.ScopeCustomersRead), Invoices: p.Has(auth.ScopeInvoicesRead),
			Payments: p.Has(auth.ScopePaymentsRead), Subscriptions: p.Has(auth.ScopeSubscriptionsRead),
		})
		if err != nil {
			u.getErr(w, r, err)
			return
		}
		d["R"] = res
		d["Empty"] = len(res.Customers)+len(res.Invoices)+len(res.Payments)+len(res.Subscriptions) == 0
	}
	u.render(w, r, "search", "Search", "", d)
}

// ---- dashboard & tenants ----------------------------------------------------------

func (u *UI) dashboard(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok {
		return
	}
	d := map[string]any{"Setup": u.setupSteps(r, tid)}
	if rcFrom(r).P.Has(auth.ScopeInvoicesRead) {
		st, err := u.svc.TenantStats(r.Context(), tid)
		if err != nil {
			u.getErr(w, r, err)
			return
		}
		d["Stats"] = st
		open, _ := u.svc.ListInvoices(r.Context(), tid, billing.InvoiceFilter{Status: "open"}, billing.Page{Limit: 10})
		d["OpenInvoices"] = open.Data
	}
	if rcFrom(r).P.Has(auth.ScopeSubscriptionsRead) {
		pd, _ := u.svc.ListSubscriptions(r.Context(), tid, billing.SubscriptionFilter{Status: "past_due"}, billing.Page{Limit: 10})
		d["PastDue"] = pd.Data
	}
	u.render(w, r, "dashboard", "Dashboard", "dashboard", d)
}

func (u *UI) tenants(w http.ResponseWriter, r *http.Request) {
	if !rcFrom(r).P.IsPlatform() {
		u.fail(w, r, http.StatusForbidden, "Tenants are managed with a platform key.")
		return
	}
	u.render(w, r, "tenants", "Tenants", "tenants", map[string]any{"Currencies": currencyCodes()})
}

func (u *UI) createTenant(w http.ResponseWriter, r *http.Request) {
	if !rcFrom(r).P.IsPlatform() || !rcFrom(r).P.Has(auth.ScopeAdmin) {
		u.fail(w, r, http.StatusForbidden, "Only platform admin keys can create tenants.")
		return
	}
	out, err := u.svc.CreateTenant(r.Context(), billing.CreateTenantInput{
		Slug: r.FormValue("slug"), Name: r.FormValue("name"), DefaultCurrency: r.FormValue("currency"),
		CreateAdminKey: r.FormValue("admin_key") == "on",
	})
	if err != nil {
		u.done(w, r, "/ui/tenants", err, "")
		return
	}
	u.setCookie(w, tenantCookie, out.Tenant.ID.String(), int(sessionTTL.Seconds()))
	if out.APIKey != nil {
		u.render(w, r, "key_created", "Tenant created", "tenants", map[string]any{
			"Token": out.APIKey.Token, "Key": out.APIKey.Key, "Context": "Admin key for tenant " + out.Tenant.Name,
		})
		return
	}
	u.done(w, r, "/ui/", nil, "Tenant "+out.Tenant.Name+" created.")
}

func (u *UI) runEngine(w http.ResponseWriter, r *http.Request) {
	if !rcFrom(r).P.IsPlatform() {
		u.fail(w, r, http.StatusForbidden, "Only platform keys can run the engine.")
		return
	}
	rep, err := u.svc.RunEngine(r.Context())
	u.done(w, r, "/ui/", err, "Engine ran: "+strings.Trim(strings.ReplaceAll(jsonCompact(rep), `"`, ""), "{}"))
}

// ---- customers ------------------------------------------------------------------------

func (u *UI) customers(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok || !u.can(w, r, auth.ScopeCustomersRead) {
		return
	}
	p := pageOf(r)
	list, err := u.svc.ListCustomers(r.Context(), tid, billing.CustomerFilter{Query: r.URL.Query().Get("q")}, p)
	if err != nil {
		u.getErr(w, r, err)
		return
	}
	u.render(w, r, "customers", "Customers", "customers", map[string]any{"List": list, "Q": r.URL.Query().Get("q"), "Next": nextCursor(list)})
}

func (u *UI) createCustomer(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok || !u.can(w, r, auth.ScopeCustomersWrite) {
		return
	}
	c, err := u.svc.CreateCustomer(r.Context(), tid, billing.CustomerInput{
		Name: opt(r, "name"), Email: opt(r, "email"), Phone: opt(r, "phone"), ExternalID: opt(r, "external_id"),
	})
	if err != nil {
		u.done(w, r, "/ui/customers", err, "")
		return
	}
	u.done(w, r, "/ui/customers/"+c.ID.String(), nil, "Customer created.")
}

func (u *UI) customer(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok || !u.can(w, r, auth.ScopeCustomersRead) {
		return
	}
	id, err := idParam(r, "id")
	if err != nil {
		u.fail(w, r, 404, "Not found")
		return
	}
	c, err := u.svc.Customer(r.Context(), tid, id)
	if err != nil {
		u.getErr(w, r, err)
		return
	}
	d := map[string]any{"C": c}
	d["Balances"], _ = u.svc.CustomerBalances(r.Context(), tid, id)
	d["Entitlements"], _ = u.svc.Entitlements(r.Context(), tid, id)
	p := rcFrom(r).P
	if p.Has(auth.ScopeSubscriptionsRead) {
		subs, _ := u.svc.ListSubscriptions(r.Context(), tid, billing.SubscriptionFilter{CustomerID: &id}, billing.Page{Limit: 50})
		d["Subs"] = subs.Data
	}
	if p.Has(auth.ScopeInvoicesRead) {
		invs, _ := u.svc.ListInvoices(r.Context(), tid, billing.InvoiceFilter{CustomerID: &id}, billing.Page{Limit: 50})
		d["Invoices"] = invs.Data
	}
	if p.Has(auth.ScopePaymentsRead) {
		pays, _ := u.svc.ListPayments(r.Context(), tid, billing.PaymentFilter{CustomerID: &id}, billing.Page{Limit: 50})
		d["Payments"] = pays.Data
	}
	if p.Has(auth.ScopePlansRead) {
		plans, _ := u.svc.ListPlans(r.Context(), tid, true, billing.Page{Limit: 100})
		d["Plans"] = plans.Data
	}
	d["Currency"] = rcFrom(r).Tenant.DefaultCurrency
	u.render(w, r, "customer", c.Name, "customers", d)
}

func (u *UI) updateCustomer(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok || !u.can(w, r, auth.ScopeCustomersWrite) {
		return
	}
	id, _ := idParam(r, "id")
	exempt := r.FormValue("tax_exempt") == "on"
	rate := -1
	if raw := strings.TrimSpace(r.FormValue("tax_rate")); raw != "" {
		bps, err := parsePercent(raw)
		if err != nil {
			u.done(w, r, "/ui/customers/"+id.String(), &billing.Error{Status: 422, Code: "invalid_tax_rate", Message: err.Error()}, "")
			return
		}
		rate = bps
	}
	_, err := u.svc.UpdateCustomer(r.Context(), tid, id, billing.CustomerInput{
		Name: opt(r, "name"), Email: optRaw(r, "email"), Phone: optRaw(r, "phone"), ExternalID: optRaw(r, "external_id"),
		TaxExempt: &exempt, TaxRateBps: &rate,
	})
	u.done(w, r, "/ui/customers/"+id.String(), err, "Customer saved.")
}

func (u *UI) grantCredit(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok || !u.can(w, r, auth.ScopeCustomersWrite) {
		return
	}
	id, _ := idParam(r, "id")
	cur := r.FormValue("currency")
	amt, err := domain.ParseAmount(r.FormValue("amount"), cur)
	if err == nil {
		_, err = u.svc.GrantCredit(r.Context(), tid, id, billing.GrantCreditInput{Amount: amt, Currency: cur, Reason: r.FormValue("reason")})
	} else {
		err = &billing.Error{Status: 422, Code: "invalid_amount", Message: err.Error()}
	}
	u.done(w, r, "/ui/customers/"+id.String(), err, "Credit granted.")
}

func (u *UI) subscribe(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok || !u.can(w, r, auth.ScopeSubscriptionsWrite) {
		return
	}
	id, _ := idParam(r, "id")
	priceID, err := uuid.Parse(r.FormValue("price_id"))
	if err != nil {
		u.done(w, r, "/ui/customers/"+id.String(), &billing.Error{Status: 422, Code: "price_required", Message: "choose a price"}, "")
		return
	}
	in := billing.CreateSubscriptionInput{CustomerID: id, PriceID: &priceID, PromoCode: r.FormValue("promo_code")}
	if n, err := strconv.Atoi(r.FormValue("quantity")); err == nil {
		in.Quantity = n
	}
	if v := strings.TrimSpace(r.FormValue("trial_days")); v != "" {
		n, _ := strconv.Atoi(v)
		in.TrialDays = &n
	}
	sub, err := u.svc.CreateSubscription(r.Context(), tid, in)
	if err != nil {
		u.done(w, r, "/ui/customers/"+id.String(), err, "")
		return
	}
	u.done(w, r, "/ui/subscriptions/"+sub.ID.String(), nil, "Subscription created.")
}

func (u *UI) createInvoice(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok || !u.can(w, r, auth.ScopeInvoicesWrite) {
		return
	}
	id, _ := idParam(r, "id")
	cur := r.FormValue("currency")
	amt, err := domain.ParseAmount(r.FormValue("amount"), cur)
	if err != nil {
		u.done(w, r, "/ui/customers/"+id.String(), &billing.Error{Status: 422, Code: "invalid_amount", Message: err.Error()}, "")
		return
	}
	qty, _ := strconv.ParseInt(r.FormValue("quantity"), 10, 64)
	inv, err := u.svc.CreateInvoice(r.Context(), tid, billing.InvoiceInput{
		CustomerID: id, Currency: cur, Memo: r.FormValue("memo"), Finalize: r.FormValue("finalize") == "on",
		PromoCode: r.FormValue("promo_code"),
		Lines:     []billing.LineInput{{Description: r.FormValue("description"), Quantity: qty, UnitAmount: amt}},
	})
	if err != nil {
		u.done(w, r, "/ui/customers/"+id.String(), err, "")
		return
	}
	u.done(w, r, "/ui/invoices/"+inv.ID.String(), nil, "Invoice created.")
}

// ---- plans ----------------------------------------------------------------------------

func (u *UI) plans(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok || !u.can(w, r, auth.ScopePlansRead) {
		return
	}
	list, err := u.svc.ListPlans(r.Context(), tid, false, pageOf(r))
	if err != nil {
		u.getErr(w, r, err)
		return
	}
	u.render(w, r, "plans", "Plans", "plans", map[string]any{"List": list, "Currency": rcFrom(r).Tenant.DefaultCurrency, "Next": nextCursor(list)})
}

func (u *UI) createPlan(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok || !u.can(w, r, auth.ScopePlansWrite) {
		return
	}
	in := billing.PlanInput{Code: r.FormValue("code"), Name: r.FormValue("name"), Description: r.FormValue("description"), Kind: r.FormValue("kind")}
	if pi, err := priceFromForm(r); err != nil {
		u.done(w, r, "/ui/plans", err, "")
		return
	} else if pi != nil {
		in.Prices = []billing.PriceInput{*pi}
	}
	p, err := u.svc.CreatePlan(r.Context(), tid, in)
	if err != nil {
		u.done(w, r, "/ui/plans", err, "")
		return
	}
	u.done(w, r, "/ui/plans/"+p.ID.String(), nil, "Plan created.")
}

func priceFromForm(r *http.Request) (*billing.PriceInput, error) {
	raw := strings.TrimSpace(r.FormValue("amount"))
	if raw == "" {
		return nil, nil
	}
	cur := r.FormValue("currency")
	amt, err := domain.ParseAmount(raw, cur)
	if err != nil {
		return nil, &billing.Error{Status: 422, Code: "invalid_amount", Message: err.Error()}
	}
	count, _ := strconv.Atoi(r.FormValue("interval_count"))
	trial, _ := strconv.Atoi(r.FormValue("trial_days"))
	return &billing.PriceInput{
		Nickname: r.FormValue("nickname"), Amount: amt, Currency: cur, Interval: r.FormValue("interval"),
		IntervalCount: count, TrialDays: trial,
	}, nil
}

func (u *UI) plan(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok || !u.can(w, r, auth.ScopePlansRead) {
		return
	}
	id, err := idParam(r, "id")
	if err != nil {
		u.fail(w, r, 404, "Not found")
		return
	}
	p, err := u.svc.Plan(r.Context(), tid, id)
	if err != nil {
		u.getErr(w, r, err)
		return
	}
	d := map[string]any{"Plan": p, "Currency": rcFrom(r).Tenant.DefaultCurrency}
	if rcFrom(r).P.Has(auth.ScopeSubscriptionsRead) {
		subs, _ := u.svc.ListSubscriptions(r.Context(), tid, billing.SubscriptionFilter{PlanID: &id}, billing.Page{Limit: 50})
		d["Subs"] = subs.Data
	}
	u.render(w, r, "plan", p.Name, "plans", d)
}

func (u *UI) updatePlan(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok || !u.can(w, r, auth.ScopePlansWrite) {
		return
	}
	id, _ := idParam(r, "id")
	active := r.FormValue("active") == "on"
	_, err := u.svc.UpdatePlan(r.Context(), tid, id, billing.UpdatePlanInput{
		Name: opt(r, "name"), Description: optRaw(r, "description"), Active: &active,
	})
	u.done(w, r, "/ui/plans/"+id.String(), err, "Plan saved.")
}

func (u *UI) addPrice(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok || !u.can(w, r, auth.ScopePlansWrite) {
		return
	}
	id, _ := idParam(r, "id")
	pi, err := priceFromForm(r)
	if err == nil && pi == nil {
		err = &billing.Error{Status: 422, Code: "invalid_amount", Message: "amount is required"}
	}
	if err == nil {
		_, err = u.svc.AddPrice(r.Context(), tid, id, *pi)
	}
	u.done(w, r, "/ui/plans/"+id.String(), err, "Price added.")
}

func (u *UI) togglePrice(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok || !u.can(w, r, auth.ScopePlansWrite) {
		return
	}
	id, _ := idParam(r, "id")
	pr, err := u.svc.SetPriceActive(r.Context(), tid, id, r.FormValue("active") == "true")
	to := "/ui/plans"
	if pr != nil {
		to += "/" + pr.PlanID.String()
	}
	u.done(w, r, to, err, "Price updated.")
}

// ---- subscriptions --------------------------------------------------------------------

func (u *UI) subscriptions(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok || !u.can(w, r, auth.ScopeSubscriptionsRead) {
		return
	}
	status := r.URL.Query().Get("status")
	list, err := u.svc.ListSubscriptions(r.Context(), tid, billing.SubscriptionFilter{Status: status}, pageOf(r))
	if err != nil {
		u.getErr(w, r, err)
		return
	}
	u.render(w, r, "subscriptions", "Subscriptions", "subscriptions", map[string]any{
		"List": list, "Status": status, "Next": nextSubCursor(list), "Names": u.customerNames(r, tid, subCustomerIDs(list.Data)),
		"Statuses": []string{"incomplete", "trialing", "active", "past_due", "canceled", "expired"},
	})
}

func (u *UI) subscription(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok || !u.can(w, r, auth.ScopeSubscriptionsRead) {
		return
	}
	id, err := idParam(r, "id")
	if err != nil {
		u.fail(w, r, 404, "Not found")
		return
	}
	sub, err := u.svc.Subscription(r.Context(), tid, id)
	if err != nil {
		u.getErr(w, r, err)
		return
	}
	d := map[string]any{"S": sub}
	d["Customer"], _ = u.svc.Customer(r.Context(), tid, sub.CustomerID)
	d["Price"], _ = u.svc.Price(r.Context(), tid, sub.PriceID)
	if sub.PendingPriceID != nil {
		d["PendingPrice"], _ = u.svc.Price(r.Context(), tid, *sub.PendingPriceID)
	}
	if rcFrom(r).P.Has(auth.ScopeInvoicesRead) {
		invs, _ := u.svc.ListInvoices(r.Context(), tid, billing.InvoiceFilter{SubscriptionID: &id}, billing.Page{Limit: 50})
		d["Invoices"] = invs.Data
	}
	if rcFrom(r).P.Has(auth.ScopePlansRead) {
		plans, _ := u.svc.ListPlans(r.Context(), tid, true, billing.Page{Limit: 100})
		d["Plans"] = plans.Data
	}
	u.render(w, r, "subscription", "Subscription", "subscriptions", d)
}

func (u *UI) setQuantity(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok || !u.can(w, r, auth.ScopeSubscriptionsWrite) {
		return
	}
	id, _ := idParam(r, "id")
	n, err := strconv.Atoi(r.FormValue("quantity"))
	if err == nil {
		_, err = u.svc.SetQuantity(r.Context(), tid, id, n)
	} else {
		err = &billing.Error{Status: 422, Code: "invalid_quantity", Message: "enter a whole number of seats"}
	}
	u.done(w, r, "/ui/subscriptions/"+id.String(), err, "Seats updated.")
}

func (u *UI) setAddOn(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok || !u.can(w, r, auth.ScopeSubscriptionsWrite) {
		return
	}
	id, _ := idParam(r, "id")
	priceID, err := uuid.Parse(r.FormValue("price_id"))
	if err != nil {
		u.done(w, r, "/ui/subscriptions/"+id.String(), &billing.Error{Status: 422, Code: "price_required", Message: "choose an add-on"}, "")
		return
	}
	n, err := strconv.Atoi(r.FormValue("quantity"))
	if err != nil {
		n = 1
	}
	_, err = u.svc.SetAddOn(r.Context(), tid, id, billing.AddOnInput{PriceID: priceID, Quantity: n})
	msg := "Add-on updated."
	if n == 0 {
		msg = "Add-on will be removed at the end of the period."
	}
	u.done(w, r, "/ui/subscriptions/"+id.String(), err, msg)
}

func (u *UI) applyPromo(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok || !u.can(w, r, auth.ScopeSubscriptionsWrite) {
		return
	}
	id, _ := idParam(r, "id")
	_, err := u.svc.ApplyPromoCode(r.Context(), tid, id, r.FormValue("promo_code"))
	u.done(w, r, "/ui/subscriptions/"+id.String(), err, "Discount applied.")
}

func (u *UI) removeDiscount(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok || !u.can(w, r, auth.ScopeSubscriptionsWrite) {
		return
	}
	id, _ := idParam(r, "id")
	_, err := u.svc.RemoveDiscount(r.Context(), tid, id)
	u.done(w, r, "/ui/subscriptions/"+id.String(), err, "Discount removed.")
}

// ---- coupons ---------------------------------------------------------------------

func (u *UI) coupons(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok || !u.can(w, r, auth.ScopeCouponsRead) {
		return
	}
	list, err := u.svc.ListCoupons(r.Context(), tid, billing.Page{Limit: 100})
	if err != nil {
		u.getErr(w, r, err)
		return
	}
	d := map[string]any{"List": list.Data, "Currency": rcFrom(r).Tenant.DefaultCurrency}
	if rcFrom(r).P.Has(auth.ScopePlansRead) {
		plans, _ := u.svc.ListPlans(r.Context(), tid, true, billing.Page{Limit: 100})
		d["Plans"] = plans.Data
	}
	u.render(w, r, "coupons", "Coupons", "coupons", d)
}

func (u *UI) createCoupon(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok || !u.can(w, r, auth.ScopeCouponsWrite) {
		return
	}
	r.ParseForm()
	bad := func(msg string) {
		u.done(w, r, "/ui/coupons", &billing.Error{Status: 422, Code: "invalid_coupon", Message: msg}, "")
	}
	in := billing.CouponInput{Code: r.FormValue("code"), Name: r.FormValue("name"), Duration: r.FormValue("duration"), PlanIDs: r.Form["plan_ids"]}
	switch r.FormValue("kind") {
	case "amount":
		cur := r.FormValue("currency")
		amt, err := domain.ParseAmount(r.FormValue("value"), cur)
		if err != nil {
			bad(err.Error())
			return
		}
		in.AmountOff, in.Currency = &amt, cur
	default:
		pct, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(r.FormValue("value")), "%"))
		if err != nil {
			bad("enter a whole percent, like 20")
			return
		}
		in.PercentOff = &pct
	}
	if in.Duration == "repeating" {
		n, err := strconv.Atoi(r.FormValue("periods"))
		if err != nil {
			bad("say for how many billing periods")
			return
		}
		in.DurationPeriods = &n
	}
	if v := strings.TrimSpace(r.FormValue("max_redemptions")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			bad("usage limit must be a whole number")
			return
		}
		in.MaxRedemptions = &n
	}
	if v := strings.TrimSpace(r.FormValue("redeem_by")); v != "" {
		t, err := time.Parse("2006-01-02", v)
		if err != nil {
			bad("expiry must be a date")
			return
		}
		end := t.Add(24*time.Hour - time.Second)
		in.RedeemBy = &end
	}
	_, err := u.svc.CreateCoupon(r.Context(), tid, in)
	u.done(w, r, "/ui/coupons", err, "Coupon created. Customers can now use the code.")
}

func (u *UI) toggleCoupon(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok || !u.can(w, r, auth.ScopeCouponsWrite) {
		return
	}
	id, _ := idParam(r, "id")
	active := r.FormValue("active") == "true"
	_, err := u.svc.UpdateCoupon(r.Context(), tid, id, billing.UpdateCouponInput{Active: &active})
	u.done(w, r, "/ui/coupons", err, "Coupon updated.")
}

func (u *UI) cancelSubscription(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok || !u.can(w, r, auth.ScopeSubscriptionsWrite) {
		return
	}
	id, _ := idParam(r, "id")
	_, err := u.svc.CancelSubscription(r.Context(), tid, id, r.FormValue("when") != "now")
	u.done(w, r, "/ui/subscriptions/"+id.String(), err, "Subscription updated.")
}

func (u *UI) resumeSubscription(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok || !u.can(w, r, auth.ScopeSubscriptionsWrite) {
		return
	}
	id, _ := idParam(r, "id")
	_, err := u.svc.ResumeSubscription(r.Context(), tid, id)
	u.done(w, r, "/ui/subscriptions/"+id.String(), err, "Subscription resumed.")
}

func (u *UI) changePrice(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok || !u.can(w, r, auth.ScopeSubscriptionsWrite) {
		return
	}
	id, _ := idParam(r, "id")
	priceID, err := uuid.Parse(r.FormValue("price_id"))
	if err == nil {
		_, err = u.svc.ChangePrice(r.Context(), tid, id, billing.ChangePriceInput{PriceID: priceID, When: r.FormValue("when")})
	} else {
		err = &billing.Error{Status: 422, Code: "price_required", Message: "choose a price"}
	}
	u.done(w, r, "/ui/subscriptions/"+id.String(), err, "Price change applied.")
}

// ---- invoices ---------------------------------------------------------------------------

func (u *UI) invoices(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok || !u.can(w, r, auth.ScopeInvoicesRead) {
		return
	}
	status := r.URL.Query().Get("status")
	list, err := u.svc.ListInvoices(r.Context(), tid, billing.InvoiceFilter{Status: status}, pageOf(r))
	if err != nil {
		u.getErr(w, r, err)
		return
	}
	ids := make([]uuid.UUID, len(list.Data))
	for i, inv := range list.Data {
		ids[i] = inv.CustomerID
	}
	u.render(w, r, "invoices", "Invoices", "invoices", map[string]any{
		"List": list, "Status": status, "Next": nextCursor(list), "Names": u.customerNames(r, tid, ids),
		"Statuses": []string{"draft", "open", "paid", "void", "uncollectible"},
	})
}

func (u *UI) invoice(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok || !u.can(w, r, auth.ScopeInvoicesRead) {
		return
	}
	id, err := idParam(r, "id")
	if err != nil {
		u.fail(w, r, 404, "Not found")
		return
	}
	inv, err := u.svc.Invoice(r.Context(), tid, id)
	if err != nil {
		u.getErr(w, r, err)
		return
	}
	d := map[string]any{"I": inv, "Providers": u.svc.Providers().Names()}
	d["Customer"], _ = u.svc.Customer(r.Context(), tid, inv.CustomerID)
	if rcFrom(r).P.Has(auth.ScopePaymentsRead) {
		pays, _ := u.svc.ListPayments(r.Context(), tid, billing.PaymentFilter{InvoiceID: &id}, billing.Page{Limit: 50})
		d["Payments"] = pays.Data
	}
	if rcFrom(r).P.Has(auth.ScopeLedgerRead) {
		txs, _ := u.svc.ListLedgerTransactions(r.Context(), tid, billing.LedgerTransactionFilter{InvoiceID: &id}, billing.Page{Limit: 50})
		// An invoice's postings read as a story: oldest first.
		for i, j := 0, len(txs.Data)-1; i < j; i, j = i+1, j-1 {
			txs.Data[i], txs.Data[j] = txs.Data[j], txs.Data[i]
		}
		d["Postings"] = txs.Data
		d["AccountNames"] = u.accountNames(r, tid, txs.Data)
	}
	title := "Draft invoice"
	if inv.Number != nil {
		title = "Invoice " + *inv.Number
	}
	u.render(w, r, "invoice", title, "invoices", d)
}

func (u *UI) addLine(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok || !u.can(w, r, auth.ScopeInvoicesWrite) {
		return
	}
	id, _ := idParam(r, "id")
	inv, err := u.svc.Invoice(r.Context(), tid, id)
	if err != nil {
		u.done(w, r, "/ui/invoices", err, "")
		return
	}
	amt, err := domain.ParseAmount(r.FormValue("amount"), inv.Currency)
	if err != nil {
		u.done(w, r, "/ui/invoices/"+id.String(), &billing.Error{Status: 422, Code: "invalid_amount", Message: err.Error()}, "")
		return
	}
	qty, _ := strconv.ParseInt(r.FormValue("quantity"), 10, 64)
	_, err = u.svc.AddInvoiceLine(r.Context(), tid, id, billing.LineInput{Description: r.FormValue("description"), Quantity: qty, UnitAmount: amt})
	u.done(w, r, "/ui/invoices/"+id.String(), err, "Line added.")
}

func (u *UI) finalize(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok || !u.can(w, r, auth.ScopeInvoicesWrite) {
		return
	}
	id, _ := idParam(r, "id")
	_, err := u.svc.FinalizeInvoice(r.Context(), tid, id)
	u.done(w, r, "/ui/invoices/"+id.String(), err, "Invoice finalized.")
}

func (u *UI) void(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok || !u.can(w, r, auth.ScopeInvoicesWrite) {
		return
	}
	id, _ := idParam(r, "id")
	_, err := u.svc.VoidInvoice(r.Context(), tid, id, r.FormValue("reason"))
	u.done(w, r, "/ui/invoices/"+id.String(), err, "Invoice voided.")
}

func (u *UI) uncollectible(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok || !u.can(w, r, auth.ScopeInvoicesWrite) {
		return
	}
	id, _ := idParam(r, "id")
	_, err := u.svc.MarkUncollectible(r.Context(), tid, id)
	u.done(w, r, "/ui/invoices/"+id.String(), err, "Invoice written off.")
}

func (u *UI) startPayment(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok || !u.can(w, r, auth.ScopePaymentsWrite) {
		return
	}
	id, _ := idParam(r, "id")
	_, err := u.svc.StartPayment(r.Context(), tid, id, billing.StartPaymentInput{Provider: r.FormValue("provider")}, keyIDOf(rcFrom(r).P))
	u.done(w, r, "/ui/invoices/"+id.String(), err, "Payment started.")
}

func (u *UI) recordManual(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok || !u.can(w, r, auth.ScopePaymentsManual) {
		return
	}
	id, _ := idParam(r, "id")
	inv, err := u.svc.Invoice(r.Context(), tid, id)
	if err != nil {
		u.done(w, r, "/ui/invoices", err, "")
		return
	}
	amt, err := domain.ParseAmount(r.FormValue("amount"), inv.Currency)
	if err != nil {
		u.done(w, r, "/ui/invoices/"+id.String(), &billing.Error{Status: 422, Code: "invalid_amount", Message: err.Error()}, "")
		return
	}
	_, err = u.svc.RecordManualPayment(r.Context(), tid, billing.ManualPaymentInput{
		InvoiceID: id, Amount: amt, Reference: r.FormValue("reference"), Note: r.FormValue("note"),
	}, keyIDOf(rcFrom(r).P))
	u.done(w, r, "/ui/invoices/"+id.String(), err, "Payment recorded.")
}

// ---- payments ---------------------------------------------------------------------------

func (u *UI) payments(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok || !u.can(w, r, auth.ScopePaymentsRead) {
		return
	}
	q := r.URL.Query()
	list, err := u.svc.ListPayments(r.Context(), tid, billing.PaymentFilter{Status: q.Get("status"), Provider: q.Get("provider")}, pageOf(r))
	if err != nil {
		u.getErr(w, r, err)
		return
	}
	ids := make([]uuid.UUID, len(list.Data))
	for i, p := range list.Data {
		ids[i] = p.CustomerID
	}
	u.render(w, r, "payments", "Payments", "payments", map[string]any{
		"List": list, "Status": q.Get("status"), "Next": nextCursor(list), "Names": u.customerNames(r, tid, ids),
		"Statuses": []string{"pending", "succeeded", "failed", "canceled", "refunded"},
	})
}

func (u *UI) paymentBack(r *http.Request, pay *models.Payment) string {
	if pay != nil {
		return "/ui/invoices/" + pay.InvoiceID.String()
	}
	return "/ui/payments"
}

func (u *UI) mockOutcome(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok || !u.can(w, r, auth.ScopePaymentsWrite) {
		return
	}
	id, _ := idParam(r, "id")
	pay, err := u.svc.MockOutcome(r.Context(), tid, id, chi.URLParam(r, "outcome"))
	u.done(w, r, u.paymentBack(r, pay), err, "Mock payment: "+chi.URLParam(r, "outcome"))
}

func (u *UI) confirmPayment(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok || !u.can(w, r, auth.ScopePaymentsManual) {
		return
	}
	id, _ := idParam(r, "id")
	var amt int64
	if raw := strings.TrimSpace(r.FormValue("amount")); raw != "" {
		pay, err := u.svc.Payment(r.Context(), tid, id)
		if err != nil {
			u.done(w, r, "/ui/payments", err, "")
			return
		}
		if amt, err = domain.ParseAmount(raw, pay.Currency); err != nil {
			u.done(w, r, u.paymentBack(r, pay), &billing.Error{Status: 422, Code: "invalid_amount", Message: err.Error()}, "")
			return
		}
	}
	pay, err := u.svc.ConfirmManualPayment(r.Context(), tid, id, amt, r.FormValue("note"))
	u.done(w, r, u.paymentBack(r, pay), err, "Payment confirmed.")
}

func (u *UI) cancelPayment(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok || !u.can(w, r, auth.ScopePaymentsWrite) {
		return
	}
	id, _ := idParam(r, "id")
	pay, err := u.svc.CancelPayment(r.Context(), tid, id)
	u.done(w, r, u.paymentBack(r, pay), err, "Payment canceled.")
}

func (u *UI) refund(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok || !u.can(w, r, auth.ScopePaymentsWrite) {
		return
	}
	id, _ := idParam(r, "id")
	pay, err := u.svc.RefundPayment(r.Context(), tid, id, r.FormValue("reason"))
	u.done(w, r, u.paymentBack(r, pay), err, "Payment refunded.")
}

func (u *UI) retryProviderCancel(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok || !u.can(w, r, auth.ScopePaymentsWrite) {
		return
	}
	id, _ := idParam(r, "id")
	pay, err := u.svc.RetryProviderCancel(r.Context(), tid, id)
	u.done(w, r, u.paymentBack(r, pay), err, "Provider cancel queued again.")
}

// ---- ledger ------------------------------------------------------------------------------

func (u *UI) ledger(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok || !u.can(w, r, auth.ScopeLedgerRead) {
		return
	}
	accts, err := u.svc.ListLedgerAccounts(r.Context(), tid, billing.LedgerAccountFilter{SystemOnly: r.URL.Query().Get("all") == ""}, billing.Page{Limit: 100})
	if err != nil {
		u.getErr(w, r, err)
		return
	}
	rec, err := u.svc.Reconcile(r.Context(), tid)
	if err != nil {
		u.getErr(w, r, err)
		return
	}
	txs, _ := u.svc.ListLedgerTransactions(r.Context(), tid, billing.LedgerTransactionFilter{}, billing.Page{Limit: 25})
	u.render(w, r, "ledger", "Ledger", "ledger", map[string]any{
		"Accounts": accts.Data, "Rec": rec, "All": r.URL.Query().Get("all") != "",
		"Txs": txs.Data, "AccountNames": u.accountNames(r, tid, txs.Data),
	})
}

func (u *UI) ledgerAccount(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok || !u.can(w, r, auth.ScopeLedgerRead) {
		return
	}
	id, err := idParam(r, "id")
	if err != nil {
		u.fail(w, r, 404, "Not found")
		return
	}
	acc, err := u.svc.LedgerAccount(r.Context(), tid, id)
	if err != nil {
		u.getErr(w, r, err)
		return
	}
	entries, err := u.svc.AccountEntries(r.Context(), tid, id, 200)
	if err != nil {
		u.getErr(w, r, err)
		return
	}
	u.render(w, r, "ledger_account", acc.Name, "ledger", map[string]any{"A": acc, "Entries": entries})
}

// ---- api keys ------------------------------------------------------------------------------

func (u *UI) keys(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok || !u.can(w, r, auth.ScopeKeysRead) {
		return
	}
	list, err := u.svc.ListKeys(r.Context(), &tid, billing.Page{Limit: 100})
	if err != nil {
		u.getErr(w, r, err)
		return
	}
	u.render(w, r, "keys", "API keys", "keys", map[string]any{"List": list.Data, "Scopes": auth.All(), "Describe": auth.Describe()})
}

func (u *UI) createKey(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok || !u.can(w, r, auth.ScopeKeysWrite) {
		return
	}
	r.ParseForm()
	out, err := u.svc.CreateKey(r.Context(), rcFrom(r).P, &tid, billing.CreateKeyInput{
		Name: r.FormValue("name"), Scopes: r.Form["scopes"], ExpiresIn: r.FormValue("expires_in"),
	})
	if err != nil {
		u.done(w, r, "/ui/keys", err, "")
		return
	}
	u.render(w, r, "key_created", "API key created", "keys", map[string]any{"Token": out.Token, "Key": out.Key, "Context": "Key " + out.Key.Name})
}

func (u *UI) revokeKey(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok || !u.can(w, r, auth.ScopeKeysWrite) {
		return
	}
	id, _ := idParam(r, "id")
	if rcFrom(r).P.KeyID == id {
		u.done(w, r, "/ui/keys", &billing.Error{Status: 409, Code: "own_key", Message: "you cannot revoke the key you are logged in with"}, "")
		return
	}
	_, err := u.svc.RevokeKey(r.Context(), &tid, id)
	u.done(w, r, "/ui/keys", err, "Key revoked.")
}

// ---- webhooks -----------------------------------------------------------------------------

func (u *UI) webhooks(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok || !u.can(w, r, auth.ScopeWebhooksRead) {
		return
	}
	list, err := u.svc.ListWebhookEndpoints(r.Context(), tid, billing.Page{Limit: 100})
	if err != nil {
		u.getErr(w, r, err)
		return
	}
	u.render(w, r, "webhooks", "Webhooks", "webhooks", map[string]any{"List": list.Data, "Types": events.Types()})
}

func (u *UI) createWebhook(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok || !u.can(w, r, auth.ScopeWebhooksWrite) {
		return
	}
	r.ParseForm()
	out, err := u.svc.CreateWebhookEndpoint(r.Context(), tid, billing.WebhookEndpointInput{
		URL: opt(r, "url"), Description: optRaw(r, "description"), EventTypes: r.Form["event_types"],
	})
	if err != nil {
		u.done(w, r, "/ui/webhooks", err, "")
		return
	}
	u.render(w, r, "key_created", "Webhook endpoint created", "webhooks", map[string]any{
		"Token": out.Secret, "Context": "Signing secret for " + out.Endpoint.URL, "Secret": true,
	})
}

func (u *UI) webhook(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok || !u.can(w, r, auth.ScopeWebhooksRead) {
		return
	}
	id, err := idParam(r, "id")
	if err != nil {
		u.fail(w, r, 404, "Not found")
		return
	}
	list, err := u.svc.ListWebhookEndpoints(r.Context(), tid, billing.Page{Limit: 100})
	if err != nil {
		u.getErr(w, r, err)
		return
	}
	var ep *models.WebhookEndpoint
	for i := range list.Data {
		if list.Data[i].ID == id {
			ep = &list.Data[i]
		}
	}
	if ep == nil {
		u.fail(w, r, 404, "Webhook endpoint not found")
		return
	}
	dels, _ := u.svc.ListDeliveries(r.Context(), tid, &id, billing.Page{Limit: 100})
	u.render(w, r, "webhook", ep.URL, "webhooks", map[string]any{"E": ep, "Deliveries": dels.Data})
}

func (u *UI) toggleWebhook(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok || !u.can(w, r, auth.ScopeWebhooksWrite) {
		return
	}
	id, _ := idParam(r, "id")
	enabled := r.FormValue("enabled") == "true"
	_, err := u.svc.UpdateWebhookEndpoint(r.Context(), tid, id, billing.WebhookEndpointInput{Enabled: &enabled})
	u.done(w, r, "/ui/webhooks/"+id.String(), err, "Endpoint updated.")
}

func (u *UI) deleteWebhook(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok || !u.can(w, r, auth.ScopeWebhooksWrite) {
		return
	}
	id, _ := idParam(r, "id")
	err := u.svc.DeleteWebhookEndpoint(r.Context(), tid, id)
	u.done(w, r, "/ui/webhooks", err, "Endpoint deleted.")
}

func (u *UI) retryDelivery(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok || !u.can(w, r, auth.ScopeWebhooksWrite) {
		return
	}
	id, _ := idParam(r, "id")
	d, err := u.svc.RetryDelivery(r.Context(), tid, id)
	to := "/ui/webhooks"
	if d != nil {
		to += "/" + d.EndpointID.String()
	}
	u.done(w, r, to, err, "Delivery queued.")
}

func (u *UI) events(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok || !u.can(w, r, auth.ScopeWebhooksRead) {
		return
	}
	t := r.URL.Query().Get("type")
	list, err := u.svc.Events(r.Context(), tid, t, pageOf(r))
	if err != nil {
		u.getErr(w, r, err)
		return
	}
	u.render(w, r, "events", "Events", "events", map[string]any{"List": list, "Type": t, "Types": events.Types(), "Next": nextCursor(list)})
}

// ---- settings ------------------------------------------------------------------------------

func (u *UI) settingsPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := u.tenant(w, r); !ok {
		return
	}
	t := rcFrom(r).Tenant
	provs, err := u.svc.PaymentProviders(r.Context(), t.ID)
	if err != nil {
		u.getErr(w, r, err)
		return
	}
	u.render(w, r, "settings", "Settings", "settings", map[string]any{
		"S": t.EffectiveSettings(), "Raw": t.Settings.Data(), "Providers": provs, "PublicURL": u.cfg.PublicURL,
	})
}

func (u *UI) configureProvider(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok || !u.can(w, r, auth.ScopeAdmin) {
		return
	}
	name := chi.URLParam(r, "name")
	if r.FormValue("disconnect") == "yes" {
		err := u.svc.DisconnectProvider(r.Context(), tid, name)
		u.done(w, r, "/ui/settings", err, "Disconnected "+name+".")
		return
	}
	r.ParseForm()
	cfg := map[string]string{}
	for k, v := range r.PostForm {
		if key, ok := strings.CutPrefix(k, "cfg_"); ok && len(v) > 0 {
			cfg[key] = v[0]
		}
	}
	enabled := r.FormValue("enabled") == "on"
	_, err := u.svc.ConfigureProvider(r.Context(), tid, name, billing.ProviderAccountInput{Mode: r.FormValue("mode"), Enabled: &enabled, Config: cfg})
	u.done(w, r, "/ui/settings", err, "Saved "+name+" settings.")
}

func (u *UI) saveSettings(w http.ResponseWriter, r *http.Request) {
	tid, ok := u.tenant(w, r)
	if !ok || !u.can(w, r, auth.ScopeAdmin) {
		return
	}
	atoi := func(k string) int { n, _ := strconv.Atoi(r.FormValue(k)); return n }
	grace := atoi("grace_days")
	s := models.TenantSettings{
		InvoicePrefix:  strings.ToUpper(strings.TrimSpace(r.FormValue("invoice_prefix"))),
		InvoiceDueDays: atoi("invoice_due_days"), RenewalLeadDays: atoi("renewal_lead_days"), GraceDays: &grace,
		IncompleteExpiryHours: atoi("incomplete_expiry_hours"), PaymentExpiryHours: atoi("payment_expiry_hours"),
		LapseAction: r.FormValue("lapse_action"), MockEnabled: r.FormValue("mock_enabled") == "on",
		ManualInstructions: r.FormValue("manual_instructions"),
	}
	if r.FormValue("tax_enabled") == "on" {
		bps, err := parsePercent(r.FormValue("tax_rate"))
		if err != nil {
			u.done(w, r, "/ui/settings", &billing.Error{Status: 422, Code: "invalid_tax_rate", Message: err.Error()}, "")
			return
		}
		s.Tax = &models.TaxSettings{Enabled: true, Name: strings.TrimSpace(r.FormValue("tax_name")), RateBps: bps,
			Inclusive: r.FormValue("tax_inclusive") == "inclusive"}
	}
	name := r.FormValue("name")
	cur := r.FormValue("default_currency")
	_, err := u.svc.UpdateTenant(r.Context(), tid, billing.UpdateTenantInput{Name: &name, DefaultCurrency: &cur, Settings: &s})
	u.done(w, r, "/ui/settings", err, "Settings saved.")
}

// ---- public mock checkout -------------------------------------------------------------------

func (u *UI) mockCheckout(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r, "paymentID")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	pay, inv, t, err := u.svc.PaymentForCheckout(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	u.render(w, r, "checkout", "Test checkout", "", map[string]any{"Pay": pay, "Inv": inv, "T": t})
}

func (u *UI) mockCheckoutSubmit(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r, "paymentID")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	pay, _, _, err := u.svc.PaymentForCheckout(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	pay, err = u.svc.MockOutcome(r.Context(), pay.TenantID, pay.ID, r.FormValue("outcome"))
	if err != nil {
		u.done(w, r, "/checkout/mock/"+id.String(), err, "")
		return
	}
	if pay.ReturnURL != "" {
		sep := "?"
		if strings.Contains(pay.ReturnURL, "?") {
			sep = "&"
		}
		http.Redirect(w, r, pay.ReturnURL+sep+"payment_id="+pay.ID.String()+"&status="+string(pay.Status), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/checkout/mock/"+id.String(), http.StatusSeeOther)
}
