package domain

import (
	"crypto/rand"
	"math/big"
)

// Subscription statuses.
//
//	incomplete  created, first invoice not paid yet; no access
//	trialing    in a free trial period
//	active      paid for the current period
//	past_due    the period ended and the renewal invoice is unpaid; still inside grace
//	canceled    ended on request (immediately or at period end)
//	expired     never paid (incomplete timed out) or lapsed after grace
type SubscriptionStatus string

const (
	SubIncomplete SubscriptionStatus = "incomplete"
	SubTrialing   SubscriptionStatus = "trialing"
	SubActive     SubscriptionStatus = "active"
	SubPastDue    SubscriptionStatus = "past_due"
	SubCanceled   SubscriptionStatus = "canceled"
	SubExpired    SubscriptionStatus = "expired"
)

// LiveSubscriptionStatuses are the statuses that still occupy a customer's
// slot for a plan. A partial unique index enforces one live subscription per
// (customer, plan) over exactly this list.
var LiveSubscriptionStatuses = []SubscriptionStatus{SubIncomplete, SubTrialing, SubActive, SubPastDue}

// Live reports whether the subscription has not ended.
func (s SubscriptionStatus) Live() bool {
	for _, l := range LiveSubscriptionStatuses {
		if s == l {
			return true
		}
	}
	return false
}

// Entitled reports whether the customer should have access right now. past_due
// keeps access: it is the grace window.
func (s SubscriptionStatus) Entitled() bool {
	return s == SubTrialing || s == SubActive || s == SubPastDue
}

// Invoice statuses.
type InvoiceStatus string

const (
	InvoiceDraft         InvoiceStatus = "draft"
	InvoiceOpen          InvoiceStatus = "open"
	InvoicePaid          InvoiceStatus = "paid"
	InvoiceVoid          InvoiceStatus = "void"
	InvoiceUncollectible InvoiceStatus = "uncollectible"
)

// Invoice kinds say why an invoice exists.
type InvoiceKind string

const (
	InvoiceSubscriptionCreate InvoiceKind = "subscription_create"
	InvoiceSubscriptionCycle  InvoiceKind = "subscription_cycle"
	InvoiceProration          InvoiceKind = "proration"
	InvoiceOneOff             InvoiceKind = "one_off"
)

// Payment statuses.
type PaymentStatus string

const (
	PaymentPending   PaymentStatus = "pending"
	PaymentSucceeded PaymentStatus = "succeeded"
	PaymentFailed    PaymentStatus = "failed"
	PaymentCanceled  PaymentStatus = "canceled"
	PaymentRefunded  PaymentStatus = "refunded"
)

// referenceAlphabet has no 0/O or 1/I, so a reference read aloud or typed from
// a phone into a bank transfer description survives.
const referenceAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// NewReference returns a short random code an invoice can be quoted by, e.g.
// in a bank transfer description for manual payments.
func NewReference(n int) string {
	b := make([]byte, n)
	max := big.NewInt(int64(len(referenceAlphabet)))
	for i := range b {
		v, err := rand.Int(rand.Reader, max)
		if err != nil {
			panic("domain: entropy unavailable: " + err.Error())
		}
		b[i] = referenceAlphabet[v.Int64()]
	}
	return string(b)
}
