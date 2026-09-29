package providers

import (
	"context"
	"net/http"
)

// Mock is a fake checkout. CreateIntent returns a link to a page served by
// this service (/checkout/mock/{payment}) with "Pay" and "Fail" buttons; the
// same outcomes are reachable through POST /payments/{id}/mock/{outcome}.
// Nothing is ever charged.
//
// Its state lives on the payment row, so Check has nothing to ask and reports
// pending; outcomes are applied directly by the billing service.
type Mock struct {
	// PublicURL is this service's browser-facing base URL.
	PublicURL string
}

func (Mock) Name() string    { return "mock" }
func (Mock) Automatic() bool { return true }
func (Mock) Fields() []Field { return nil }

func (m Mock) CreateIntent(_ context.Context, _ *Account, in IntentInput) (*Intent, error) {
	return &Intent{
		ProviderRef:  "mock_" + in.PaymentID,
		PayURL:       m.PublicURL + "/checkout/mock/" + in.PaymentID,
		Instructions: "Test payment. Open the pay URL and choose an outcome; no money moves.",
		ExpiresAt:    in.ExpiresAt,
		Raw:          map[string]any{"mode": "mock"},
	}, nil
}

func (Mock) Check(context.Context, *Account, string) (*CheckResult, error) {
	return &CheckResult{Status: CheckPending}, nil
}

func (Mock) ParseWebhook(*http.Request, *Account) (string, error) { return "", ErrUnsupported }

// CancelIntent closes the fake checkout. The mock keeps its state on the
// payment row, so there is nothing remote to call; once the cancellation is
// recorded as done, the checkout page refuses to take the payment.
func (Mock) CancelIntent(context.Context, *Account, string) error { return nil }
