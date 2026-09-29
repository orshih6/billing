// Package api is the HTTP surface. Handlers decode, call the billing service
// and encode; every rule lives in the service so the web UI enforces the same
// ones.
package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"gorm.io/gorm"

	"github.com/orshih6/billing/internal/auth"
	"github.com/orshih6/billing/internal/billing"
	"github.com/orshih6/billing/internal/config"
)

// Handler serves the API.
type Handler struct {
	svc  *billing.Service
	auth *auth.Authenticator
	db   *gorm.DB
	cfg  *config.Config
	log  *slog.Logger
	// Extra mounts other surfaces (the web UI) on the same router.
	Extra func(r chi.Router)
}

// NewHandler builds the API handler.
func NewHandler(svc *billing.Service, a *auth.Authenticator, db *gorm.DB, cfg *config.Config, log *slog.Logger) *Handler {
	return &Handler{svc: svc, auth: a, db: db, cfg: cfg, log: log}
}

// Routes builds the router.
//
// Paths are unversioned and shaped by resource. Every authenticated route
// declares the scope it needs right here, so the permission model reads in one
// place. Tenant routes resolve the tenant from the key (or X-Tenant-Id for a
// platform key) before any handler runs.
func (h *Handler) Routes() http.Handler {
	r := chi.NewRouter()
	r.Use(requestID, middleware.RealIP, recoverPanics(h.log), requestLogger(h.log))
	r.Use(middleware.Timeout(h.cfg.HTTP.WriteTimeout))

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { respondJSON(w, 200, map[string]string{"status": "ok"}) })
	r.Get("/readyz", h.ready)
	// Provider callbacks carry no key; the body is only used to find the
	// payment and the provider is asked for the truth.
	r.Post("/webhooks/{provider}/{accountID}", h.providerWebhook)

	if h.Extra != nil {
		h.Extra(r)
	}

	r.Group(func(r chi.Router) {
		r.Use(h.authenticate)

		r.Get("/me", h.me)
		r.Get("/scopes", h.scopes)
		r.Get("/event-types", h.eventTypes)

		// Platform: tenants.
		r.Route("/tenants", func(r chi.Router) {
			r.Use(requirePlatform)
			r.Get("/", h.listTenants)
			r.Post("/", h.createTenant)
			r.Get("/{tenantID}", h.getTenant)
			r.Patch("/{tenantID}", h.updateTenant)
			r.Get("/{tenantID}/api-keys", h.listTenantKeys)
			r.Post("/{tenantID}/api-keys", h.createTenantKey)
		})
		r.With(requirePlatform).Post("/engine/run", h.runEngine)

		// Everything below acts inside one tenant.
		r.Group(func(r chi.Router) {
			r.Use(h.withTenant, h.idempotent)

			r.Get("/tenant", h.currentTenant)
			r.Get("/search", h.search)
			r.With(requireScope(auth.ScopeAdmin)).Patch("/tenant", h.updateCurrentTenant)

			r.Route("/api-keys", func(r chi.Router) {
				r.With(requireScope(auth.ScopeKeysRead)).Get("/", h.listKeys)
				r.With(requireScope(auth.ScopeKeysWrite)).Post("/", h.createKey)
				r.With(requireScope(auth.ScopeKeysWrite)).Post("/{keyID}/revoke", h.revokeKey)
			})

			r.Route("/customers", func(r chi.Router) {
				read, write := requireScope(auth.ScopeCustomersRead), requireScope(auth.ScopeCustomersWrite)
				r.With(read).Get("/", h.listCustomers)
				r.With(write).Post("/", h.createCustomer)
				r.With(read).Get("/by-external/{externalID}", h.getCustomerByExternal)
				r.With(read).Get("/by-external/{externalID}/entitlements", h.entitlementsByExternal)
				r.With(read).Get("/{customerID}", h.getCustomer)
				r.With(write).Patch("/{customerID}", h.updateCustomer)
				r.With(read).Get("/{customerID}/balance", h.customerBalance)
				r.With(read).Get("/{customerID}/entitlements", h.entitlements)
				r.With(write).Post("/{customerID}/credits", h.grantCredit)
			})

			r.Route("/plans", func(r chi.Router) {
				read, write := requireScope(auth.ScopePlansRead), requireScope(auth.ScopePlansWrite)
				r.With(read).Get("/", h.listPlans)
				r.With(write).Post("/", h.createPlan)
				r.With(read).Get("/by-code/{code}", h.getPlanByCode)
				r.With(read).Get("/{planID}", h.getPlan)
				r.With(write).Patch("/{planID}", h.updatePlan)
				r.With(write).Post("/{planID}/prices", h.addPrice)
			})
			r.Route("/prices/{priceID}", func(r chi.Router) {
				r.With(requireScope(auth.ScopePlansRead)).Get("/", h.getPrice)
				r.With(requireScope(auth.ScopePlansWrite)).Post("/activate", h.setPriceActive(true))
				r.With(requireScope(auth.ScopePlansWrite)).Post("/deactivate", h.setPriceActive(false))
			})

			r.Route("/coupons", func(r chi.Router) {
				read, write := requireScope(auth.ScopeCouponsRead), requireScope(auth.ScopeCouponsWrite)
				r.With(read).Get("/", h.listCoupons)
				r.With(write).Post("/", h.createCoupon)
				r.With(read).Get("/check", h.checkPromoCode)
				r.With(read).Get("/{couponID}", h.getCoupon)
				r.With(write).Patch("/{couponID}", h.updateCoupon)
			})

			r.Route("/subscriptions", func(r chi.Router) {
				read, write := requireScope(auth.ScopeSubscriptionsRead), requireScope(auth.ScopeSubscriptionsWrite)
				r.With(read).Get("/", h.listSubscriptions)
				r.With(write).Post("/", h.createSubscription)
				r.With(read).Get("/{subscriptionID}", h.getSubscription)
				r.With(write).Post("/{subscriptionID}/cancel", h.cancelSubscription)
				r.With(write).Post("/{subscriptionID}/resume", h.resumeSubscription)
				r.With(write).Post("/{subscriptionID}/change-price", h.changePrice)
				r.With(write).Post("/{subscriptionID}/quantity", h.setQuantity)
				r.With(write).Post("/{subscriptionID}/add-ons", h.setAddOn)
				r.With(write).Post("/{subscriptionID}/discount", h.applyPromoCode)
				r.With(write).Delete("/{subscriptionID}/discount", h.removeDiscount)
			})

			r.Route("/invoices", func(r chi.Router) {
				read, write := requireScope(auth.ScopeInvoicesRead), requireScope(auth.ScopeInvoicesWrite)
				r.With(read).Get("/", h.listInvoices)
				r.With(write).Post("/", h.createInvoice)
				r.With(read).Get("/{invoiceID}", h.getInvoice)
				r.With(write).Post("/{invoiceID}/lines", h.addInvoiceLine)
				r.With(write).Post("/{invoiceID}/finalize", h.finalizeInvoice)
				r.With(write).Post("/{invoiceID}/void", h.voidInvoice)
				r.With(write).Post("/{invoiceID}/mark-uncollectible", h.markUncollectible)
				r.With(requireScope(auth.ScopePaymentsRead)).Get("/{invoiceID}/payments", h.listInvoicePayments)
				r.With(requireScope(auth.ScopePaymentsWrite)).Post("/{invoiceID}/payments", h.startPayment)
			})

			r.Route("/payment-providers", func(r chi.Router) {
				r.With(requireScope(auth.ScopePaymentsRead)).Get("/", h.listPaymentProviders)
				r.With(requireScope(auth.ScopeAdmin)).Put("/{provider}", h.configureProvider)
				r.With(requireScope(auth.ScopeAdmin)).Delete("/{provider}", h.disconnectProvider)
			})

			r.Route("/payments", func(r chi.Router) {
				read, write := requireScope(auth.ScopePaymentsRead), requireScope(auth.ScopePaymentsWrite)
				manual := requireScope(auth.ScopePaymentsManual)
				r.With(read).Get("/", h.listPayments)
				r.With(manual).Post("/manual", h.recordManualPayment)
				r.With(read).Get("/{paymentID}", h.getPayment)
				r.With(manual).Post("/{paymentID}/confirm", h.confirmPayment)
				r.With(write).Post("/{paymentID}/mock/{outcome}", h.mockOutcome)
				r.With(write).Post("/{paymentID}/cancel", h.cancelPayment)
				r.With(write).Post("/{paymentID}/refund", h.refundPayment)
				r.With(write).Post("/{paymentID}/retry-provider-cancel", h.retryProviderCancel)
			})

			r.Route("/ledger", func(r chi.Router) {
				r.Use(requireScope(auth.ScopeLedgerRead))
				r.Get("/accounts", h.listLedgerAccounts)
				r.Get("/accounts/{accountID}", h.getLedgerAccount)
				r.Get("/accounts/{accountID}/entries", h.ledgerEntries)
				r.Get("/transactions", h.listLedgerTransactions)
			})
			r.With(requireScope(auth.ScopeLedgerRead)).Get("/reports/ledger-reconciliation", h.reconcile)
			r.With(requireScope(auth.ScopeInvoicesRead)).Get("/reports/stats", h.stats)

			r.Route("/webhook-endpoints", func(r chi.Router) {
				read, write := requireScope(auth.ScopeWebhooksRead), requireScope(auth.ScopeWebhooksWrite)
				r.With(read).Get("/", h.listWebhookEndpoints)
				r.With(write).Post("/", h.createWebhookEndpoint)
				r.With(write).Patch("/{endpointID}", h.updateWebhookEndpoint)
				r.With(write).Delete("/{endpointID}", h.deleteWebhookEndpoint)
				r.With(read).Get("/{endpointID}/deliveries", h.listDeliveries)
			})
			r.With(requireScope(auth.ScopeWebhooksWrite)).Post("/webhook-deliveries/{deliveryID}/retry", h.retryDelivery)
			r.With(requireScope(auth.ScopeWebhooksRead)).Get("/events", h.listEvents)
		})
	})

	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		respondError(w, http.StatusNotFound, "route_not_found", "no such route")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		respondError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed on this route")
	})
	return r
}

func (h *Handler) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	sqlDB, err := h.db.DB()
	if err == nil {
		err = sqlDB.PingContext(ctx)
	}
	if err != nil {
		respondError(w, http.StatusServiceUnavailable, "database_unavailable", "database is not reachable")
		return
	}
	respondJSON(w, 200, map[string]string{"status": "ready"})
}
