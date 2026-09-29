// Package client is the Go client for the billing service. It has its own wire
// types and imports nothing from the server, so any service can depend on it.
//
//	c := client.New("https://billing.example.com", os.Getenv("BILLING_API_KEY"))
//	cust, _ := c.EnsureCustomer(ctx, client.CustomerInput{ExternalID: "user-42", Name: "Bold"})
//	sub, _ := c.CreateSubscription(ctx, client.CreateSubscriptionInput{CustomerID: cust.ID, PlanCode: "pro"})
//	pay, _ := c.StartPayment(ctx, sub.LatestInvoice.ID, client.StartPaymentInput{Provider: "mock", ReturnURL: "https://app/return"})
//	// redirect the user to pay.PayURL; later:
//	ents, _ := c.EntitlementsByExternalID(ctx, "user-42")
package client

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client talks to the billing API.
type Client struct {
	BaseURL string
	APIKey  string
	// TenantID is only needed with a platform key.
	TenantID string
	HTTP     *http.Client
}

// New builds a client.
func New(baseURL, apiKey string) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), APIKey: apiKey, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// Error is an API error response.
type Error struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return fmt.Sprintf("billing: %d %s: %s", e.Status, e.Code, e.Message) }

// IsCode reports whether err is an API error with the given code.
func IsCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// RequestOption tweaks one request.
type RequestOption func(*http.Request)

// WithIdempotencyKey makes a POST safe to retry.
func WithIdempotencyKey(key string) RequestOption {
	return func(r *http.Request) { r.Header.Set("Idempotency-Key", key) }
}

