// Package wire is the public shape of every object the API returns and every
// webhook carries. It is deliberately separate from the database models: a
// column can be added, renamed or dropped without silently changing the
// contract integrators depend on. Change a type here only on purpose, and keep
// client/ in step (client_test.go fails otherwise).
//
// Every object has an "object" field naming its type, so a webhook consumer
// can switch on it.
package wire

import (
	"time"

	"github.com/google/uuid"

	"github.com/orshih6/billing/internal/domain"
	"github.com/orshih6/billing/internal/models"
)

// Wirer is implemented by service-level views that know their wire form.
type Wirer interface{ Wire() any }

// Of converts a model (or view, or list) to its wire form. Values it does not
// know pass through unchanged (they are already plain response structs).
func Of(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case Wirer:
		return x.Wire()
	case *models.Tenant:
		return tenant(x)
	case *models.APIKey:
		return apiKey(x)
	case models.APIKey:
		return apiKey(&x)
	case *models.Customer:
		return customer(x)
	case models.Customer:
		return customer(&x)
	case *models.Plan:
		return plan(x)
	case models.Plan:
		return plan(&x)
	case *models.Price:
		return price(x)
	case *models.Subscription:
		return subscription(x)
	case *models.Invoice:
		return invoice(x)
	case models.Invoice:
		return invoice(&x)
	case *models.Payment:
		return payment(x)
	case models.Payment:
		return payment(&x)
	case *models.Coupon:
		return coupon(x)
	case models.Coupon:
		return coupon(&x)
	case *models.LedgerAccount:
		return ledgerAccount(x)
	case models.LedgerAccount:
		return ledgerAccount(&x)
	case models.LedgerTransaction:
		return ledgerTransaction(&x)
	case models.Event:
		return event(&x)
	case *models.WebhookEndpoint:
		return webhookEndpoint(x)
	case models.WebhookEndpoint:
		return webhookEndpoint(&x)
	case *models.WebhookDelivery:
		return webhookDelivery(x)
	case models.WebhookDelivery:
		return webhookDelivery(&x)
	}
	return v
}

// List is one page of results.
type List struct {
	Object  string `json:"object"`
	Data    []any  `json:"data"`
	HasMore bool   `json:"has_more"`
}

// ListOf converts each item.
func ListOf[T any](items []T, hasMore bool) List {
	out := List{Object: "list", Data: make([]any, len(items)), HasMore: hasMore}
	for i := range items {
		out.Data[i] = Of(items[i])
	}
	return out
}

type Tenant struct {
	Object            string                `json:"object"`
	ID                uuid.UUID             `json:"id"`
	Slug              string                `json:"slug"`
	Name              string                `json:"name"`
	DefaultCurrency   string                `json:"default_currency"`
	Status            string                `json:"status"`
	Settings          models.TenantSettings `json:"settings"`
	EffectiveSettings models.TenantSettings `json:"effective_settings"`
	CreatedAt         time.Time             `json:"created_at"`
}

func tenant(t *models.Tenant) Tenant {
	return Tenant{"tenant", t.ID, t.Slug, t.Name, t.DefaultCurrency, t.Status, t.Settings.Data(), t.EffectiveSettings(), t.CreatedAt}
}

type APIKey struct {
	Object     string     `json:"object"`
	ID         uuid.UUID  `json:"id"`
	TenantID   *uuid.UUID `json:"tenant_id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	LastFour   string     `json:"last_four"`
	Scopes     []string   `json:"scopes"`
	ExpiresAt  *time.Time `json:"expires_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	CreatedAt  time.Time  `json:"created_at"`
}

func apiKey(k *models.APIKey) APIKey {
	return APIKey{"api_key", k.ID, k.TenantID, k.Name, k.Prefix, k.LastFour, []string(k.Scopes), k.ExpiresAt, k.RevokedAt, k.LastUsedAt, k.CreatedAt}
}

type Customer struct {
	Object     string         `json:"object"`
	ID         uuid.UUID      `json:"id"`
	ExternalID *string        `json:"external_id"`
	Name       string         `json:"name"`
	Email      string         `json:"email"`
	Phone      string         `json:"phone"`
	TaxExempt  bool           `json:"tax_exempt"`
	TaxRateBps *int           `json:"tax_rate_bps"`
	Metadata   map[string]any `json:"metadata"`
	CreatedAt  time.Time      `json:"created_at"`
}

func customer(c *models.Customer) Customer {
	return Customer{"customer", c.ID, c.ExternalID, c.Name, c.Email, c.Phone, c.TaxExempt, c.TaxRateBps, meta(c.Metadata), c.CreatedAt}
}

