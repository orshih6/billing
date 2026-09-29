package api

import (
	"net/http"
	"sort"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/orshih6/billing/internal/auth"
	"github.com/orshih6/billing/internal/billing"
	"github.com/orshih6/billing/internal/domain"
	"github.com/orshih6/billing/internal/events"
	"github.com/orshih6/billing/internal/models"
	"github.com/orshih6/billing/internal/wire"
)

// ---- identity -------------------------------------------------------------

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	p := PrincipalFrom(r.Context())
	out := map[string]any{
		"key_id": p.KeyID, "name": p.Name, "scopes": p.Scopes, "platform": p.IsPlatform(), "bootstrap": p.Bootstrap,
	}
	if p.KeyTenantID != nil {
		if t, err := h.svc.Tenant(r.Context(), *p.KeyTenantID); err == nil {
			out["tenant"] = wire.Of(t)
		}
	}
	respondJSON(w, 200, out)
}

func (h *Handler) scopes(w http.ResponseWriter, _ *http.Request) {
	desc := auth.Describe()
	type item struct {
		Scope       auth.Scope `json:"scope"`
		Description string     `json:"description"`
	}
	out := []item{}
	for _, s := range auth.All() {
		out = append(out, item{s, desc[s]})
	}
	respondJSON(w, 200, out)
}

func (h *Handler) eventTypes(w http.ResponseWriter, _ *http.Request) {
	respondJSON(w, 200, events.Types())
}

// ---- tenants (platform) ------------------------------------------------------

func (h *Handler) listTenants(w http.ResponseWriter, r *http.Request) {
	p, ok := page(w, r)
	if !ok {
		return
	}
	out, err := h.svc.ListTenants(r.Context(), p)
	h.respond(w, r, 200, out, err)
}

func (h *Handler) createTenant(w http.ResponseWriter, r *http.Request) {
	var in billing.CreateTenantInput
	if !decode(w, r, &in) {
		return
	}
	out, err := h.svc.CreateTenant(r.Context(), in)
	h.respond(w, r, 201, out, err)
}

func (h *Handler) getTenant(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	t, err := h.svc.Tenant(r.Context(), id)
	h.respond(w, r, 200, tenantOrNil(t), err)
}

func (h *Handler) updateTenant(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	var in billing.UpdateTenantInput
	if !decode(w, r, &in) {
		return
	}
	t, err := h.svc.UpdateTenant(r.Context(), id, in)
	h.respond(w, r, 200, tenantOrNil(t), err)
}

func (h *Handler) listTenantKeys(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	p, ok := page(w, r)
	if !ok {
		return
	}
	out, err := h.svc.ListKeys(r.Context(), &id, p)
	h.respond(w, r, 200, out, err)
}

func (h *Handler) createTenantKey(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	var in billing.CreateKeyInput
	if !decode(w, r, &in) {
		return
	}
	out, err := h.svc.CreateKey(r.Context(), PrincipalFrom(r.Context()), &id, in)
	h.respond(w, r, 201, out, err)
}

func (h *Handler) runEngine(w http.ResponseWriter, r *http.Request) {
	rep, err := h.svc.RunEngine(r.Context())
	h.respond(w, r, 200, rep, err)
}

// tenantOrNil avoids wrapping a nil tenant (the error path) in an interface.
func tenantOrNil(t *models.Tenant) any {
	if t == nil {
		return nil
	}
	return t
}

func (h *Handler) currentTenant(w http.ResponseWriter, r *http.Request) {
	t, err := h.svc.Tenant(r.Context(), TenantFrom(r.Context()))
	h.respond(w, r, 200, tenantOrNil(t), err)
}

func (h *Handler) updateCurrentTenant(w http.ResponseWriter, r *http.Request) {
	var in billing.UpdateTenantInput
	if !decode(w, r, &in) {
		return
	}
	if in.Status != nil && !PrincipalFrom(r.Context()).IsPlatform() {
		respondError(w, http.StatusForbidden, "platform_only", "only a platform key can change tenant status")
		return
	}
	t, err := h.svc.UpdateTenant(r.Context(), TenantFrom(r.Context()), in)
	h.respond(w, r, 200, tenantOrNil(t), err)
}

