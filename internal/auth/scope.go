package auth

import (
	"fmt"
	"sort"
	"strings"
)

// Scope is a permission carried by an API key.
type Scope string

const (
	// ScopeAdmin grants everything inside the key's tenant, including managing
	// its API keys and webhook endpoints. On a platform key it also covers
	// creating and managing tenants.
	ScopeAdmin Scope = "admin"

	ScopeCustomersRead      Scope = "customers:read"
	ScopeCustomersWrite     Scope = "customers:write"
	ScopePlansRead          Scope = "plans:read"
	ScopePlansWrite         Scope = "plans:write"
	ScopeCouponsRead        Scope = "coupons:read"
	ScopeCouponsWrite       Scope = "coupons:write"
	ScopeSubscriptionsRead  Scope = "subscriptions:read"
	ScopeSubscriptionsWrite Scope = "subscriptions:write"
	ScopeInvoicesRead       Scope = "invoices:read"
	ScopeInvoicesWrite      Scope = "invoices:write"
	ScopePaymentsRead       Scope = "payments:read"
	ScopePaymentsWrite      Scope = "payments:write"
	// ScopePaymentsManual records money that arrived outside any provider
	// (cash, bank transfer). It is separate from payments:write because it
	// asserts that money was received, which a checkout integration never
	// needs to do.
	ScopePaymentsManual Scope = "payments:manual"
	ScopeLedgerRead     Scope = "ledger:read"
	ScopeWebhooksRead   Scope = "webhooks:read"
	ScopeWebhooksWrite  Scope = "webhooks:write"
	ScopeKeysRead       Scope = "keys:read"
	ScopeKeysWrite      Scope = "keys:write"
)

// All lists every grantable scope in documentation order.
func All() []Scope {
	return []Scope{
		ScopeAdmin,
		ScopeCustomersRead, ScopeCustomersWrite,
		ScopePlansRead, ScopePlansWrite,
		ScopeCouponsRead, ScopeCouponsWrite,
		ScopeSubscriptionsRead, ScopeSubscriptionsWrite,
		ScopeInvoicesRead, ScopeInvoicesWrite,
		ScopePaymentsRead, ScopePaymentsWrite, ScopePaymentsManual,
		ScopeLedgerRead,
		ScopeWebhooksRead, ScopeWebhooksWrite,
		ScopeKeysRead, ScopeKeysWrite,
	}
}

// Describe explains each scope, for GET /scopes and the UI.
func Describe() map[Scope]string {
	return map[Scope]string{
		ScopeAdmin:              "Everything in the tenant; on a platform key also tenants",
		ScopeCustomersRead:      "List and read customers and their balances",
		ScopeCustomersWrite:     "Create and update customers, grant credit",
		ScopePlansRead:          "List and read plans and prices",
		ScopePlansWrite:         "Create and update plans, add prices",
		ScopeCouponsRead:        "List coupons and check promo codes",
		ScopeCouponsWrite:       "Create, update and deactivate coupons",
		ScopeSubscriptionsRead:  "List and read subscriptions",
		ScopeSubscriptionsWrite: "Create, cancel, resume and change subscriptions",
		ScopeInvoicesRead:       "List and read invoices",
		ScopeInvoicesWrite:      "Create, finalize and void invoices",
		ScopePaymentsRead:       "List and read payments",
		ScopePaymentsWrite:      "Start payments, simulate mock outcomes, refund",
		ScopePaymentsManual:     "Record money received outside a provider (cash, bank transfer)",
		ScopeLedgerRead:         "Read ledger accounts, transactions and reconciliation",
		ScopeWebhooksRead:       "List webhook endpoints, events and deliveries",
		ScopeWebhooksWrite:      "Create, update and delete webhook endpoints",
		ScopeKeysRead:           "List API keys",
		ScopeKeysWrite:          "Create and revoke API keys (never beyond the creator's own scopes)",
	}
}

// Parse validates one scope.
func Parse(raw string) (Scope, error) {
	s := Scope(strings.TrimSpace(raw))
	for _, known := range All() {
		if s == known {
			return s, nil
		}
	}
	return "", fmt.Errorf("unknown scope %q", raw)
}

// ParseAll validates a list. Unknown values are an error rather than ignored:
// a silently dropped typo would hand out a key that quietly does nothing.
func ParseAll(raw []string) ([]Scope, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("at least one scope is required")
	}
	seen := map[Scope]bool{}
	out := make([]Scope, 0, len(raw))
	for _, r := range raw {
		s, err := Parse(r)
		if err != nil {
			return nil, err
		}
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// Strings converts scopes for storage.
func Strings(scopes []Scope) []string {
	out := make([]string, len(scopes))
	for i, s := range scopes {
		out[i] = string(s)
	}
	return out
}

// FromStrings converts stored values back, dropping anything no longer known
// so a removed scope cannot keep granting access.
func FromStrings(raw []string) []Scope {
	out := make([]Scope, 0, len(raw))
	for _, r := range raw {
		if s, err := Parse(r); err == nil {
			out = append(out, s)
		}
	}
	return out
}

// Grants reports whether holding `held` satisfies `want`:
//
//	admin          grants everything
//	<res>:write    grants <res>:read
//	payments:write does NOT grant payments:manual
func Grants(held, want Scope) bool {
	if held == ScopeAdmin || held == want {
		return true
	}
	res, action, ok := strings.Cut(string(want), ":")
	if !ok || action != "read" {
		return false
	}
	return held == Scope(res+":write")
}

// Satisfies reports whether a set of held scopes covers the requirement.
func Satisfies(held []Scope, want Scope) bool {
	for _, h := range held {
		if Grants(h, want) {
			return true
		}
	}
	return false
}
