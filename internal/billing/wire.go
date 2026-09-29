package billing

import (
	"time"

	"github.com/google/uuid"

	"github.com/orshih6/billing/internal/wire"
)

// Wire makes every page of results a public list.
func (l List[T]) Wire() any { return wire.ListOf(l.Data, l.HasMore) }

// AddOnWire is an add-on as the API shows it.
type AddOnWire struct {
	ID              uuid.UUID  `json:"id"`
	PlanID          uuid.UUID  `json:"plan_id"`
	PlanCode        string     `json:"plan_code"`
	PlanName        string     `json:"plan_name"`
	PriceID         uuid.UUID  `json:"price_id"`
	Quantity        int        `json:"quantity"`
	PendingQuantity *int       `json:"pending_quantity"`
	PendingFrom     *time.Time `json:"pending_from"`
	UnitAmount      int64      `json:"unit_amount"`
	Currency        string     `json:"currency"`
}

// SubscriptionWire is the API's subscription: the stored fields plus what an
// integrator needs next.
type SubscriptionWire struct {
	wire.Subscription
	PlanCode      string        `json:"plan_code"`
	PlanName      string        `json:"plan_name"`
	AddOns        []AddOnWire   `json:"add_ons"`
	Discount      *DiscountView `json:"discount"`
	LatestInvoice *wire.Invoice `json:"latest_invoice"`
}

func (v SubscriptionView) Wire() any {
	out := SubscriptionWire{Subscription: wire.SubscriptionOf(&v.Subscription), PlanCode: v.PlanCode, PlanName: v.PlanName,
		AddOns: []AddOnWire{}, Discount: v.Discount}
	for _, a := range v.AddOns {
		out.AddOns = append(out.AddOns, AddOnWire{a.ID, a.PlanID, a.PlanCode, a.PlanName, a.PriceID, a.Quantity,
			a.PendingQuantity, a.PendingFrom, a.UnitAmount, a.Currency})
	}
	if v.LatestInvoice != nil {
		inv := wire.InvoiceOf(v.LatestInvoice)
		out.LatestInvoice = &inv
	}
	return out
}

func (c *CreatedKey) Wire() any {
	return map[string]any{"key": wire.Of(c.Key), "token": c.Token}
}

func (c *CreatedTenant) Wire() any {
	out := map[string]any{"tenant": wire.Of(c.Tenant), "api_key": nil}
	if c.APIKey != nil {
		out["api_key"] = c.APIKey.Wire()
	}
	return out
}

func (c *CreatedEndpoint) Wire() any {
	return map[string]any{"endpoint": wire.Of(c.Endpoint), "secret": c.Secret}
}

// EntryWire is a ledger entry with its posting's description.
type EntryWire struct {
	ID          int64      `json:"id"`
	AccountID   uuid.UUID  `json:"account_id"`
	Debit       int64      `json:"debit"`
	Credit      int64      `json:"credit"`
	Kind        string     `json:"kind"`
	Description string     `json:"description"`
	InvoiceID   *uuid.UUID `json:"invoice_id"`
	PaymentID   *uuid.UUID `json:"payment_id"`
	CreatedAt   time.Time  `json:"created_at"`
}

func (e EntryView) Wire() any {
	return EntryWire{e.ID, e.AccountID, e.Debit, e.Credit, e.Kind, e.Description, e.InvoiceID, e.PaymentID, e.CreatedAt}
}