// ---- api keys ------------------------------------------------------------------

func (h *Handler) listKeys(w http.ResponseWriter, r *http.Request) {
	p, ok := page(w, r)
	if !ok {
		return
	}
	tid := TenantFrom(r.Context())
	out, err := h.svc.ListKeys(r.Context(), &tid, p)
	h.respond(w, r, 200, out, err)
}

func (h *Handler) createKey(w http.ResponseWriter, r *http.Request) {
	var in billing.CreateKeyInput
	if !decode(w, r, &in) {
		return
	}
	tid := TenantFrom(r.Context())
	out, err := h.svc.CreateKey(r.Context(), PrincipalFrom(r.Context()), &tid, in)
	h.respond(w, r, 201, out, err)
}

func (h *Handler) revokeKey(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "keyID")
	if !ok {
		return
	}
	tid := TenantFrom(r.Context())
	out, err := h.svc.RevokeKey(r.Context(), &tid, id)
	h.respond(w, r, 200, out, err)
}

// ---- customers -------------------------------------------------------------------

func (h *Handler) listCustomers(w http.ResponseWriter, r *http.Request) {
	p, ok := page(w, r)
	if !ok {
		return
	}
	c, ok := common(w, r)
	if !ok {
		return
	}
	out, err := h.svc.ListCustomers(r.Context(), TenantFrom(r.Context()), billing.CustomerFilter{Query: r.URL.Query().Get("q"), Common: c}, p)
	h.respond(w, r, 200, out, err)
}

func (h *Handler) createCustomer(w http.ResponseWriter, r *http.Request) {
	var in billing.CustomerInput
	if !decode(w, r, &in) {
		return
	}
	out, err := h.svc.CreateCustomer(r.Context(), TenantFrom(r.Context()), in)
	h.respond(w, r, 201, out, err)
}

func (h *Handler) getCustomer(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "customerID")
	if !ok {
		return
	}
	out, err := h.svc.Customer(r.Context(), TenantFrom(r.Context()), id)
	h.respond(w, r, 200, out, err)
}

func (h *Handler) getCustomerByExternal(w http.ResponseWriter, r *http.Request) {
	out, err := h.svc.CustomerByExternalID(r.Context(), TenantFrom(r.Context()), chi.URLParam(r, "externalID"))
	h.respond(w, r, 200, out, err)
}

func (h *Handler) updateCustomer(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "customerID")
	if !ok {
		return
	}
	var in billing.CustomerInput
	if !decode(w, r, &in) {
		return
	}
	out, err := h.svc.UpdateCustomer(r.Context(), TenantFrom(r.Context()), id, in)
	h.respond(w, r, 200, out, err)
}

func (h *Handler) customerBalance(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "customerID")
	if !ok {
		return
	}
	out, err := h.svc.CustomerBalances(r.Context(), TenantFrom(r.Context()), id)
	h.respond(w, r, 200, map[string]any{"balances": out}, err)
}

func (h *Handler) entitlements(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "customerID")
	if !ok {
		return
	}
	out, err := h.svc.Entitlements(r.Context(), TenantFrom(r.Context()), id)
	h.respond(w, r, 200, map[string]any{"customer_id": id, "entitlements": out}, err)
}

func (h *Handler) entitlementsByExternal(w http.ResponseWriter, r *http.Request) {
	c, err := h.svc.CustomerByExternalID(r.Context(), TenantFrom(r.Context()), chi.URLParam(r, "externalID"))
	if err != nil {
		respondServiceError(w, r, h.log, err)
		return
	}
	out, err := h.svc.Entitlements(r.Context(), TenantFrom(r.Context()), c.ID)
	h.respond(w, r, 200, map[string]any{"customer_id": c.ID, "entitlements": out}, err)
}

func (h *Handler) grantCredit(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "customerID")
	if !ok {
		return
	}
	var in billing.GrantCreditInput
	if !decode(w, r, &in) {
		return
	}
	out, err := h.svc.GrantCredit(r.Context(), TenantFrom(r.Context()), id, in)
	h.respond(w, r, 201, map[string]any{"balances": out}, err)
}

// ---- plans & prices -----------------------------------------------------------------