type Price struct {
	Object        string          `json:"object"`
	ID            uuid.UUID       `json:"id"`
	PlanID        uuid.UUID       `json:"plan_id"`
	Nickname      string          `json:"nickname"`
	Amount        int64           `json:"amount"`
	Currency      string          `json:"currency"`
	Interval      domain.Interval `json:"interval"`
	IntervalCount int             `json:"interval_count"`
	TrialDays     int             `json:"trial_days"`
	Active        bool            `json:"active"`
	CreatedAt     time.Time       `json:"created_at"`
}

func price(p *models.Price) Price {
	return Price{"price", p.ID, p.PlanID, p.Nickname, p.Amount, p.Currency, p.Interval, p.IntervalCount, p.TrialDays, p.Active, p.CreatedAt}
}

type Plan struct {
	Object      string         `json:"object"`
	ID          uuid.UUID      `json:"id"`
	Code        string         `json:"code"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Kind        string         `json:"kind"`
	Active      bool           `json:"active"`
	Prices      []Price        `json:"prices"`
	Metadata    map[string]any `json:"metadata"`
	CreatedAt   time.Time      `json:"created_at"`
}

func plan(p *models.Plan) Plan {
	out := Plan{"plan", p.ID, p.Code, p.Name, p.Description, p.Kind, p.Active, []Price{}, meta(p.Metadata), p.CreatedAt}
	for i := range p.Prices {
		out.Prices = append(out.Prices, price(&p.Prices[i]))
	}
	return out
}

// Subscription is the subscription as stored. The API's subscription
// responses extend it with plan, add-ons, discount and latest invoice.
type Subscription struct {
	Object             string                    `json:"object"`
	ID                 uuid.UUID                 `json:"id"`
	CustomerID         uuid.UUID                 `json:"customer_id"`
	PlanID             uuid.UUID                 `json:"plan_id"`
	PriceID            uuid.UUID                 `json:"price_id"`
	Quantity           int                       `json:"quantity"`
	Status             domain.SubscriptionStatus `json:"status"`
	Entitled           bool                      `json:"entitled"`
	CurrentPeriodStart time.Time                 `json:"current_period_start"`
	CurrentPeriodEnd   time.Time                 `json:"current_period_end"`
	TrialEnd           *time.Time                `json:"trial_end"`
	CancelAtPeriodEnd  bool                      `json:"cancel_at_period_end"`
	CanceledAt         *time.Time                `json:"canceled_at"`
	EndedAt            *time.Time                `json:"ended_at"`
	PendingPriceID     *uuid.UUID                `json:"pending_price_id"`
	PendingQuantity    *int                      `json:"pending_quantity"`
	PendingFrom        *time.Time                `json:"pending_from"`
	DiscountID         *uuid.UUID                `json:"discount_id"`
	Metadata           map[string]any            `json:"metadata"`
	CreatedAt          time.Time                 `json:"created_at"`
}

// SubscriptionOf is exported for service views that extend it.
func SubscriptionOf(s *models.Subscription) Subscription { return subscription(s) }

func subscription(s *models.Subscription) Subscription {
	return Subscription{"subscription", s.ID, s.CustomerID, s.PlanID, s.PriceID, s.Quantity, s.Status, s.Status.Entitled(),
		s.CurrentPeriodStart, s.CurrentPeriodEnd, s.TrialEnd, s.CancelAtPeriodEnd, s.CanceledAt, s.EndedAt,
		s.PendingPriceID, s.PendingQuantity, s.PendingFrom, s.DiscountID, meta(s.Metadata), s.CreatedAt}
}

type InvoiceLine struct {
	ID                 uuid.UUID  `json:"id"`
	PriceID            *uuid.UUID `json:"price_id"`
	PlanID             *uuid.UUID `json:"plan_id"`
	SubscriptionItemID *uuid.UUID `json:"subscription_item_id"`
	Description        string     `json:"description"`
	Quantity           int64      `json:"quantity"`
	UnitAmount         int64      `json:"unit_amount"`
	Amount             int64      `json:"amount"`
	PeriodStart        *time.Time `json:"period_start"`
	PeriodEnd          *time.Time `json:"period_end"`
}

type Invoice struct {
	Object              string               `json:"object"`
	ID                  uuid.UUID            `json:"id"`
	CustomerID          uuid.UUID            `json:"customer_id"`
	SubscriptionID      *uuid.UUID           `json:"subscription_id"`
	Number              *string              `json:"number"`
	Reference           *string              `json:"reference"`
	Kind                domain.InvoiceKind   `json:"kind"`
	Status              domain.InvoiceStatus `json:"status"`
	Currency            string               `json:"currency"`
	Subtotal            int64                `json:"subtotal"`
	DiscountTotal       int64                `json:"discount_total"`
	DiscountDescription string               `json:"discount_description,omitempty"`
	TaxTotal            int64                `json:"tax_total"`
	TaxRateBps          int                  `json:"tax_rate_bps"`
	TaxName             string               `json:"tax_name,omitempty"`
	TaxInclusive        bool                 `json:"tax_inclusive"`
	Total               int64                `json:"total"`
	AmountPaid          int64                `json:"amount_paid"`
	CreditApplied       int64                `json:"credit_applied"`
	AmountDue           int64                `json:"amount_due"`
	PeriodStart         *time.Time           `json:"period_start"`
	PeriodEnd           *time.Time           `json:"period_end"`
	DueAt               *time.Time           `json:"due_at"`
	FinalizedAt         *time.Time           `json:"finalized_at"`
	PaidAt              *time.Time           `json:"paid_at"`
	VoidedAt            *time.Time           `json:"voided_at"`
	Memo                string               `json:"memo"`
	Lines               []InvoiceLine        `json:"lines"`
	Metadata            map[string]any       `json:"metadata"`
	CreatedAt           time.Time            `json:"created_at"`
}

// InvoiceOf is exported for service views that embed an invoice.
func InvoiceOf(i *models.Invoice) Invoice { return invoice(i) }

func invoice(i *models.Invoice) Invoice {
	out := Invoice{Object: "invoice", ID: i.ID, CustomerID: i.CustomerID, SubscriptionID: i.SubscriptionID, Number: i.Number,
		Reference: i.Reference, Kind: i.Kind, Status: i.Status, Currency: i.Currency, Subtotal: i.Subtotal,
		DiscountTotal: i.DiscountTotal, DiscountDescription: i.DiscountDescription, TaxTotal: i.TaxTotal,
		TaxRateBps: i.TaxRateBps, TaxName: i.TaxName, TaxInclusive: i.TaxInclusive, Total: i.Total,
		AmountPaid: i.AmountPaid, CreditApplied: i.CreditApplied, AmountDue: i.AmountDue, PeriodStart: i.PeriodStart,
		PeriodEnd: i.PeriodEnd, DueAt: i.DueAt, FinalizedAt: i.FinalizedAt, PaidAt: i.PaidAt, VoidedAt: i.VoidedAt,
		Memo: i.Memo, Lines: []InvoiceLine{}, Metadata: meta(i.Metadata), CreatedAt: i.CreatedAt}
	for _, l := range i.Lines {
		out.Lines = append(out.Lines, InvoiceLine{l.ID, l.PriceID, l.PlanID, l.SubscriptionItemID, l.Description,
			l.Quantity, l.UnitAmount, l.Amount, l.PeriodStart, l.PeriodEnd})
	}
	return out
}

type Payment struct {
	Object              string               `json:"object"`
	ID                  uuid.UUID            `json:"id"`
	InvoiceID           uuid.UUID            `json:"invoice_id"`
	CustomerID          uuid.UUID            `json:"customer_id"`
	Provider            string               `json:"provider"`
	ProviderAccountID   *uuid.UUID           `json:"provider_account_id"`
	ProviderRef         string               `json:"provider_ref"`
	Status              domain.PaymentStatus `json:"status"`
	Amount              int64                `json:"amount"`
	Currency            string               `json:"currency"`
	Instructions        string               `json:"instructions,omitempty"`
	PayURL              string               `json:"pay_url,omitempty"`
	ReturnURL           string               `json:"return_url,omitempty"`
	ExpiresAt           *time.Time           `json:"expires_at"`
	SettledAt           *time.Time           `json:"settled_at"`
	RefundedAt          *time.Time           `json:"refunded_at"`
	FailureReason       string               `json:"failure_reason,omitempty"`
	CancelReason        string               `json:"cancel_reason,omitempty"`
	ProviderCancel      string               `json:"provider_cancel,omitempty"`
	ProviderCancelError string               `json:"provider_cancel_error,omitempty"`
	Note                string               `json:"note,omitempty"`
	CreatedAt           time.Time            `json:"created_at"`
}

func payment(p *models.Payment) Payment {
	return Payment{"payment", p.ID, p.InvoiceID, p.CustomerID, p.Provider, p.ProviderAccountID, p.ProviderRef, p.Status,
		p.Amount, p.Currency, p.Instructions, p.PayURL, p.ReturnURL, p.ExpiresAt, p.SettledAt, p.RefundedAt,
		p.FailureReason, p.CancelReason, p.ProviderCancel, p.ProviderCancelError, p.Note, p.CreatedAt}
}

type Coupon struct {
	Object          string     `json:"object"`
	ID              uuid.UUID  `json:"id"`
	Code            string     `json:"code"`
	Name            string     `json:"name"`
	PercentOff      *int       `json:"percent_off"`
	AmountOff       *int64     `json:"amount_off"`
	Currency        string     `json:"currency,omitempty"`
	Duration        string     `json:"duration"`
	DurationPeriods *int       `json:"duration_periods"`
	PlanIDs         []string   `json:"plan_ids"`
	MaxRedemptions  *int       `json:"max_redemptions"`
	TimesRedeemed   int        `json:"times_redeemed"`
	RedeemBy        *time.Time `json:"redeem_by"`
	Active          bool       `json:"active"`
	CreatedAt       time.Time  `json:"created_at"`
}

func coupon(c *models.Coupon) Coupon {
	ids := []string(c.PlanIDs)
	if ids == nil {
		ids = []string{}
	}
	return Coupon{"coupon", c.ID, c.Code, c.Name, c.PercentOff, c.AmountOff, c.Currency, c.Duration, c.DurationPeriods,
		ids, c.MaxRedemptions, c.TimesRedeemed, c.RedeemBy, c.Active, c.CreatedAt}
}

type LedgerAccount struct {
	Object     string                   `json:"object"`
	ID         uuid.UUID                `json:"id"`
	Code       string                   `json:"code"`
	Name       string                   `json:"name"`
	Type       models.LedgerAccountType `json:"type"`
	Currency   string                   `json:"currency"`
	CustomerID *uuid.UUID               `json:"customer_id"`
	Balance    int64                    `json:"balance"`
}

func ledgerAccount(a *models.LedgerAccount) LedgerAccount {
	return LedgerAccount{"ledger_account", a.ID, a.Code, a.Name, a.Type, a.Currency, a.CustomerID, a.Balance}
}

type LedgerEntry struct {
	AccountID uuid.UUID `json:"account_id"`
	Debit     int64     `json:"debit"`
	Credit    int64     `json:"credit"`
}

type LedgerTransaction struct {
	Object         string        `json:"object"`
	ID             uuid.UUID     `json:"id"`
	Kind           string        `json:"kind"`
	IdempotencyKey string        `json:"idempotency_key"`
	Currency       string        `json:"currency"`
	InvoiceID      *uuid.UUID    `json:"invoice_id"`
	PaymentID      *uuid.UUID    `json:"payment_id"`
	CustomerID     *uuid.UUID    `json:"customer_id"`
	Description    string        `json:"description"`
	EffectiveAt    time.Time     `json:"effective_at"`
	Entries        []LedgerEntry `json:"entries"`
}

func ledgerTransaction(t *models.LedgerTransaction) LedgerTransaction {
	out := LedgerTransaction{"ledger_transaction", t.ID, t.Kind, t.IdempotencyKey, t.Currency, t.InvoiceID, t.PaymentID,
		t.CustomerID, t.Description, t.EffectiveAt, []LedgerEntry{}}
	for _, e := range t.Entries {
		out.Entries = append(out.Entries, LedgerEntry{e.AccountID, e.Debit, e.Credit})
	}
	return out
}

type Event struct {
	Object    string    `json:"object"`
	ID        uuid.UUID `json:"id"`
	Type      string    `json:"type"`
	Data      any       `json:"data"`
	CreatedAt time.Time `json:"created_at"`
}

func event(e *models.Event) Event {
	return Event{"event", e.ID, e.Type, rawJSON(e.Data), e.CreatedAt}
}

type WebhookEndpoint struct {
	Object      string    `json:"object"`
	ID          uuid.UUID `json:"id"`
	URL         string    `json:"url"`
	Description string    `json:"description"`
	EventTypes  []string  `json:"event_types"`
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"created_at"`
}

