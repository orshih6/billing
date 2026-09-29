package providers

import (
	"context"
	"net/http"
	"strings"

	"github.com/orshih6/billing/internal/domain"
)

// Manual covers money that arrives outside any gateway: a bank transfer, cash
// at a counter. Starting a manual payment returns the tenant's instructions
// with the invoice reference filled in; the payment stays pending until an
// operator holding payments:manual confirms how much actually arrived.
//
// It does not implement Canceler: there is nothing at a gateway to close. A
// canceled manual payment can still be confirmed if the transfer arrives late.
type Manual struct{}

func (Manual) Name() string    { return "manual" }
func (Manual) Automatic() bool { return false }
func (Manual) Fields() []Field { return nil }

func (Manual) CreateIntent(_ context.Context, _ *Account, in IntentInput) (*Intent, error) {
	text := in.Instructions
	if text == "" {
		text = "Transfer {amount} and write {reference} in the payment description."
	}
	text = strings.NewReplacer(
		"{reference}", in.Reference,
		"{amount}", domain.FormatAmount(in.Amount, in.Currency),
		"{invoice}", in.InvoiceNumber,
	).Replace(text)
	return &Intent{
		ProviderRef:  "manual_" + in.PaymentID,
		Instructions: text,
		ExpiresAt:    in.ExpiresAt,
	}, nil
}

func (Manual) Check(context.Context, *Account, string) (*CheckResult, error) {
	return &CheckResult{Status: CheckPending}, nil
}

func (Manual) ParseWebhook(*http.Request, *Account) (string, error) { return "", ErrUnsupported }