func (h *Handler) listPlans(w http.ResponseWriter, r *http.Request) {
	p, ok := page(w, r)
	if !ok {
		return
	}
	out, err := h.svc.ListPlans(r.Context(), TenantFrom(r.Context()), queryBool(r, "active"), p)
	h.respond(w, r, 200, out, err)
}

func (h *Handler) createPlan(w http.ResponseWriter, r *http.Request) {
	var in billing.PlanInput
	if !decode(w, r, &in) {
		return
	}
	out, err := h.svc.CreatePlan(r.Context(), TenantFrom(r.Context()), in)
	h.respond(w, r, 201, out, err)
}

func (h *Handler) getPlan(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "planID")
	if !ok {
		return
	}
	out, err := h.svc.Plan(r.Context(), TenantFrom(r.Context()), id)
	h.respond(w, r, 200, out, err)
}

func (h *Handler) getPlanByCode(w http.ResponseWriter, r *http.Request) {
	out, err := h.svc.PlanByCode(r.Context(), TenantFrom(r.Context()), chi.URLParam(r, "code"))
	h.respond(w, r, 200, out, err)
}

func (h *Handler) updatePlan(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "planID")
	if !ok {
		return
	}
	var in billing.UpdatePlanInput
	if !decode(w, r, &in) {
		return
	}
	out, err := h.svc.UpdatePlan(r.Context(), TenantFrom(r.Context()), id, in)
	h.respond(w, r, 200, out, err)
}

func (h *Handler) addPrice(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "planID")
	if !ok {
		return
	}
	var in billing.PriceInput
	if !decode(w, r, &in) {
		return
	}
	out, err := h.svc.AddPrice(r.Context(), TenantFrom(r.Context()), id, in)
	h.respond(w, r, 201, out, err)
}

func (h *Handler) getPrice(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "priceID")
	if !ok {
		return
	}
	out, err := h.svc.Price(r.Context(), TenantFrom(r.Context()), id)
	h.respond(w, r, 200, out, err)
}

func (h *Handler) setPriceActive(active bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathUUID(w, r, "priceID")
		if !ok {
			return
		}
		out, err := h.svc.SetPriceActive(r.Context(), TenantFrom(r.Context()), id, active)
		h.respond(w, r, 200, out, err)
	}
}

// ---- subscriptions -------------------------------------------------------------------

func (h *Handler) listSubscriptions(w http.ResponseWriter, r *http.Request) {
	p, ok := page(w, r)
	if !ok {
		return
	}
	cust, ok := queryUUID(w, r, "customer_id")
	if !ok {
		return
	}
	plan, ok := queryUUID(w, r, "plan_id")
	if !ok {
		return
	}
	c, ok := common(w, r)
	if !ok {
		return
	}
	out, err := h.svc.ListSubscriptions(r.Context(), TenantFrom(r.Context()), billing.SubscriptionFilter{
		CustomerID: cust, PlanID: plan, PlanCode: r.URL.Query().Get("plan_code"), Status: r.URL.Query().Get("status"), Common: c}, p)
	h.respond(w, r, 200, out, err)
}

func (h *Handler) createSubscription(w http.ResponseWriter, r *http.Request) {
	var in billing.CreateSubscriptionInput
	if !decode(w, r, &in) {
		return
	}
	out, err := h.svc.CreateSubscription(r.Context(), TenantFrom(r.Context()), in)
	h.respond(w, r, 201, out, err)
}

func (h *Handler) getSubscription(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "subscriptionID")
	if !ok {
		return
	}
	out, err := h.svc.Subscription(r.Context(), TenantFrom(r.Context()), id)
	h.respond(w, r, 200, out, err)
}

func (h *Handler) cancelSubscription(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "subscriptionID")
	if !ok {
		return
	}
	in := struct {
		AtPeriodEnd *bool `json:"at_period_end"`
	}{}
	if !decode(w, r, &in) {
		return
	}
	atEnd := in.AtPeriodEnd == nil || *in.AtPeriodEnd
	out, err := h.svc.CancelSubscription(r.Context(), TenantFrom(r.Context()), id, atEnd)
	h.respond(w, r, 200, out, err)
}

func (h *Handler) resumeSubscription(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "subscriptionID")
	if !ok {
		return
	}
	out, err := h.svc.ResumeSubscription(r.Context(), TenantFrom(r.Context()), id)
	h.respond(w, r, 200, out, err)
}