func (c *Client) do(ctx context.Context, method, path string, in, out any, opts ...RequestOption) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", c.APIKey)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.TenantID != "" {
		req.Header.Set("X-Tenant-Id", c.TenantID)
	}
	for _, o := range opts {
		o(req)
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}
	if res.StatusCode >= 400 {
		var eb struct {
			Error Error `json:"error"`
		}
		if json.Unmarshal(raw, &eb) != nil || eb.Error.Code == "" {
			eb.Error = Error{Code: "http_error", Message: strings.TrimSpace(string(raw))}
		}
		eb.Error.Status = res.StatusCode
		return &eb.Error
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// ---- wire types ----------------------------------------------------------------

type Customer struct {
	ID         string         `json:"id"`
	ExternalID *string        `json:"external_id"`
	Name       string         `json:"name"`
	Email      string         `json:"email"`
	Phone      string         `json:"phone"`
	Metadata   map[string]any `json:"metadata"`
	TaxExempt  bool           `json:"tax_exempt"`
	TaxRateBps *int           `json:"tax_rate_bps"`
	CreatedAt  time.Time      `json:"created_at"`
}

type CustomerInput struct {
	ExternalID string         `json:"external_id,omitempty"`
	Name       string         `json:"name,omitempty"`
	Email      string         `json:"email,omitempty"`
	Phone      string         `json:"phone,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
	TaxExempt  *bool          `json:"tax_exempt,omitempty"`
	// TaxRateBps overrides the tenant tax rate (1000 = 10%); -1 clears it.
	TaxRateBps *int `json:"tax_rate_bps,omitempty"`
}

type Balance struct {
	Currency string `json:"currency"`
	Owed     int64  `json:"owed"`
	Credit   int64  `json:"credit"`
}

type Entitlement struct {
	PlanID   string `json:"plan_id"`
	PlanCode string `json:"plan_code"`
	// Kind is "base" or "addon"; Quantity is seats / add-on units.
	Kind              string    `json:"kind"`
	Quantity          int       `json:"quantity"`
	SubscriptionID    string    `json:"subscription_id"`
	Status            string    `json:"status"`
	CurrentPeriodEnd  time.Time `json:"current_period_end"`
	CancelAtPeriodEnd bool      `json:"cancel_at_period_end"`
}

type Price struct {
	ID            string `json:"id"`
	PlanID        string `json:"plan_id"`
	Nickname      string `json:"nickname"`
	Amount        int64  `json:"amount"`
	Currency      string `json:"currency"`
	Interval      string `json:"interval"`
	IntervalCount int    `json:"interval_count"`
	TrialDays     int    `json:"trial_days"`
	Active        bool   `json:"active"`
}

type Plan struct {
	ID          string  `json:"id"`
	Code        string  `json:"code"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Kind        string  `json:"kind"`
	Active      bool    `json:"active"`
	Prices      []Price `json:"prices"`
}

type PriceInput struct {
	Nickname      string `json:"nickname,omitempty"`
	Amount        int64  `json:"amount"`
	Currency      string `json:"currency,omitempty"`
	Interval      string `json:"interval"`
	IntervalCount int    `json:"interval_count,omitempty"`
	TrialDays     int    `json:"trial_days,omitempty"`
}

type PlanInput struct {
	Code        string `json:"code"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Kind is "base" (default) or "addon".
	Kind   string       `json:"kind,omitempty"`
	Prices []PriceInput `json:"prices,omitempty"`
}

type InvoiceLine struct {
	ID          string  `json:"id"`
	PriceID     *string `json:"price_id"`
	Description string  `json:"description"`
	Quantity    int64   `json:"quantity"`
	UnitAmount  int64   `json:"unit_amount"`
	Amount      int64   `json:"amount"`
}

type Invoice struct {
	ID             string        `json:"id"`
	CustomerID     string        `json:"customer_id"`
	SubscriptionID *string       `json:"subscription_id"`
	Number         *string       `json:"number"`
	Reference      *string       `json:"reference"`
	Kind           string        `json:"kind"`
	Status         string        `json:"status"`
	Currency       string        `json:"currency"`
	Subtotal       int64         `json:"subtotal"`
	DiscountTotal  int64         `json:"discount_total"`
	TaxTotal       int64         `json:"tax_total"`
	TaxRateBps     int           `json:"tax_rate_bps"`
	TaxName        string        `json:"tax_name"`
	TaxInclusive   bool          `json:"tax_inclusive"`
	Total          int64         `json:"total"`
	AmountPaid     int64         `json:"amount_paid"`
	CreditApplied  int64         `json:"credit_applied"`
	AmountDue      int64         `json:"amount_due"`
	PeriodStart    *time.Time    `json:"period_start"`
	PeriodEnd      *time.Time    `json:"period_end"`
	DueAt          *time.Time    `json:"due_at"`
	PaidAt         *time.Time    `json:"paid_at"`
	Lines          []InvoiceLine `json:"lines"`
}

type LineInput struct {
	PriceID     string `json:"price_id,omitempty"`
	Description string `json:"description,omitempty"`
	Quantity    int64  `json:"quantity,omitempty"`
	UnitAmount  int64  `json:"unit_amount,omitempty"`
}

type InvoiceInput struct {
	CustomerID string      `json:"customer_id"`
	Currency   string      `json:"currency,omitempty"`
	Lines      []LineInput `json:"lines"`
	DueDays    *int        `json:"due_days,omitempty"`
	Memo       string      `json:"memo,omitempty"`
	Finalize   bool        `json:"finalize"`
	PromoCode  string      `json:"promo_code,omitempty"`
}

type Subscription struct {
	ID                 string     `json:"id"`
	CustomerID         string     `json:"customer_id"`
	PlanID             string     `json:"plan_id"`
	PlanCode           string     `json:"plan_code"`
	PriceID            string     `json:"price_id"`
	PendingPriceID     *string    `json:"pending_price_id"`
	Quantity           int        `json:"quantity"`
	PendingQuantity    *int       `json:"pending_quantity"`
	PendingFrom        *time.Time `json:"pending_from"`
	AddOns             []AddOn    `json:"add_ons"`
	Discount           *Discount  `json:"discount"`
	Status             string     `json:"status"`
	Entitled           bool       `json:"entitled"`
	CurrentPeriodStart time.Time  `json:"current_period_start"`
	CurrentPeriodEnd   time.Time  `json:"current_period_end"`
	TrialEnd           *time.Time `json:"trial_end"`
	CancelAtPeriodEnd  bool       `json:"cancel_at_period_end"`
	EndedAt            *time.Time `json:"ended_at"`
	LatestInvoice      *Invoice   `json:"latest_invoice"`
}

// AddOn is an add-on on a subscription.
type AddOn struct {
	ID              string `json:"id"`
	PlanID          string `json:"plan_id"`
	PlanCode        string `json:"plan_code"`
	PriceID         string `json:"price_id"`
	Quantity        int    `json:"quantity"`
	PendingQuantity *int   `json:"pending_quantity"`
	UnitAmount      int64  `json:"unit_amount"`
	Currency        string `json:"currency"`
}

// AddOnInput attaches an add-on price; Quantity 0 removes it.
type AddOnInput struct {
	PriceID  string `json:"price_id"`
	Quantity int    `json:"quantity"`
}

// Discount is the coupon applied to a subscription.
type Discount struct {
	ID          string `json:"id"`
	Code        string `json:"code"`
	Name        string `json:"name"`
	PercentOff  *int   `json:"percent_off"`
	AmountOff   *int64 `json:"amount_off"`
	Duration    string `json:"duration"`
	PeriodsUsed int64  `json:"periods_used"`
	Description string `json:"description"`
}

// Coupon is a promo code.
type Coupon struct {
	ID              string     `json:"id"`
	Code            string     `json:"code"`
	Name            string     `json:"name"`
	PercentOff      *int       `json:"percent_off"`
	AmountOff       *int64     `json:"amount_off"`
	Currency        string     `json:"currency"`
	Duration        string     `json:"duration"`
	DurationPeriods *int       `json:"duration_periods"`
	PlanIDs         []string   `json:"plan_ids"`
	MaxRedemptions  *int       `json:"max_redemptions"`
	TimesRedeemed   int        `json:"times_redeemed"`
	RedeemBy        *time.Time `json:"redeem_by"`
	Active          bool       `json:"active"`
}

// CouponInput creates a coupon: exactly one of PercentOff / AmountOff.
type CouponInput struct {
	Code            string     `json:"code"`
	Name            string     `json:"name,omitempty"`
	PercentOff      *int       `json:"percent_off,omitempty"`
	AmountOff       *int64     `json:"amount_off,omitempty"`
	Currency        string     `json:"currency,omitempty"`
	Duration        string     `json:"duration,omitempty"` // once | repeating | forever
	DurationPeriods *int       `json:"duration_periods,omitempty"`
	PlanIDs         []string   `json:"plan_ids,omitempty"`
	MaxRedemptions  *int       `json:"max_redemptions,omitempty"`
	RedeemBy        *time.Time `json:"redeem_by,omitempty"`
}

type CreateSubscriptionInput struct {
	CustomerID string         `json:"customer_id"`
	PriceID    string         `json:"price_id,omitempty"`
	PlanCode   string         `json:"plan_code,omitempty"`
	Currency   string         `json:"currency,omitempty"`
	Quantity   int            `json:"quantity,omitempty"`
	AddOns     []AddOnInput   `json:"add_ons,omitempty"`
	PromoCode  string         `json:"promo_code,omitempty"`
	TrialDays  *int           `json:"trial_days,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
}

type Payment struct {
	ID           string     `json:"id"`
	InvoiceID    string     `json:"invoice_id"`
	CustomerID   string     `json:"customer_id"`
	Provider     string     `json:"provider"`
	ProviderRef  string     `json:"provider_ref"`
	Status       string     `json:"status"`
	Amount       int64      `json:"amount"`
	Currency     string     `json:"currency"`
	Instructions string     `json:"instructions"`
	PayURL       string     `json:"pay_url"`
	ExpiresAt    *time.Time `json:"expires_at"`
	SettledAt    *time.Time `json:"settled_at"`
	Note         string     `json:"note"`
	// CancelReason is set when Status is "canceled".
	CancelReason string `json:"cancel_reason"`
	// ProviderCancel tracks closing the intent at the gateway:
	// "" | pending | done | already_paid | failed.
	ProviderCancel      string `json:"provider_cancel"`
	ProviderCancelError string `json:"provider_cancel_error"`
}

type StartPaymentInput struct {
	Provider  string `json:"provider"`
	ReturnURL string `json:"return_url,omitempty"`
}

type ManualPaymentInput struct {
	InvoiceID  string     `json:"invoice_id"`
	Amount     int64      `json:"amount"`
	Reference  string     `json:"reference,omitempty"`
	Note       string     `json:"note,omitempty"`
	ReceivedAt *time.Time `json:"received_at,omitempty"`
}

// List is one page.
type List[T any] struct {
	Data    []T  `json:"data"`
	HasMore bool `json:"has_more"`
}

// ListOptions pages a list.
type ListOptions struct {
	Limit         int
	StartingAfter string
	Filters       map[string]string
}

func (o *ListOptions) query() string {
	v := url.Values{}
	if o != nil {
		if o.Limit > 0 {
			v.Set("limit", strconv.Itoa(o.Limit))
		}
		if o.StartingAfter != "" {
			v.Set("starting_after", o.StartingAfter)
		}
		for k, val := range o.Filters {
			v.Set(k, val)
		}
	}
	if len(v) == 0 {
		return ""
	}
	return "?" + v.Encode()
}

// ---- customers -----------------------------------------------------------------

func (c *Client) CreateCustomer(ctx context.Context, in CustomerInput, opts ...RequestOption) (*Customer, error) {
	var out Customer
	return &out, c.do(ctx, "POST", "/customers", in, &out, opts...)
}

func (c *Client) GetCustomer(ctx context.Context, id string) (*Customer, error) {
	var out Customer
	return &out, c.do(ctx, "GET", "/customers/"+url.PathEscape(id), nil, &out)
}

func (c *Client) GetCustomerByExternalID(ctx context.Context, externalID string) (*Customer, error) {
	var out Customer
	return &out, c.do(ctx, "GET", "/customers/by-external/"+url.PathEscape(externalID), nil, &out)
}

// EnsureCustomer returns the customer with in.ExternalID, creating it if it
// does not exist. Safe to call on every sign-in.
func (c *Client) EnsureCustomer(ctx context.Context, in CustomerInput) (*Customer, error) {
	if in.ExternalID == "" {
		return nil, errors.New("billing: EnsureCustomer needs an ExternalID")
	}
	cust, err := c.GetCustomerByExternalID(ctx, in.ExternalID)
	if err == nil {
		return cust, nil
	}
	if !IsCode(err, "customer_not_found") {
		return nil, err
	}
	cust, err = c.CreateCustomer(ctx, in)
	if IsCode(err, "external_id_taken") { // lost a race with another sign-in
		return c.GetCustomerByExternalID(ctx, in.ExternalID)
	}
	return cust, err
}

func (c *Client) UpdateCustomer(ctx context.Context, id string, in CustomerInput) (*Customer, error) {
	var out Customer
	return &out, c.do(ctx, "PATCH", "/customers/"+url.PathEscape(id), in, &out)
}

func (c *Client) ListCustomers(ctx context.Context, opts *ListOptions) (*List[Customer], error) {
	var out List[Customer]
	return &out, c.do(ctx, "GET", "/customers"+opts.query(), nil, &out)
}

func (c *Client) CustomerBalances(ctx context.Context, id string) ([]Balance, error) {
	var out struct {
		Balances []Balance `json:"balances"`
	}
	return out.Balances, c.do(ctx, "GET", "/customers/"+url.PathEscape(id)+"/balance", nil, &out)
}

func (c *Client) GrantCredit(ctx context.Context, customerID string, amount int64, currency, reason string, opts ...RequestOption) ([]Balance, error) {
	var out struct {
		Balances []Balance `json:"balances"`
	}
	in := map[string]any{"amount": amount, "currency": currency, "reason": reason}
	return out.Balances, c.do(ctx, "POST", "/customers/"+url.PathEscape(customerID)+"/credits", in, &out, opts...)
}

// Entitlements lists the plans a customer may use right now.
func (c *Client) Entitlements(ctx context.Context, customerID string) ([]Entitlement, error) {
	var out struct {
		Entitlements []Entitlement `json:"entitlements"`
	}
	return out.Entitlements, c.do(ctx, "GET", "/customers/"+url.PathEscape(customerID)+"/entitlements", nil, &out)
}

// EntitlementsByExternalID is Entitlements keyed by your own user id.
func (c *Client) EntitlementsByExternalID(ctx context.Context, externalID string) ([]Entitlement, error) {
	var out struct {
		Entitlements []Entitlement `json:"entitlements"`
	}
	return out.Entitlements, c.do(ctx, "GET", "/customers/by-external/"+url.PathEscape(externalID)+"/entitlements", nil, &out)
}

// HasPlan reports whether the customer is entitled to a plan code.
func HasPlan(ents []Entitlement, planCode string) bool {
	for _, e := range ents {
		if e.PlanCode == planCode {
			return true
		}
	}
	return false
}

// ---- plans -----------------------------------------------------------------------

func (c *Client) CreatePlan(ctx context.Context, in PlanInput, opts ...RequestOption) (*Plan, error) {
	var out Plan
	return &out, c.do(ctx, "POST", "/plans", in, &out, opts...)
}

func (c *Client) GetPlanByCode(ctx context.Context, code string) (*Plan, error) {
	var out Plan
	return &out, c.do(ctx, "GET", "/plans/by-code/"+url.PathEscape(code), nil, &out)
}

func (c *Client) ListPlans(ctx context.Context, opts *ListOptions) (*List[Plan], error) {
	var out List[Plan]
	return &out, c.do(ctx, "GET", "/plans"+opts.query(), nil, &out)
}

func (c *Client) AddPrice(ctx context.Context, planID string, in PriceInput) (*Price, error) {
	var out Price
	return &out, c.do(ctx, "POST", "/plans/"+url.PathEscape(planID)+"/prices", in, &out)
}

// ---- subscriptions ----------------------------------------------------------------

func (c *Client) CreateSubscription(ctx context.Context, in CreateSubscriptionInput, opts ...RequestOption) (*Subscription, error) {
	var out Subscription
	return &out, c.do(ctx, "POST", "/subscriptions", in, &out, opts...)
}

func (c *Client) GetSubscription(ctx context.Context, id string) (*Subscription, error) {
	var out Subscription
	return &out, c.do(ctx, "GET", "/subscriptions/"+url.PathEscape(id), nil, &out)
}

func (c *Client) ListSubscriptions(ctx context.Context, opts *ListOptions) (*List[Subscription], error) {
	var out List[Subscription]
	return &out, c.do(ctx, "GET", "/subscriptions"+opts.query(), nil, &out)
}

// CancelSubscription ends a subscription now or at the end of its period.
func (c *Client) CancelSubscription(ctx context.Context, id string, atPeriodEnd bool) (*Subscription, error) {
	var out Subscription
	return &out, c.do(ctx, "POST", "/subscriptions/"+url.PathEscape(id)+"/cancel", map[string]bool{"at_period_end": atPeriodEnd}, &out)
}

func (c *Client) ResumeSubscription(ctx context.Context, id string) (*Subscription, error) {
	var out Subscription
	return &out, c.do(ctx, "POST", "/subscriptions/"+url.PathEscape(id)+"/resume", nil, &out)
}

// ChangePrice moves a subscription; when is "now", "period_end" or "" (auto).
func (c *Client) ChangePrice(ctx context.Context, id, priceID, when string) (*Subscription, error) {
	var out Subscription
	return &out, c.do(ctx, "POST", "/subscriptions/"+url.PathEscape(id)+"/change-price", map[string]string{"price_id": priceID, "when": when}, &out)
}

// SetQuantity changes seats: more now (prorated), fewer at renewal.
func (c *Client) SetQuantity(ctx context.Context, id string, quantity int) (*Subscription, error) {
	var out Subscription
	return &out, c.do(ctx, "POST", "/subscriptions/"+url.PathEscape(id)+"/quantity", map[string]int{"quantity": quantity}, &out)
}

// SetAddOn adds an add-on or changes its quantity; 0 removes it at renewal.
func (c *Client) SetAddOn(ctx context.Context, id string, in AddOnInput) (*Subscription, error) {
	var out Subscription
	return &out, c.do(ctx, "POST", "/subscriptions/"+url.PathEscape(id)+"/add-ons", in, &out)
}

// ApplyPromoCode puts a coupon on a subscription, replacing any other.
func (c *Client) ApplyPromoCode(ctx context.Context, id, code string) (*Subscription, error) {
	var out Subscription
	return &out, c.do(ctx, "POST", "/subscriptions/"+url.PathEscape(id)+"/discount", map[string]string{"promo_code": code}, &out)
}

func (c *Client) RemoveDiscount(ctx context.Context, id string) (*Subscription, error) {
	var out Subscription
	return &out, c.do(ctx, "DELETE", "/subscriptions/"+url.PathEscape(id)+"/discount", nil, &out)
}

// SearchResult groups matches by resource.
type SearchResult struct {
	Query         string         `json:"query"`
	Customers     []Customer     `json:"customers"`
	Invoices      []Invoice      `json:"invoices"`
	Payments      []Payment      `json:"payments"`
	Subscriptions []Subscription `json:"subscriptions"`
}

// Search finds customers, invoices, payments and subscriptions matching q
// (names, emails, external ids, invoice numbers, references, or an id).
func (c *Client) Search(ctx context.Context, q string) (*SearchResult, error) {
	var out SearchResult
	return &out, c.do(ctx, "GET", "/search?"+url.Values{"q": {q}}.Encode(), nil, &out)
}

// ---- coupons -------------------------------------------------------------------------

func (c *Client) CreateCoupon(ctx context.Context, in CouponInput) (*Coupon, error) {
	var out Coupon
	return &out, c.do(ctx, "POST", "/coupons", in, &out)
}

func (c *Client) ListCoupons(ctx context.Context, opts *ListOptions) (*List[Coupon], error) {
	var out List[Coupon]
	return &out, c.do(ctx, "GET", "/coupons"+opts.query(), nil, &out)
}

// CheckPromoCode validates a code for a checkout without redeeming it.
func (c *Client) CheckPromoCode(ctx context.Context, code, currency string) (*Coupon, error) {
	var out Coupon
	q := url.Values{"code": {code}, "currency": {currency}}
	return &out, c.do(ctx, "GET", "/coupons/check?"+q.Encode(), nil, &out)
}

// ---- invoices & payments -------------------------------------------------------------

func (c *Client) CreateInvoice(ctx context.Context, in InvoiceInput, opts ...RequestOption) (*Invoice, error) {
	var out Invoice
	return &out, c.do(ctx, "POST", "/invoices", in, &out, opts...)
}

func (c *Client) GetInvoice(ctx context.Context, id string) (*Invoice, error) {
	var out Invoice
	return &out, c.do(ctx, "GET", "/invoices/"+url.PathEscape(id), nil, &out)
}

func (c *Client) ListInvoices(ctx context.Context, opts *ListOptions) (*List[Invoice], error) {
	var out List[Invoice]
	return &out, c.do(ctx, "GET", "/invoices"+opts.query(), nil, &out)
}

func (c *Client) FinalizeInvoice(ctx context.Context, id string) (*Invoice, error) {
	var out Invoice
	return &out, c.do(ctx, "POST", "/invoices/"+url.PathEscape(id)+"/finalize", nil, &out)
}

func (c *Client) VoidInvoice(ctx context.Context, id, reason string) (*Invoice, error) {
	var out Invoice
	return &out, c.do(ctx, "POST", "/invoices/"+url.PathEscape(id)+"/void", map[string]string{"reason": reason}, &out)
}

// StartPayment begins collecting what is due on an invoice.
func (c *Client) StartPayment(ctx context.Context, invoiceID string, in StartPaymentInput, opts ...RequestOption) (*Payment, error) {
	var out Payment
	return &out, c.do(ctx, "POST", "/invoices/"+url.PathEscape(invoiceID)+"/payments", in, &out, opts...)
}

func (c *Client) GetPayment(ctx context.Context, id string) (*Payment, error) {
	var out Payment
	return &out, c.do(ctx, "GET", "/payments/"+url.PathEscape(id), nil, &out)
}

// RecordManualPayment needs the payments:manual scope.
func (c *Client) RecordManualPayment(ctx context.Context, in ManualPaymentInput, opts ...RequestOption) (*Payment, error) {
	var out Payment
	return &out, c.do(ctx, "POST", "/payments/manual", in, &out, opts...)
}

// SimulateMockPayment drives a mock payment: outcome "succeed" or "fail".
func (c *Client) SimulateMockPayment(ctx context.Context, paymentID, outcome string) (*Payment, error) {
	var out Payment
	return &out, c.do(ctx, "POST", "/payments/"+url.PathEscape(paymentID)+"/mock/"+url.PathEscape(outcome), nil, &out)
}

func (c *Client) RefundPayment(ctx context.Context, id, reason string) (*Payment, error) {
	var out Payment
	return &out, c.do(ctx, "POST", "/payments/"+url.PathEscape(id)+"/refund", map[string]string{"reason": reason}, &out)
}

// ---- webhooks ------------------------------------------------------------------------

// WebhookEvent is the body POSTed to your endpoint.
type WebhookEvent struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	TenantID  string          `json:"tenant_id"`
	CreatedAt time.Time       `json:"created_at"`
	Data      json.RawMessage `json:"data"`
}

// ErrInvalidSignature means a webhook was not signed with your secret, or is
// too old.
var ErrInvalidSignature = errors.New("billing: invalid webhook signature")

// VerifyWebhook checks X-Billing-Signature ("t=<unix>,v1=<hex>") against the
// raw body and rejects anything older than tolerance (use 5 minutes), then
// decodes the event.
func VerifyWebhook(body []byte, signatureHeader, secret string, tolerance time.Duration) (*WebhookEvent, error) {
	var ts, sig string
	for _, part := range strings.Split(signatureHeader, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch k {
		case "t":
			ts = v
		case "v1":
			sig = v
		}
	}
	unix, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || sig == "" {
		return nil, ErrInvalidSignature
	}
	if tolerance > 0 && time.Since(time.Unix(unix, 0)).Abs() > tolerance {
		return nil, ErrInvalidSignature
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(sig)) {
		return nil, ErrInvalidSignature
	}
	var ev WebhookEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return nil, err
	}
	return &ev, nil
}