func webhookEndpoint(e *models.WebhookEndpoint) WebhookEndpoint {
	types := []string(e.EventTypes)
	if types == nil {
		types = []string{}
	}
	return WebhookEndpoint{"webhook_endpoint", e.ID, e.URL, e.Description, types, e.Enabled, e.CreatedAt}
}

type WebhookDelivery struct {
	Object         string     `json:"object"`
	ID             uuid.UUID  `json:"id"`
	EventID        uuid.UUID  `json:"event_id"`
	EndpointID     uuid.UUID  `json:"endpoint_id"`
	Status         string     `json:"status"`
	Attempts       int        `json:"attempts"`
	NextAttemptAt  time.Time  `json:"next_attempt_at"`
	LastStatusCode int        `json:"last_status_code"`
	LastError      string     `json:"last_error"`
	DeliveredAt    *time.Time `json:"delivered_at"`
	CreatedAt      time.Time  `json:"created_at"`
}

func webhookDelivery(d *models.WebhookDelivery) WebhookDelivery {
	return WebhookDelivery{"webhook_delivery", d.ID, d.EventID, d.EndpointID, d.Status, d.Attempts, d.NextAttemptAt,
		d.LastStatusCode, d.LastError, d.DeliveredAt, d.CreatedAt}
}

func meta(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}