func (h *Handler) changePrice(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "subscriptionID")
	if !ok {
		return
	}
	var in billing.ChangePriceInput
	if !decode(w, r, &in) {
		return
	}
	out, err := h.svc.ChangePrice(r.Context(), TenantFrom(r.Context()), id, in)
	h.respond(w, r, 200, out, err)
}

func (h *Handler) setQuantity(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "subscriptionID")
	if !ok {
		return
	}
	in := struct {
		Quantity int `json:"quantity"`
	}{}
	if !decode(w, r, &in) {
		return
	}
	out, err := h.svc.SetQuantity(r.Context(), TenantFrom(r.Context()), id, in.Quantity)
	h.respond(w, r, 200, out, err)
}

func (h *Handler) setAddOn(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "subscriptionID")
	if !ok {
		return
	}
	var in billing.AddOnInput
	if !decode(w, r, &in) {
		return
	}
	out, err := h.svc.SetAddOn(r.Context(), TenantFrom(r.Context()), id, in)
	h.respond(w, r, 200, out, err)
}

func (h *Handler) applyPromoCode(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "subscriptionID")
	if !ok {
		return
	}
	in := struct {
		PromoCode string `json:"promo_code"`
	}{}
	if !decode(w, r, &in) {
		return
	}
	out, err := h.svc.ApplyPromoCode(r.Context(), TenantFrom(r.Context()), id, in.PromoCode)
	h.respond(w, r, 200, out, err)
}

func (h *Handler) removeDiscount(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "subscriptionID")
	if !ok {
		return
	}
	out, err := h.svc.RemoveDiscount(r.Context(), TenantFrom(r.Context()), id)
	h.respond(w, r, 200, out, err)
}

// search looks across every resource the key can read: GET /search?q=…
func (h *Handler) search(w http.ResponseWriter, r *http.Request) {
	p := PrincipalFrom(r.Context())
	in := billing.SearchIn{
		Customers: p.Has(auth.ScopeCustomersRead), Invoices: p.Has(auth.ScopeInvoicesRead),
		Payments: p.Has(auth.ScopePaymentsRead), Subscriptions: p.Has(auth.ScopeSubscriptionsRead),
	}
	out, err := h.svc.Search(r.Context(), TenantFrom(r.Context()), r.URL.Query().Get("q"), in)
	h.respond(w, r, 200, out, err)
}

// ---- coupons ------------------------------------------------------------------

func (h *Handler) listCoupons(w http.ResponseWriter, r *http.Request) {
	p, ok := page(w, r)
	if !ok {
		return
	}
	out, err := h.svc.ListCoupons(r.Context(), TenantFrom(r.Context()), p)
	h.respond(w, r, 200, out, err)
}

func (h *Handler) createCoupon(w http.ResponseWriter, r *http.Request) {
	var in billing.CouponInput
	if !decode(w, r, &in) {
		return
	}
	out, err := h.svc.CreateCoupon(r.Context(), TenantFrom(r.Context()), in)
	h.respond(w, r, 201, out, err)
}

func (h *Handler) getCoupon(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "couponID")
	if !ok {
		return
	}
	out, err := h.svc.Coupon(r.Context(), TenantFrom(r.Context()), id)
	h.respond(w, r, 200, out, err)
}

func (h *Handler) updateCoupon(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "couponID")
	if !ok {
		return
	}
	var in billing.UpdateCouponInput
	if !decode(w, r, &in) {
		return
	}
	out, err := h.svc.UpdateCoupon(r.Context(), TenantFrom(r.Context()), id, in)
	h.respond(w, r, 200, out, err)
}

// checkPromoCode lets a checkout validate a code before subscribing:
// GET /coupons/check?code=SPRING&currency=MNT
func (h *Handler) checkPromoCode(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	out, err := h.svc.CheckPromoCode(r.Context(), TenantFrom(r.Context()), q.Get("code"), q.Get("currency"))
	h.respond(w, r, 200, out, err)
}

// ---- invoices ----------------------------------------------------------------------------

