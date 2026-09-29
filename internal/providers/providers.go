// Package providers abstracts how money is collected. Each provider turns an
// invoice into instructions for the payer (a checkout URL, a QR code, bank
// details) and later tells us whether it was paid.
//
// Only two exist today:
//
//   - mock:   a fake checkout for development and integration testing. The
//     outcome is chosen by pressing a button (or calling the API).
//   - manual: money arrives outside the system (bank transfer, cash) and an
//     operator records it.
//
// Real gateways (QPay, Stripe, …) implement the same interface. Each tenant
// connects its own merchant account: the provider declares the credentials it
// needs (Fields), the tenant stores them (encrypted) as a provider account, and
// every call receives that tenant's Account.
//
// Webhooks from a provider are never trusted on their own: ParseWebhook only
// identifies which payment the callback is about (verifying the signature with
// the account's secret where the gateway signs callbacks), and Check asks the
// provider for the truth.
package providers

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"time"
)

// IntentInput is what a provider needs to start collecting.
type IntentInput struct {
	PaymentID     string
	InvoiceNumber string
	Reference     string
	Amount        int64
	Currency      string
	Description   string
	ReturnURL     string
	ExpiresAt     time.Time
	// Instructions is the tenant's template for manual payment text.
	Instructions string
}

// Intent is what the payer is shown.
type Intent struct {
	ProviderRef  string
	PayURL       string
	Instructions string
	ExpiresAt    time.Time
	Raw          map[string]any
}

// CheckStatus is the provider's view of a payment.
type CheckStatus string

const (
	CheckPending CheckStatus = "pending"
	CheckPaid    CheckStatus = "paid"
	CheckFailed  CheckStatus = "failed"
)

// CheckResult is the answer to "was this paid?".
type CheckResult struct {
	Status CheckStatus
	Amount int64
	Reason string
}

// Field is one credential or setting a provider needs from a tenant, e.g. a
// merchant id or an API secret. The API and UI render forms from these.
type Field struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	// Secret values are write-only: they are stored encrypted and never
	// returned, only a masked hint.
	Secret   bool   `json:"secret"`
	Required bool   `json:"required"`
	Help     string `json:"help,omitempty"`
}

// Account is one tenant's connection to a provider. It is nil for providers
// that need no credentials (Fields returns nothing).
type Account struct {
	ID       string
	TenantID string
	// Mode is "test" or "live"; gateways use it to pick sandbox endpoints.
	Mode   string
	Config map[string]string
}

// Get returns a config value.
func (a *Account) Get(key string) string {
	if a == nil {
		return ""
	}
	return a.Config[key]
}

// Provider is one way of collecting money.
type Provider interface {
	Name() string
	// Automatic reports whether the provider confirms payments itself. Manual
	// payments need an operator with payments:manual.
	Automatic() bool
	// Fields lists what a tenant must configure. Empty means the provider
	// works without an account.
	Fields() []Field
	CreateIntent(ctx context.Context, acct *Account, in IntentInput) (*Intent, error)
	Check(ctx context.Context, acct *Account, providerRef string) (*CheckResult, error)
	// ParseWebhook extracts the provider reference from an inbound callback
	// to this account's webhook URL, verifying its signature if the gateway
	// signs them.
	ParseWebhook(r *http.Request, acct *Account) (providerRef string, err error)
}

// ErrUnsupported is returned by operations a provider cannot perform.
var ErrUnsupported = errors.New("providers: operation not supported by this provider")

// Canceler is implemented by providers that can close an open intent at the
// gateway, so the customer can no longer pay it (void a QPay invoice, expire a
// checkout session). It is optional: a provider without it simply keeps the
// intent open until it expires on its own.
//
// The billing service never calls it inside a database transaction. When a
// payment is canceled (the invoice was paid another way, voided, the payment
// expired or was canceled on request) the cancellation is queued on the payment
// and the worker calls CancelIntent afterwards, retrying with backoff.
//
// CancelIntent must be idempotent: canceling an already-canceled intent is
// success. If the customer paid before the cancel arrived, return
// ErrAlreadyPaid; the service then asks Check for the amount and records the
// money (as customer credit if the invoice is already paid) instead of losing it.
type Canceler interface {
	CancelIntent(ctx context.Context, acct *Account, providerRef string) error
}

// ErrAlreadyPaid means the intent could not be canceled because it was paid.
var ErrAlreadyPaid = errors.New("providers: intent was already paid")

// CanCancel reports whether p supports canceling intents.
func CanCancel(p Provider) (Canceler, bool) {
	c, ok := p.(Canceler)
	return c, ok
}

// Registry holds the configured providers.
type Registry struct{ byName map[string]Provider }

// NewRegistry registers providers by name.
func NewRegistry(ps ...Provider) *Registry {
	r := &Registry{byName: map[string]Provider{}}
	for _, p := range ps {
		r.byName[p.Name()] = p
	}
	return r
}

// Get returns a provider by name.
func (r *Registry) Get(name string) (Provider, bool) {
	p, ok := r.byName[name]
	return p, ok
}

// Names lists registered providers.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.byName))
	for n := range r.byName {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
