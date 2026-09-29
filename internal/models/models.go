// Package models holds the GORM rows. Every tenant-owned row carries TenantID,
// and every query that reads one must filter by it: the tenant always comes from
// the authenticated API key, never from the request body.
package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	"github.com/orshih6/billing/internal/domain"
)

// Base gives every row a UUID primary key and timestamps.
type Base struct {
	ID        uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`
	CreatedAt time.Time `gorm:"not null" json:"created_at"`
	UpdatedAt time.Time `gorm:"not null" json:"updated_at"`
}

// BeforeCreate assigns an id when the caller has not.
func (b *Base) BeforeCreate(*gorm.DB) error {
	if b.ID == uuid.Nil {
		b.ID = uuid.New()
	}
	return nil
}

// TenantSettings are the knobs a tenant can turn. Zero values fall back to the
// defaults in Defaults().
type TenantSettings struct {
	// InvoicePrefix starts every invoice number, e.g. "ACME" → ACME-000042.
	InvoicePrefix string `json:"invoice_prefix,omitempty"`
	// InvoiceDueDays is how long a one-off or proration invoice stays payable.
	InvoiceDueDays int `json:"invoice_due_days,omitempty"`
	// RenewalLeadDays is how early a renewal invoice is raised before the
	// period ends.
	RenewalLeadDays int `json:"renewal_lead_days,omitempty"`
	// GraceDays is how long a subscription stays past_due (still entitled)
	// after its period ends unpaid, before it expires. nil means the default;
	// 0 is a real setting (lapse the moment the period ends).
	GraceDays *int `json:"grace_days,omitempty"`
	// IncompleteExpiryHours is how long a new subscription waits for its first
	// payment before it expires.
	IncompleteExpiryHours int `json:"incomplete_expiry_hours,omitempty"`
	// PaymentExpiryHours is how long a pending payment intent stays open.
	PaymentExpiryHours int `json:"payment_expiry_hours,omitempty"`
	// LapseAction is what happens to the unpaid renewal invoice when a
	// subscription expires: "void" (default) or "uncollectible".
	LapseAction string `json:"lapse_action,omitempty"`
	// MockEnabled allows the mock provider when APP_ENV=production.
	MockEnabled bool `json:"mock_enabled,omitempty"`
	// Tax is optional. Nil or disabled means no tax is added.
	Tax *TaxSettings `json:"tax,omitempty"`
	// ManualInstructions is shown to a customer who pays by manual transfer,
	// e.g. the bank account to send money to. {reference} and {amount} are
	// substituted.
	ManualInstructions string `json:"manual_instructions,omitempty"`
}

// TaxSettings is one simple tax rate for a tenant.
type TaxSettings struct {
	Enabled bool `json:"enabled"`
	// Name is shown on invoices, e.g. "VAT".
	Name string `json:"name,omitempty"`
	// RateBps is the rate in basis points: 1000 = 10%.
	RateBps int `json:"rate_bps"`
	// Inclusive means prices already contain the tax (common for consumer
	// prices); otherwise tax is added on top.
	Inclusive bool `json:"inclusive"`
}

// Defaults returns the settings with every unset knob filled in.
func (s TenantSettings) Defaults(slug string) TenantSettings {
	if s.InvoicePrefix == "" {
		s.InvoicePrefix = defaultPrefix(slug)
	}
	if s.InvoiceDueDays <= 0 {
		s.InvoiceDueDays = 7
	}
	if s.RenewalLeadDays <= 0 {
		s.RenewalLeadDays = 7
	}
	if s.GraceDays == nil || *s.GraceDays < 0 {
		d := 3
		s.GraceDays = &d
	}
	if s.IncompleteExpiryHours <= 0 {
		s.IncompleteExpiryHours = 48
	}
	if s.PaymentExpiryHours <= 0 {
		s.PaymentExpiryHours = 24
	}
	if s.LapseAction != "uncollectible" {
		s.LapseAction = "void"
	}
	if s.ManualInstructions == "" {
		s.ManualInstructions = "Transfer {amount} and write {reference} in the payment description."
	}
	return s
}

func defaultPrefix(slug string) string {
	out := make([]byte, 0, 6)
	for i := 0; i < len(slug) && len(out) < 6; i++ {
		c := slug[i]
		switch {
		case c >= 'a' && c <= 'z':
			out = append(out, c-'a'+'A')
		case c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return "INV"
	}
	return string(out)
}

// Tenant is one integrating system: a SaaS that bills its own customers here.
type Tenant struct {
	Base
	Slug            string                             `gorm:"size:64;not null;uniqueIndex" json:"slug"`
	Name            string                             `gorm:"size:200;not null" json:"name"`
	DefaultCurrency string                             `gorm:"size:3;not null" json:"default_currency"`
	Settings        datatypes.JSONType[TenantSettings] `gorm:"type:jsonb;not null" json:"settings"`
	Status          string                             `gorm:"size:16;not null;default:active" json:"status"`
	// InvoiceSeq is the last invoice number issued, bumped under row lock.
	InvoiceSeq int64 `gorm:"not null;default:0" json:"-"`
}

// Grace returns the grace window, with the default applied.
func (s TenantSettings) Grace() int {
	if s.GraceDays == nil {
		return 3
	}
	return *s.GraceDays
}

// EffectiveSettings returns the settings with defaults applied.
func (t *Tenant) EffectiveSettings() TenantSettings {
	return t.Settings.Data().Defaults(t.Slug)
}

// APIKey is a credential. TenantID nil marks a platform key, which manages
// tenants and may act inside any tenant.
type APIKey struct {
	Base
	TenantID   *uuid.UUID                  `gorm:"type:uuid;index" json:"tenant_id"`
	Name       string                      `gorm:"size:120;not null" json:"name"`
	Prefix     string                      `gorm:"size:32;not null" json:"prefix"`
	LastFour   string                      `gorm:"size:4;not null" json:"last_four"`
	Hash       string                      `gorm:"size:64;not null;uniqueIndex" json:"-"`
	Scopes     datatypes.JSONSlice[string] `gorm:"type:jsonb;not null" json:"scopes"`
	ExpiresAt  *time.Time                  `json:"expires_at"`
	RevokedAt  *time.Time                  `json:"revoked_at"`
	LastUsedAt *time.Time                  `json:"last_used_at"`
}

// Expired reports whether the key is past its expiry.
func (k *APIKey) Expired() bool {
	return k.ExpiresAt != nil && time.Now().After(*k.ExpiresAt)
}

// Customer is someone a tenant bills. ExternalID is the tenant's own user id.
type Customer struct {
	Base
	TenantID   uuid.UUID         `gorm:"type:uuid;not null;uniqueIndex:ux_customer_external,priority:1" json:"tenant_id"`
	ExternalID *string           `gorm:"size:200;uniqueIndex:ux_customer_external,priority:2" json:"external_id"`
	Name       string            `gorm:"size:200;not null" json:"name"`
	Email      string            `gorm:"size:320" json:"email"`
	Phone      string            `gorm:"size:40" json:"phone"`
	Metadata   datatypes.JSONMap `gorm:"type:jsonb" json:"metadata"`
	ArchivedAt *time.Time        `json:"archived_at"`
	// TaxExempt customers are never charged tax.
	TaxExempt bool `gorm:"not null;default:false" json:"tax_exempt"`
	// TaxRateBps overrides the tenant's rate for this customer (e.g. another
	// country), even when tenant tax is disabled.
	TaxRateBps *int `json:"tax_rate_bps"`
}

// Plan is a product a tenant sells. Its money lives on Prices.
type Plan struct {
	Base
	TenantID    uuid.UUID `gorm:"type:uuid;not null;uniqueIndex:ux_plan_code,priority:1" json:"tenant_id"`
	Code        string    `gorm:"size:64;not null;uniqueIndex:ux_plan_code,priority:2" json:"code"`
	Name        string    `gorm:"size:200;not null" json:"name"`
	Description string    `gorm:"size:2000" json:"description"`
	Active      bool      `gorm:"not null;default:true" json:"active"`
	// Kind is "base" (a subscription is to one base plan) or "addon" (extra
	// items attached to a base subscription, e.g. extra storage).
	Kind     string            `gorm:"size:8;not null;default:base" json:"kind"`
	Metadata datatypes.JSONMap `gorm:"type:jsonb" json:"metadata"`
	Prices   []Price           `gorm:"foreignKey:PlanID" json:"prices,omitempty"`
}

// Plan kinds.
const (
	PlanBase  = "base"
	PlanAddon = "addon"
)

// Price is an immutable version of what a plan costs. Changing a price means
// creating a new row; subscriptions and invoices keep pointing at the old one.
type Price struct {
	Base
	TenantID      uuid.UUID       `gorm:"type:uuid;not null;index" json:"tenant_id"`
	PlanID        uuid.UUID       `gorm:"type:uuid;not null;index" json:"plan_id"`
	Nickname      string          `gorm:"size:120" json:"nickname"`
	Amount        int64           `gorm:"not null" json:"amount"`
	Currency      string          `gorm:"size:3;not null" json:"currency"`
	Interval      domain.Interval `gorm:"size:16;not null" json:"interval"`
	IntervalCount int             `gorm:"not null;default:1" json:"interval_count"`
	TrialDays     int             `gorm:"not null;default:0" json:"trial_days"`
	Active        bool            `gorm:"not null;default:true" json:"active"`
}

// Subscription ties a customer to a recurring price.
//
// Periods are counted from BillingAnchor: period n (1-based) ends at
// NthPeriodEnd(anchor, n). Cycle is the number of the current period, 0 while
// in the initial trial. Counting from an anchor instead of chaining keeps a
// customer who started on the 31st on the 31st.
type Subscription struct {
	Base
	TenantID       uuid.UUID  `gorm:"type:uuid;not null;index" json:"tenant_id"`
	CustomerID     uuid.UUID  `gorm:"type:uuid;not null;index" json:"customer_id"`
	PlanID         uuid.UUID  `gorm:"type:uuid;not null;index" json:"plan_id"`
	PriceID        uuid.UUID  `gorm:"type:uuid;not null" json:"price_id"`
	PendingPriceID *uuid.UUID `gorm:"type:uuid" json:"pending_price_id"`
	// Quantity is the number of seats of the base price.
	Quantity        int  `gorm:"not null;default:1" json:"quantity"`
	PendingQuantity *int `json:"pending_quantity"`
	// PendingFrom is when PendingPriceID / PendingQuantity take effect: the
	// start of the first period not yet paid for.
	PendingFrom *time.Time `json:"pending_from"`
	// DiscountID is the coupon currently applied to the subscription.
	DiscountID         *uuid.UUID                `gorm:"type:uuid" json:"discount_id"`
	Status             domain.SubscriptionStatus `gorm:"size:16;not null;index" json:"status"`
	BillingAnchor      time.Time                 `gorm:"not null" json:"billing_anchor"`
	Cycle              int                       `gorm:"not null;default:0" json:"cycle"`
	CurrentPeriodStart time.Time                 `gorm:"not null" json:"current_period_start"`
	CurrentPeriodEnd   time.Time                 `gorm:"not null;index" json:"current_period_end"`
	TrialEnd           *time.Time                `json:"trial_end"`
	CancelAtPeriodEnd  bool                      `gorm:"not null;default:false" json:"cancel_at_period_end"`
	CanceledAt         *time.Time                `json:"canceled_at"`
	EndedAt            *time.Time                `json:"ended_at"`
	Metadata           datatypes.JSONMap         `gorm:"type:jsonb" json:"metadata"`
}

// Invoice is a bill. Drafts have no number and post nothing to the ledger;
// finalising assigns a number and posts the receivable.
type Invoice struct {
	Base
	TenantID       uuid.UUID            `gorm:"type:uuid;not null;index;uniqueIndex:ux_invoice_number,priority:1;uniqueIndex:ux_invoice_reference,priority:1" json:"tenant_id"`
	CustomerID     uuid.UUID            `gorm:"type:uuid;not null;index" json:"customer_id"`
	SubscriptionID *uuid.UUID           `gorm:"type:uuid;index" json:"subscription_id"`
	Number         *string              `gorm:"size:40;uniqueIndex:ux_invoice_number,priority:2" json:"number"`
	Reference      *string              `gorm:"size:16;uniqueIndex:ux_invoice_reference,priority:2" json:"reference"`
	Kind           domain.InvoiceKind   `gorm:"size:24;not null" json:"kind"`
	Status         domain.InvoiceStatus `gorm:"size:16;not null;index" json:"status"`
	Currency       string               `gorm:"size:3;not null" json:"currency"`
	// Subtotal is the sum of lines; Total = Subtotal - DiscountTotal +
	// TaxTotal (exclusive tax only; inclusive tax is inside the subtotal).
	Subtotal            int64             `gorm:"not null;default:0" json:"subtotal"`
	DiscountTotal       int64             `gorm:"not null;default:0" json:"discount_total"`
	DiscountID          *uuid.UUID        `gorm:"type:uuid;index" json:"discount_id"`
	DiscountDescription string            `gorm:"size:200" json:"discount_description,omitempty"`
	TaxTotal            int64             `gorm:"not null;default:0" json:"tax_total"`
	TaxRateBps          int               `gorm:"not null;default:0" json:"tax_rate_bps"`
	TaxName             string            `gorm:"size:40" json:"tax_name,omitempty"`
	TaxInclusive        bool              `gorm:"not null;default:false" json:"tax_inclusive"`
	Total               int64             `gorm:"not null;default:0" json:"total"`
	AmountPaid          int64             `gorm:"not null;default:0" json:"amount_paid"`
	CreditApplied       int64             `gorm:"not null;default:0" json:"credit_applied"`
	AmountDue           int64             `gorm:"not null;default:0" json:"amount_due"`
	PeriodStart         *time.Time        `json:"period_start"`
	PeriodEnd           *time.Time        `json:"period_end"`
	DueAt               *time.Time        `json:"due_at"`
	FinalizedAt         *time.Time        `json:"finalized_at"`
	PaidAt              *time.Time        `json:"paid_at"`
	VoidedAt            *time.Time        `json:"voided_at"`
	Memo                string            `gorm:"size:2000" json:"memo"`
	Metadata            datatypes.JSONMap `gorm:"type:jsonb" json:"metadata"`
	Lines               []InvoiceLine     `gorm:"foreignKey:InvoiceID" json:"lines,omitempty"`
}

// InvoiceLine is one charge on an invoice. Amount = Quantity * UnitAmount and
// is never negative; discounts are not modelled yet.
type InvoiceLine struct {
	Base
	InvoiceID uuid.UUID  `gorm:"type:uuid;not null;index" json:"invoice_id"`
	PriceID   *uuid.UUID `gorm:"type:uuid" json:"price_id"`
	// PlanID lets plan-restricted coupons find the lines they discount.
	PlanID             *uuid.UUID `gorm:"type:uuid" json:"plan_id"`
	SubscriptionItemID *uuid.UUID `gorm:"type:uuid" json:"subscription_item_id"`
	Description        string     `gorm:"size:500;not null" json:"description"`
	Quantity           int64      `gorm:"not null" json:"quantity"`
	UnitAmount         int64      `gorm:"not null" json:"unit_amount"`
	Amount             int64      `gorm:"not null" json:"amount"`
	PeriodStart        *time.Time `json:"period_start"`
	PeriodEnd          *time.Time `json:"period_end"`
}

// Payment is one attempt to pay an invoice through a provider.
type Payment struct {
	Base
	TenantID   uuid.UUID `gorm:"type:uuid;not null;index" json:"tenant_id"`
	InvoiceID  uuid.UUID `gorm:"type:uuid;not null;index" json:"invoice_id"`
	CustomerID uuid.UUID `gorm:"type:uuid;not null;index" json:"customer_id"`
	Provider   string    `gorm:"size:32;not null" json:"provider"`
	// ProviderAccountID is the tenant's account the payment went through
	// (nil for providers that need none, e.g. mock and manual).
	ProviderAccountID *uuid.UUID           `gorm:"type:uuid" json:"provider_account_id"`
	ProviderRef       string               `gorm:"size:200;not null" json:"provider_ref"`
	Status            domain.PaymentStatus `gorm:"size:16;not null;index" json:"status"`
	Amount            int64                `gorm:"not null" json:"amount"`
	Currency          string               `gorm:"size:3;not null" json:"currency"`
	Instructions      string               `gorm:"size:2000" json:"instructions,omitempty"`
	PayURL            string               `gorm:"size:1000" json:"pay_url,omitempty"`
	ReturnURL         string               `gorm:"size:1000" json:"return_url,omitempty"`
	ExpiresAt         *time.Time           `json:"expires_at"`
	SettledAt         *time.Time           `json:"settled_at"`
	RefundedAt        *time.Time           `json:"refunded_at"`
	FailureReason     string               `gorm:"size:500" json:"failure_reason,omitempty"`
	Note              string               `gorm:"size:1000" json:"note,omitempty"`
	CreatedByKeyID    *uuid.UUID           `gorm:"type:uuid" json:"created_by_key_id"`
	Raw               datatypes.JSONMap    `gorm:"type:jsonb" json:"-"`

	// CancelReason says why a canceled payment was canceled.
	CancelReason string `gorm:"size:200" json:"cancel_reason,omitempty"`
	// ProviderCancel tracks closing the intent at the gateway, for providers
	// that support it (providers.Canceler). It is filled only when the
	// payment is canceled:
	//
	//	""             nothing to do (never canceled, or provider cannot cancel)
	//	pending        queued; the worker will call the provider
	//	done           the gateway confirmed the cancel
	//	already_paid   the customer paid first; the money was recorded
	//	failed         gave up after retries (see ProviderCancelError)
	ProviderCancel         string     `gorm:"size:16;not null;default:''" json:"provider_cancel,omitempty"`
	ProviderCancelAttempts int        `gorm:"not null;default:0" json:"provider_cancel_attempts,omitempty"`
	ProviderCancelNextAt   *time.Time `json:"-"`
	ProviderCancelError    string     `gorm:"size:500" json:"provider_cancel_error,omitempty"`
	ProviderCanceledAt     *time.Time `json:"provider_canceled_at,omitempty"`
}

// Provider cancel states.
const (
	ProviderCancelPending     = "pending"
	ProviderCancelDone        = "done"
	ProviderCancelAlreadyPaid = "already_paid"
	ProviderCancelFailed      = "failed"
)

// SubscriptionItem is an add-on on a subscription: an add-on plan's price ×
// quantity, billed with the base plan on the same interval.
type SubscriptionItem struct {
	Base
	TenantID       uuid.UUID `gorm:"type:uuid;not null;index" json:"tenant_id"`
	SubscriptionID uuid.UUID `gorm:"type:uuid;not null;index" json:"subscription_id"`
	PlanID         uuid.UUID `gorm:"type:uuid;not null" json:"plan_id"`
	PriceID        uuid.UUID `gorm:"type:uuid;not null" json:"price_id"`
	Quantity       int       `gorm:"not null" json:"quantity"`
	// PendingQuantity takes effect at PendingFrom; 0 means removal.
	PendingQuantity *int       `json:"pending_quantity"`
	PendingFrom     *time.Time `json:"pending_from"`
	RemovedAt       *time.Time `json:"removed_at"`
}

// ProviderAccount is a tenant's connection to a payment provider: its
// merchant credentials, encrypted with SECRETS_KEY.
type ProviderAccount struct {
	Base
	TenantID uuid.UUID `gorm:"type:uuid;not null;uniqueIndex:ux_provider_account,priority:1" json:"tenant_id"`
	Provider string    `gorm:"size:32;not null;uniqueIndex:ux_provider_account,priority:2" json:"provider"`
	Mode     string    `gorm:"size:8;not null;default:test" json:"mode"`
	Enabled  bool      `gorm:"not null;default:true" json:"enabled"`
	// Config is the AES-GCM sealed JSON of every field value.
	Config []byte `gorm:"type:bytea;not null" json:"-"`
	// Hints shows non-secret values in full and secrets as "••••1234".
	Hints datatypes.JSONMap `gorm:"type:jsonb" json:"hints"`
}

// Coupon durations.
const (
	CouponOnce      = "once"
	CouponRepeating = "repeating"
	CouponForever   = "forever"
)

// Coupon is a discount customers redeem with a promo code.
type Coupon struct {
	Base
	TenantID uuid.UUID `gorm:"type:uuid;not null;uniqueIndex:ux_coupon_code,priority:1" json:"tenant_id"`
	// Code is what the customer types (stored uppercase).
	Code string `gorm:"size:64;not null;uniqueIndex:ux_coupon_code,priority:2" json:"code"`
	Name string `gorm:"size:200;not null" json:"name"`
	// Exactly one of PercentOff (1–100) or AmountOff (+Currency) is set.
	PercentOff *int   `json:"percent_off"`
	AmountOff  *int64 `json:"amount_off"`
	Currency   string `gorm:"size:3" json:"currency,omitempty"`
	// Duration on subscriptions: once (first invoice), repeating
	// (DurationPeriods invoices) or forever.
	Duration        string `gorm:"size:16;not null" json:"duration"`
	DurationPeriods *int   `json:"duration_periods"`
	// PlanIDs restricts the discount to lines of these plans; empty = all.
	PlanIDs        datatypes.JSONSlice[string] `gorm:"type:jsonb;not null" json:"plan_ids"`
	MaxRedemptions *int                        `json:"max_redemptions"`
	TimesRedeemed  int                         `gorm:"not null;default:0" json:"times_redeemed"`
	RedeemBy       *time.Time                  `json:"redeem_by"`
	Active         bool                        `gorm:"not null;default:true" json:"active"`
}

// AppliesTo reports whether the coupon discounts a line of this plan. Lines
// with no plan (custom one-off charges) only get unrestricted coupons.
func (c *Coupon) AppliesTo(planID *uuid.UUID) bool {
	if len(c.PlanIDs) == 0 {
		return true
	}
	if planID == nil {
		return false
	}
	for _, id := range c.PlanIDs {
		if id == planID.String() {
			return true
		}
	}
	return false
}

// Discount is one redemption of a coupon: on a subscription (lasting per the
// coupon's duration) or on a single one-off invoice.
type Discount struct {
	Base
	TenantID       uuid.UUID  `gorm:"type:uuid;not null;index" json:"tenant_id"`
	CouponID       uuid.UUID  `gorm:"type:uuid;not null;index" json:"coupon_id"`
	CustomerID     uuid.UUID  `gorm:"type:uuid;not null;index" json:"customer_id"`
	SubscriptionID *uuid.UUID `gorm:"type:uuid;index" json:"subscription_id"`
	InvoiceID      *uuid.UUID `gorm:"type:uuid;index" json:"invoice_id"`
	EndedAt        *time.Time `json:"ended_at"`
}

// LedgerAccountType decides which side increases the balance.
type LedgerAccountType string

const (
	AccountAsset     LedgerAccountType = "asset"
	AccountLiability LedgerAccountType = "liability"
	AccountRevenue   LedgerAccountType = "revenue"
	AccountExpense   LedgerAccountType = "expense"
)

// DebitNormal reports whether debits increase this account's balance.
func (t LedgerAccountType) DebitNormal() bool {
	return t == AccountAsset || t == AccountExpense
}

// LedgerAccount is one single-currency account. Balance is stored in the
// account's normal orientation (debit-normal accounts: debits minus credits).
type LedgerAccount struct {
	Base
	TenantID   uuid.UUID         `gorm:"type:uuid;not null;uniqueIndex:ux_ledger_account,priority:1" json:"tenant_id"`
	Code       string            `gorm:"size:120;not null;uniqueIndex:ux_ledger_account,priority:2" json:"code"`
	Currency   string            `gorm:"size:3;not null;uniqueIndex:ux_ledger_account,priority:3" json:"currency"`
	Type       LedgerAccountType `gorm:"size:16;not null" json:"type"`
	Name       string            `gorm:"size:200;not null" json:"name"`
	CustomerID *uuid.UUID        `gorm:"type:uuid;index" json:"customer_id"`
	Balance    int64             `gorm:"not null;default:0" json:"balance"`
}

// LedgerTransaction is the header of one balanced posting.
type LedgerTransaction struct {
	Base
	TenantID       uuid.UUID         `gorm:"type:uuid;not null;index;uniqueIndex:ux_ledger_idem,priority:1" json:"tenant_id"`
	Kind           string            `gorm:"size:40;not null" json:"kind"`
	IdempotencyKey string            `gorm:"size:200;not null;uniqueIndex:ux_ledger_idem,priority:2" json:"idempotency_key"`
	Currency       string            `gorm:"size:3;not null" json:"currency"`
	InvoiceID      *uuid.UUID        `gorm:"type:uuid;index" json:"invoice_id"`
	PaymentID      *uuid.UUID        `gorm:"type:uuid;index" json:"payment_id"`
	CustomerID     *uuid.UUID        `gorm:"type:uuid;index" json:"customer_id"`
	Description    string            `gorm:"size:500" json:"description"`
	EffectiveAt    time.Time         `gorm:"not null" json:"effective_at"`
	Metadata       datatypes.JSONMap `gorm:"type:jsonb" json:"metadata"`
	Entries        []LedgerEntry     `gorm:"foreignKey:TransactionID" json:"entries,omitempty"`
}

// LedgerEntry is one leg. Exactly one of Debit/Credit is non-zero (a CHECK
// constraint enforces it). Entries are append-only.
type LedgerEntry struct {
	ID            int64     `gorm:"primaryKey;autoIncrement" json:"id"`
	TransactionID uuid.UUID `gorm:"type:uuid;not null;index" json:"transaction_id"`
	TenantID      uuid.UUID `gorm:"type:uuid;not null;index" json:"tenant_id"`
	AccountID     uuid.UUID `gorm:"type:uuid;not null;index" json:"account_id"`
	Debit         int64     `gorm:"not null;default:0" json:"debit"`
	Credit        int64     `gorm:"not null;default:0" json:"credit"`
	CreatedAt     time.Time `gorm:"not null" json:"created_at"`
}

// IdempotencyRecord remembers the response to a POST sent with an
// Idempotency-Key, so a retried request gets the same answer.
type IdempotencyRecord struct {
	TenantID    uuid.UUID `gorm:"type:uuid;primaryKey"`
	Key         string    `gorm:"size:200;primaryKey"`
	Method      string    `gorm:"size:8;not null"`
	Path        string    `gorm:"size:500;not null"`
	RequestHash string    `gorm:"size:64;not null"`
	Status      int       `gorm:"not null;default:0"` // 0 while in flight
	Response    []byte    `gorm:"type:bytea"`
	CreatedAt   time.Time `gorm:"not null;index"`
}

// Event is an outbox row: something happened that integrators may care about.
type Event struct {
	Base
	TenantID uuid.UUID      `gorm:"type:uuid;not null;index" json:"tenant_id"`
	Type     string         `gorm:"size:64;not null;index" json:"type"`
	Data     datatypes.JSON `gorm:"type:jsonb;not null" json:"data"`
}

// WebhookEndpoint receives events for one tenant.
type WebhookEndpoint struct {
	Base
	TenantID    uuid.UUID                   `gorm:"type:uuid;not null;index" json:"tenant_id"`
	URL         string                      `gorm:"size:1000;not null" json:"url"`
	Description string                      `gorm:"size:200" json:"description"`
	Secret      string                      `gorm:"size:100;not null" json:"-"`
	EventTypes  datatypes.JSONSlice[string] `gorm:"type:jsonb;not null" json:"event_types"`
	Enabled     bool                        `gorm:"not null;default:true" json:"enabled"`
}

// Wants reports whether the endpoint subscribes to an event type. An empty
// list or "*" means everything.
func (e *WebhookEndpoint) Wants(eventType string) bool {
	if len(e.EventTypes) == 0 {
		return true
	}
	for _, t := range e.EventTypes {
		if t == "*" || t == eventType {
			return true
		}
	}
	return false
}

// WebhookDelivery is one event to one endpoint, retried with backoff.
type WebhookDelivery struct {
	Base
	TenantID       uuid.UUID  `gorm:"type:uuid;not null;index" json:"tenant_id"`
	EventID        uuid.UUID  `gorm:"type:uuid;not null;index" json:"event_id"`
	EndpointID     uuid.UUID  `gorm:"type:uuid;not null;index" json:"endpoint_id"`
	Status         string     `gorm:"size:16;not null" json:"status"` // pending|succeeded|failed
	Attempts       int        `gorm:"not null;default:0" json:"attempts"`
	NextAttemptAt  time.Time  `gorm:"not null" json:"next_attempt_at"`
	LastStatusCode int        `json:"last_status_code"`
	LastError      string     `gorm:"size:1000" json:"last_error"`
	DeliveredAt    *time.Time `json:"delivered_at"`
}

// All lists every model for migration and test truncation.
func All() []any {
	return []any{
		&Tenant{}, &APIKey{}, &Customer{}, &Plan{}, &Price{}, &Subscription{}, &SubscriptionItem{},
		&Coupon{}, &Discount{}, &ProviderAccount{},
		&Invoice{}, &InvoiceLine{}, &Payment{},
		&LedgerAccount{}, &LedgerTransaction{}, &LedgerEntry{},
		&IdempotencyRecord{}, &Event{}, &WebhookEndpoint{}, &WebhookDelivery{},
	}
}