func (h *Handler) listInvoices(w http.ResponseWriter, r *http.Request) {
	p, ok := page(w, r)
	if !ok {
		return
	}
	cust, ok := queryUUID(w, r, "customer_id")
	if !ok {
		return
	}
	sub, ok := queryUUID(w, r, "subscription_id")
	if !ok {
		return
	}
	c, ok := common(w, r)
	if !ok {
		return
	}
	out, err := h.svc.ListInvoices(r.Context(), TenantFrom(r.Context()), billing.InvoiceFilter{
		CustomerID: cust, SubscriptionID: sub, Status: r.URL.Query().Get("status"), Query: r.URL.Query().Get("q"), Common: c}, p)
	h.respond(w, r, 200, out, err)
}

func (h *Handler) createInvoice(w http.ResponseWriter, r *http.Request) {
	var in billing.InvoiceInput
	if !decode(w, r, &in) {
		return
	}
	out, err := h.svc.CreateInvoice(r.Context(), TenantFrom(r.Context()), in)
	h.respond(w, r, 201, out, err)
}

func (h *Handler) getInvoice(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "invoiceID")
	if !ok {
		return
	}
	out, err := h.svc.Invoice(r.Context(), TenantFrom(r.Context()), id)
	h.respond(w, r, 200, out, err)
}

func (h *Handler) addInvoiceLine(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "invoiceID")
	if !ok {
		return
	}
	var in billing.LineInput
	if !decode(w, r, &in) {
		return
	}
	out, err := h.svc.AddInvoiceLine(r.Context(), TenantFrom(r.Context()), id, in)
	h.respond(w, r, 200, out, err)
}

func (h *Handler) finalizeInvoice(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "invoiceID")
	if !ok {
		return
	}
	out, err := h.svc.FinalizeInvoice(r.Context(), TenantFrom(r.Context()), id)
	h.respond(w, r, 200, out, err)
}

func (h *Handler) voidInvoice(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "invoiceID")
	if !ok {
		return
	}
	in := struct {
		Reason string `json:"reason"`
	}{}
	if !decode(w, r, &in) {
		return
	}
	out, err := h.svc.VoidInvoice(r.Context(), TenantFrom(r.Context()), id, in.Reason)
	h.respond(w, r, 200, out, err)
}

func (h *Handler) markUncollectible(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "invoiceID")
	if !ok {
		return
	}
	out, err := h.svc.MarkUncollectible(r.Context(), TenantFrom(r.Context()), id)
	h.respond(w, r, 200, out, err)
}

func (h *Handler) listInvoicePayments(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "invoiceID")
	if !ok {
		return
	}
	p, ok := page(w, r)
	if !ok {
		return
	}
	out, err := h.svc.ListPayments(r.Context(), TenantFrom(r.Context()), billing.PaymentFilter{InvoiceID: &id}, p)
	h.respond(w, r, 200, out, err)
}

func (h *Handler) startPayment(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "invoiceID")
	if !ok {
		return
	}
	var in billing.StartPaymentInput
	if !decode(w, r, &in) {
		return
	}
	out, err := h.svc.StartPayment(r.Context(), TenantFrom(r.Context()), id, in, keyID(r))
	h.respond(w, r, 201, out, err)
}

// ---- payments --------------------------------------------------------------------------

func keyID(r *http.Request) *uuid.UUID {
	if p := PrincipalFrom(r.Context()); p != nil && p.KeyID != uuid.Nil {
		return &p.KeyID
	}
	return nil
}

func (h *Handler) listPayments(w http.ResponseWriter, r *http.Request) {
	p, ok := page(w, r)
	if !ok {
		return
	}
	inv, ok := queryUUID(w, r, "invoice_id")
	if !ok {
		return
	}
	cust, ok := queryUUID(w, r, "customer_id")
	if !ok {
		return
	}
	c, ok := common(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	out, err := h.svc.ListPayments(r.Context(), TenantFrom(r.Context()), billing.PaymentFilter{
		InvoiceID: inv, CustomerID: cust, Status: q.Get("status"), Provider: q.Get("provider"), Query: q.Get("q"), Common: c}, p)
	h.respond(w, r, 200, out, err)
}

func (h *Handler) getPayment(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "paymentID")
	if !ok {
		return
	}
	out, err := h.svc.Payment(r.Context(), TenantFrom(r.Context()), id)
	h.respond(w, r, 200, out, err)
}

