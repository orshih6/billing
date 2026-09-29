# Writing a payment provider

A provider turns "collect this invoice" into whatever a gateway needs: a hosted
checkout URL, a QR code, bank details. Later it answers "was it paid?". The
billing core does everything else: invoices, ledger postings, settlement,
retries, and cancelling intents that are no longer needed.

`mock` (`internal/providers/mock.go`) and `manual` (`manual.go`) are complete,
small examples.

## The interface

```go
type Provider interface {
    Name() string                 // "qpay", "stripe" — also the URL segment
    Automatic() bool              // true if the gateway confirms payments itself
    Fields() []Field              // credentials a tenant must enter; nil = none
    CreateIntent(ctx, acct *Account, in IntentInput) (*Intent, error)
    Check(ctx, acct *Account, providerRef string) (*CheckResult, error)
    ParseWebhook(r *http.Request, acct *Account) (providerRef string, err error)
}

// Optional: implement it if the gateway can close an open intent.
type Canceler interface {
    CancelIntent(ctx, acct *Account, providerRef string) error // idempotent
}
```

- **`Fields`**: each tenant connects its own merchant account. Declare what you
  need (`{Key: "client_secret", Label: "Client secret", Secret: true, Required:
  true}`). The API (`PUT /payment-providers/{name}`) and the UI's
  *Settings → Payment methods* build their forms from this list. Values are
  stored encrypted, and `Secret` fields are never shown again.
- **`Account`**: holds the tenant's decrypted values (`acct.Get("client_secret")`)
  and `acct.Mode` (`"test"` or `"live"`; use it to choose sandbox or production
  endpoints). It is `nil` for providers without fields.
- **`CreateIntent`** returns the gateway's id for the intent in `ProviderRef`
  (unique per tenant and provider), plus `PayURL` and/or `Instructions` for the
  payer. Use `in.PaymentID` as your idempotency key or merchant order id if the
  gateway takes one.
- **`Check`** is the source of truth. Return `CheckPaid` with the amount actually
  received, in minor units; the core records over- and under-payments correctly.
- **`ParseWebhook`** receives callbacks at
  `POST /webhooks/{provider}/{accountID}`. Verify the signature with the account's
  secret, then return the reference. Do not decide the outcome here; the core
  calls `Check` next.
- **`CancelIntent`** is called by the worker after an invoice is paid some other
  way, voided, or the payment expires. It is never called inside a database
  transaction, and it is retried with backoff. If the customer already paid,
  return `providers.ErrAlreadyPaid`; the core then records the money (as credit)
  instead of losing it.

## Skeleton

```go
package qpay

type Provider struct{ HTTP *http.Client }

func (Provider) Name() string    { return "qpay" }
func (Provider) Automatic() bool { return true }
func (Provider) Fields() []providers.Field {
    return []providers.Field{
        {Key: "username", Label: "Username", Required: true},
        {Key: "password", Label: "Password", Secret: true, Required: true},
        {Key: "invoice_code", Label: "Invoice code", Required: true},
    }
}

func (p Provider) base(a *providers.Account) string {
    if a.Mode == "live" { return "https://merchant.qpay.mn/v2" }
    return "https://merchant-sandbox.qpay.mn/v2"
}

func (p Provider) CreateIntent(ctx context.Context, a *providers.Account, in providers.IntentInput) (*providers.Intent, error) {
    // 1. get a token with a.Get("username"), a.Get("password")
    // 2. POST /invoice with sender_invoice_no = in.PaymentID, amount = in.Amount (convert minor units)
    // 3. return &providers.Intent{ProviderRef: qpayInvoiceID, PayURL: shortURL, ExpiresAt: in.ExpiresAt}
}

func (p Provider) Check(ctx context.Context, a *providers.Account, ref string) (*providers.CheckResult, error) {
    // POST /payment/check {object_id: ref}; paid → CheckPaid with the paid amount
}

func (p Provider) ParseWebhook(r *http.Request, a *providers.Account) (string, error) {
    // QPay calls your callback URL with ?ref=…; verify what can be verified, return the ref
}

func (p Provider) CancelIntent(ctx context.Context, a *providers.Account, ref string) error {
    // DELETE /invoice/{ref}; treat "already cancelled" as success,
    // "already paid" as providers.ErrAlreadyPaid
}
```

Register it in `cmd/billing/main.go` (`newService`):

```go
reg := providers.NewRegistry(providers.Mock{…}, providers.Manual{}, qpay.Provider{HTTP: …})
```

## Testing

- Unit-test the HTTP mapping with `httptest.Server` standing in for the gateway.
- For the billing flow, register your provider in a `newHarness` test (see
  `internal/billing/provider_accounts_test.go` and `provider_cancel_test.go`). Then
  assert the payment settles, the invoice is paid, and `h.reconciled()` holds.
- Convert amounts carefully. The core uses minor units (`domain.Currency.Exponent`);
  many gateways use whole units or decimals.
