package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/orshih6/billing/internal/billing"
	"github.com/orshih6/billing/internal/domain"
	"github.com/orshih6/billing/internal/ledger"
	"github.com/orshih6/billing/internal/models"
)

func currencyCodes() []string {
	out := []string{}
	for _, c := range domain.Currencies() {
		out = append(out, c.Code)
	}
	sortStrings(out)
	return out
}

func jsonCompact(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// opt returns a trimmed form value, or nil when it is empty.
func opt(r *http.Request, name string) *string {
	v := strings.TrimSpace(r.FormValue(name))
	if v == "" {
		return nil
	}
	return &v
}

// optRaw returns the value whenever the field was submitted, so a form can
// clear it.
func optRaw(r *http.Request, name string) *string {
	if err := r.ParseForm(); err != nil {
		return nil
	}
	if _, ok := r.PostForm[name]; !ok {
		return nil
	}
	v := strings.TrimSpace(r.PostForm.Get(name))
	return &v
}

func pageOf(r *http.Request) billing.Page {
	p := billing.Page{Limit: 50}
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		p.Limit = v
	}
	if id, err := uuid.Parse(r.URL.Query().Get("after")); err == nil {
		p.StartingAfter = &id
	}
	return p
}

type identified interface {
	models.Customer | models.Plan | models.Invoice | models.Payment | models.Event
}

func idOf(v any) uuid.UUID {
	switch x := v.(type) {
	case models.Customer:
		return x.ID
	case models.Plan:
		return x.ID
	case models.Invoice:
		return x.ID
	case models.Payment:
		return x.ID
	case models.Event:
		return x.ID
	}
	return uuid.Nil
}

// nextCursor returns the id to continue after, or "".
func nextCursor[T identified](l billing.List[T]) string {
	if !l.HasMore || len(l.Data) == 0 {
		return ""
	}
	return idOf(l.Data[len(l.Data)-1]).String()
}

func nextSubCursor(l billing.List[billing.SubscriptionView]) string {
	if !l.HasMore || len(l.Data) == 0 {
		return ""
	}
	return l.Data[len(l.Data)-1].ID.String()
}

func subCustomerIDs(subs []billing.SubscriptionView) []uuid.UUID {
	out := make([]uuid.UUID, len(subs))
	for i, s := range subs {
		out[i] = s.CustomerID
	}
	return out
}

// customerNames maps ids to names for list pages.
func (u *UI) customerNames(r *http.Request, tenantID uuid.UUID, ids []uuid.UUID) map[string]string {
	out := map[string]string{}
	if len(ids) == 0 {
		return out
	}
	var cs []models.Customer
	u.svc.DB().WithContext(r.Context()).Select("id, name").
		Where("tenant_id = ? AND id IN ?", tenantID, ids).Find(&cs)
	for _, c := range cs {
		out[c.ID.String()] = c.Name
	}
	return out
}

// accountNames labels ledger accounts in posting tables: system accounts by
// code, customer accounts as "receivable"/"credit".
func (u *UI) accountNames(r *http.Request, tenantID uuid.UUID, txs []models.LedgerTransaction) map[string]string {
	ids := []uuid.UUID{}
	for _, t := range txs {
		for _, e := range t.Entries {
			ids = append(ids, e.AccountID)
		}
	}
	out := map[string]string{}
	if len(ids) == 0 {
		return out
	}
	var accs []models.LedgerAccount
	u.svc.DB().WithContext(r.Context()).Where("tenant_id = ? AND id IN ?", tenantID, ids).Find(&accs)
	for _, a := range accs {
		label := a.Code
		if a.CustomerID != nil {
			switch a.Code {
			case ledger.ReceivableCode(*a.CustomerID):
				label = "customer receivable"
			case ledger.CreditCode(*a.CustomerID):
				label = "customer credit"
			}
		}
		out[a.ID.String()] = label
	}
	return out
}

// parsePercent reads "10" or "7.5%" into basis points (1000, 750).
func parsePercent(raw string) (int, error) {
	raw = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(raw), "%"))
	whole, frac, _ := strings.Cut(raw, ".")
	if len(frac) > 2 {
		return 0, fmt.Errorf("use at most two decimals, like 7.25")
	}
	for len(frac) < 2 {
		frac += "0"
	}
	n, err := strconv.Atoi(whole + frac)
	if err != nil || n < 0 || n > 10000 {
		return 0, fmt.Errorf("enter a percent between 0 and 100, like 10")
	}
	return n, nil
}

// setupStep is one item of the dashboard's getting-started list.
type setupStep struct {
	Title, Hint, Link, Action string
	Done                      bool
}

// setupSteps tracks a new tenant's path to its first paid subscription.
func (u *UI) setupSteps(r *http.Request, tenantID uuid.UUID) []setupStep {
	db := u.svc.DB().WithContext(r.Context())
	count := func(model any, where string, args ...any) bool {
		var n int64
		db.Model(model).Where("tenant_id = ?", tenantID).Where(where, args...).Count(&n)
		return n > 0
	}
	return []setupStep{
		{Title: "Create a plan", Hint: "What you sell and what it costs, e.g. Pro at 29,000 MNT a month.",
			Link: "/ui/plans", Action: "Create a plan", Done: count(&models.Plan{}, "true")},
		{Title: "Add a customer", Hint: "Someone who pays you. Your app usually does this through the API.",
			Link: "/ui/customers", Action: "Add a customer", Done: count(&models.Customer{}, "true")},
		{Title: "Subscribe the customer", Hint: "Open the customer and pick a plan. This creates their first invoice.",
			Link: "/ui/customers", Action: "Choose a customer", Done: count(&models.Subscription{}, "true")},
		{Title: "Take a test payment", Hint: "Open the invoice and pay it with the test provider. No real money moves.",
			Link: "/ui/invoices?status=open", Action: "Open unpaid invoices", Done: count(&models.Payment{}, "status = ?", domain.PaymentSucceeded)},
	}
}