func (h *Handler) recordManualPayment(w http.ResponseWriter, r *http.Request) {
	var in billing.ManualPaymentInput
	if !decode(w, r, &in) {
		return
	}
	out, err := h.svc.RecordManualPayment(r.Context(), TenantFrom(r.Context()), in, keyID(r))
	h.respond(w, r, 201, out, err)
}

func (h *Handler) confirmPayment(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "paymentID")
	if !ok {
		return
	}
	in := struct {
		Amount int64  `json:"amount"`
		Note   string `json:"note"`
	}{}
	if !decode(w, r, &in) {
		return
	}
	out, err := h.svc.ConfirmManualPayment(r.Context(), TenantFrom(r.Context()), id, in.Amount, in.Note)
	h.respond(w, r, 200, out, err)
}

func (h *Handler) mockOutcome(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "paymentID")
	if !ok {
		return
	}
	out, err := h.svc.MockOutcome(r.Context(), TenantFrom(r.Context()), id, chi.URLParam(r, "outcome"))
	h.respond(w, r, 200, out, err)
}

func (h *Handler) cancelPayment(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "paymentID")
	if !ok {
		return
	}
	out, err := h.svc.CancelPayment(r.Context(), TenantFrom(r.Context()), id)
	h.respond(w, r, 200, out, err)
}

func (h *Handler) refundPayment(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "paymentID")
	if !ok {
		return
	}
	in := struct {
		Reason string `json:"reason"`
	}{}
	if !decode(w, r, &in) {
		return
	}
	out, err := h.svc.RefundPayment(r.Context(), TenantFrom(r.Context()), id, in.Reason)
	h.respond(w, r, 200, out, err)
}

func (h *Handler) retryProviderCancel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "paymentID")
	if !ok {
		return
	}
	out, err := h.svc.RetryProviderCancel(r.Context(), TenantFrom(r.Context()), id)
	h.respond(w, r, 200, out, err)
}

func (h *Handler) listPaymentProviders(w http.ResponseWriter, r *http.Request) {
	out, err := h.svc.PaymentProviders(r.Context(), TenantFrom(r.Context()))
	h.respond(w, r, 200, map[string]any{"data": out}, err)
}

func (h *Handler) configureProvider(w http.ResponseWriter, r *http.Request) {
	var in billing.ProviderAccountInput
	if !decode(w, r, &in) {
		return
	}
	out, err := h.svc.ConfigureProvider(r.Context(), TenantFrom(r.Context()), chi.URLParam(r, "provider"), in)
	h.respond(w, r, 200, out, err)
}

func (h *Handler) disconnectProvider(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.DisconnectProvider(r.Context(), TenantFrom(r.Context()), chi.URLParam(r, "provider")); err != nil {
		respondServiceError(w, r, h.log, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) providerWebhook(w http.ResponseWriter, r *http.Request) {
	accountID, ok := pathUUID(w, r, "accountID")
	if !ok {
		return
	}
	if err := h.svc.HandleWebhook(r.Context(), chi.URLParam(r, "provider"), accountID, r); err != nil {
		respondServiceError(w, r, h.log, err)
		return
	}
	respondJSON(w, 200, map[string]bool{"received": true})
}

// ---- ledger & reports ------------------------------------------------------------------

func (h *Handler) listLedgerAccounts(w http.ResponseWriter, r *http.Request) {
	p, ok := page(w, r)
	if !ok {
		return
	}
	cust, ok := queryUUID(w, r, "customer_id")
	if !ok {
		return
	}
	out, err := h.svc.ListLedgerAccounts(r.Context(), TenantFrom(r.Context()), billing.LedgerAccountFilter{
		CustomerID: cust, Currency: r.URL.Query().Get("currency"), SystemOnly: queryBool(r, "system"),
	}, p)
	h.respond(w, r, 200, out, err)
}

func (h *Handler) getLedgerAccount(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "accountID")
	if !ok {
		return
	}
	out, err := h.svc.LedgerAccount(r.Context(), TenantFrom(r.Context()), id)
	h.respond(w, r, 200, out, err)
}

func (h *Handler) ledgerEntries(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "accountID")
	if !ok {
		return
	}
	p, ok := page(w, r)
	if !ok {
		return
	}
	out, err := h.svc.AccountEntries(r.Context(), TenantFrom(r.Context()), id, p.Limit)
	h.respond(w, r, 200, wire.ListOf(out, false), err)
}

func (h *Handler) listLedgerTransactions(w http.ResponseWriter, r *http.Request) {
	p, ok := page(w, r)
	if !ok {
		return
	}
	inv, ok := queryUUID(w, r, "invoice_id")
	if !ok {
		return
	}
	pay, ok := queryUUID(w, r, "payment_id")
	if !ok {
		return
	}
	cust, ok := queryUUID(w, r, "customer_id")
	if !ok {
		return
	}
	out, err := h.svc.ListLedgerTransactions(r.Context(), TenantFrom(r.Context()),
		billing.LedgerTransactionFilter{InvoiceID: inv, PaymentID: pay, CustomerID: cust}, p)
	h.respond(w, r, 200, out, err)
}

func (h *Handler) reconcile(w http.ResponseWriter, r *http.Request) {
	out, err := h.svc.Reconcile(r.Context(), TenantFrom(r.Context()))
	h.respond(w, r, 200, out, err)
}

func (h *Handler) stats(w http.ResponseWriter, r *http.Request) {
	out, err := h.svc.TenantStats(r.Context(), TenantFrom(r.Context()))
	h.respond(w, r, 200, out, err)
}

// ---- webhooks -------------------------------------------------------------------------

func (h *Handler) listWebhookEndpoints(w http.ResponseWriter, r *http.Request) {
	p, ok := page(w, r)
	if !ok {
		return
	}
	out, err := h.svc.ListWebhookEndpoints(r.Context(), TenantFrom(r.Context()), p)
	h.respond(w, r, 200, out, err)
}

func (h *Handler) createWebhookEndpoint(w http.ResponseWriter, r *http.Request) {
	var in billing.WebhookEndpointInput
	if !decode(w, r, &in) {
		return
	}
	out, err := h.svc.CreateWebhookEndpoint(r.Context(), TenantFrom(r.Context()), in)
	h.respond(w, r, 201, out, err)
}

func (h *Handler) updateWebhookEndpoint(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "endpointID")
	if !ok {
		return
	}
	var in billing.WebhookEndpointInput
	if !decode(w, r, &in) {
		return
	}
	out, err := h.svc.UpdateWebhookEndpoint(r.Context(), TenantFrom(r.Context()), id, in)
	h.respond(w, r, 200, out, err)
}

func (h *Handler) deleteWebhookEndpoint(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "endpointID")
	if !ok {
		return
	}
	if err := h.svc.DeleteWebhookEndpoint(r.Context(), TenantFrom(r.Context()), id); err != nil {
		respondServiceError(w, r, h.log, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) listDeliveries(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "endpointID")
	if !ok {
		return
	}
	p, ok := page(w, r)
	if !ok {
		return
	}
	out, err := h.svc.ListDeliveries(r.Context(), TenantFrom(r.Context()), &id, p)
	h.respond(w, r, 200, out, err)
}

func (h *Handler) retryDelivery(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "deliveryID")
	if !ok {
		return
	}
	out, err := h.svc.RetryDelivery(r.Context(), TenantFrom(r.Context()), id)
	h.respond(w, r, 200, out, err)
}

func (h *Handler) listEvents(w http.ResponseWriter, r *http.Request) {
	p, ok := page(w, r)
	if !ok {
		return
	}
	out, err := h.svc.Events(r.Context(), TenantFrom(r.Context()), r.URL.Query().Get("type"), p)
	h.respond(w, r, 200, out, err)
}

// respond writes v with status, or the error.
func (h *Handler) respond(w http.ResponseWriter, r *http.Request, status int, v any, err error) {
	if err != nil {
		respondServiceError(w, r, h.log, err)
		return
	}
	respondJSON(w, status, wire.Of(v))
}

// Currencies is exposed for the UI and clients.
func Currencies() []domain.Currency {
	cs := domain.Currencies()
	sort.Slice(cs, func(i, j int) bool { return cs[i].Code < cs[j].Code })
	return cs
}
