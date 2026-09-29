package billing

import (
	"context"

	"github.com/google/uuid"

	"github.com/orshih6/billing/internal/domain"
	"github.com/orshih6/billing/internal/ledger"
	"github.com/orshih6/billing/internal/models"
)

// LedgerAccountFilter narrows ListLedgerAccounts.
type LedgerAccountFilter struct {
	CustomerID *uuid.UUID
	Currency   string
	// SystemOnly hides per-customer accounts.
	SystemOnly bool
}

// ListLedgerAccounts lists accounts.
func (s *Service) ListLedgerAccounts(ctx context.Context, tenantID uuid.UUID, f LedgerAccountFilter, p Page) (List[models.LedgerAccount], error) {
	q := s.db.WithContext(ctx).Model(&models.LedgerAccount{}).Where("tenant_id = ?", tenantID)
	if f.CustomerID != nil {
		q = q.Where("customer_id = ?", *f.CustomerID)
	}
	if f.SystemOnly {
		q = q.Where("customer_id IS NULL")
	}
	if f.Currency != "" {
		q = q.Where("currency = ?", f.Currency)
	}
	return paginate[models.LedgerAccount](q, "ledger_accounts", p)
}

// LedgerAccount loads one account.
func (s *Service) LedgerAccount(ctx context.Context, tenantID, id uuid.UUID) (*models.LedgerAccount, error) {
	return take[models.LedgerAccount](s.db.WithContext(ctx), tenantID, id, "ledger_account")
}

// EntryView is an entry with its transaction's description.
type EntryView struct {
	models.LedgerEntry
	Kind        string     `json:"kind"`
	Description string     `json:"description"`
	InvoiceID   *uuid.UUID `json:"invoice_id"`
	PaymentID   *uuid.UUID `json:"payment_id"`
}

// AccountEntries lists an account's entries, newest first (simple limit, no
// cursor: entries are for inspection, not export).
func (s *Service) AccountEntries(ctx context.Context, tenantID, accountID uuid.UUID, limit int) ([]EntryView, error) {
	if _, err := s.LedgerAccount(ctx, tenantID, accountID); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	out := []EntryView{}
	err := s.db.WithContext(ctx).Table("ledger_entries e").
		Select("e.*, t.kind, t.description, t.invoice_id, t.payment_id").
		Joins("JOIN ledger_transactions t ON t.id = e.transaction_id").
		Where("e.tenant_id = ? AND e.account_id = ?", tenantID, accountID).
		Order("e.id DESC").Limit(limit).Scan(&out).Error
	return out, err
}

// LedgerTransactionFilter narrows ListLedgerTransactions.
type LedgerTransactionFilter struct {
	InvoiceID  *uuid.UUID
	PaymentID  *uuid.UUID
	CustomerID *uuid.UUID
}

// ListLedgerTransactions lists postings with their entries.
func (s *Service) ListLedgerTransactions(ctx context.Context, tenantID uuid.UUID, f LedgerTransactionFilter, p Page) (List[models.LedgerTransaction], error) {
	q := s.db.WithContext(ctx).Model(&models.LedgerTransaction{}).Where("tenant_id = ?", tenantID).Preload("Entries")
	if f.InvoiceID != nil {
		q = q.Where("invoice_id = ?", *f.InvoiceID)
	}
	if f.PaymentID != nil {
		q = q.Where("payment_id = ?", *f.PaymentID)
	}
	if f.CustomerID != nil {
		q = q.Where("customer_id = ?", *f.CustomerID)
	}
	return paginate[models.LedgerTransaction](q, "ledger_transactions", p)
}

// Reconciliation is the result of recomputing the books.
type Reconciliation struct {
	OK         bool              `json:"ok"`
	Mismatches []ledger.Mismatch `json:"mismatches"`
	// InvoiceMismatches are open invoices whose amount_due disagrees with
	// total - paid - credit.
	InvoiceMismatches []uuid.UUID `json:"invoice_mismatches"`
}

// Reconcile recomputes balances from entries and cross-checks invoices.
func (s *Service) Reconcile(ctx context.Context, tenantID uuid.UUID) (*Reconciliation, error) {
	mm, err := ledger.Reconcile(s.db.WithContext(ctx), tenantID)
	if err != nil {
		return nil, err
	}
	var bad []uuid.UUID
	if err := s.db.WithContext(ctx).Model(&models.Invoice{}).
		Where("tenant_id = ? AND status = ? AND amount_due <> total - amount_paid - credit_applied", tenantID, domain.InvoiceOpen).
		Pluck("id", &bad).Error; err != nil {
		return nil, err
	}
	if mm == nil {
		mm = []ledger.Mismatch{}
	}
	if bad == nil {
		bad = []uuid.UUID{}
	}
	return &Reconciliation{OK: len(mm) == 0 && len(bad) == 0, Mismatches: mm, InvoiceMismatches: bad}, nil
}

// Stats summarises a tenant for the dashboard.
type Stats struct {
	Customers           int64            `json:"customers"`
	ActiveSubscriptions int64            `json:"active_subscriptions"`
	Trialing            int64            `json:"trialing"`
	PastDue             int64            `json:"past_due"`
	OpenInvoices        int64            `json:"open_invoices"`
	Outstanding         map[string]int64 `json:"outstanding"`
	// MRR is monthly recurring revenue per currency from active and past_due
	// subscriptions, normalised to 30-day months.
	MRR map[string]int64 `json:"mrr"`
	// Collected is succeeded payments in the last 30 days, per currency.
	Collected map[string]int64 `json:"collected_30d"`
}

// TenantStats computes dashboard numbers.
func (s *Service) TenantStats(ctx context.Context, tenantID uuid.UUID) (*Stats, error) {
	db := s.db.WithContext(ctx)
	st := &Stats{Outstanding: map[string]int64{}, MRR: map[string]int64{}, Collected: map[string]int64{}}
	db.Model(&models.Customer{}).Where("tenant_id = ?", tenantID).Count(&st.Customers)
	db.Model(&models.Subscription{}).Where("tenant_id = ? AND status = ?", tenantID, domain.SubActive).Count(&st.ActiveSubscriptions)
	db.Model(&models.Subscription{}).Where("tenant_id = ? AND status = ?", tenantID, domain.SubTrialing).Count(&st.Trialing)
	db.Model(&models.Subscription{}).Where("tenant_id = ? AND status = ?", tenantID, domain.SubPastDue).Count(&st.PastDue)
	db.Model(&models.Invoice{}).Where("tenant_id = ? AND status = ?", tenantID, domain.InvoiceOpen).Count(&st.OpenInvoices)

	var sums []struct {
		Currency string
		Total    int64
	}
	db.Model(&models.Invoice{}).Select("currency, SUM(amount_due) AS total").
		Where("tenant_id = ? AND status = ?", tenantID, domain.InvoiceOpen).Group("currency").Scan(&sums)
	for _, r := range sums {
		st.Outstanding[r.Currency] = r.Total
	}
	sums = nil
	db.Model(&models.Payment{}).Select("currency, SUM(amount) AS total").
		Where("tenant_id = ? AND status = ? AND settled_at >= ?", tenantID, domain.PaymentSucceeded, s.now().AddDate(0, 0, -30)).
		Group("currency").Scan(&sums)
	for _, r := range sums {
		st.Collected[r.Currency] = r.Total
	}

	// Base seats and add-ons of paying subscriptions, before discounts and tax.
	var rows []struct {
		models.Price
		Qty int64
	}
	paying := []domain.SubscriptionStatus{domain.SubActive, domain.SubPastDue}
	db.Raw(`SELECT p.*, s.quantity AS qty FROM prices p JOIN subscriptions s ON s.price_id = p.id
	         WHERE s.tenant_id = ? AND s.status IN ?
	        UNION ALL
	        SELECT p.*, i.quantity FROM prices p JOIN subscription_items i ON i.price_id = p.id
	          JOIN subscriptions s ON s.id = i.subscription_id
	         WHERE s.tenant_id = ? AND s.status IN ? AND i.removed_at IS NULL`,
		tenantID, paying, tenantID, paying).Scan(&rows)
	for _, r := range rows {
		st.MRR[r.Currency] += monthly(r.Price) * r.Qty
	}
	return st, nil
}

// monthly normalises a price to a 30-day month.
func monthly(p models.Price) int64 {
	n := int64(max(p.IntervalCount, 1))
	switch p.Interval {
	case domain.IntervalDay:
		return p.Amount * 30 / n
	case domain.IntervalWeek:
		return p.Amount * 30 / (7 * n)
	case domain.IntervalMonth:
		return p.Amount / n
	case domain.IntervalYear:
		return p.Amount / (12 * n)
	}
	return 0
}

// Events lists a tenant's events, newest first.
func (s *Service) Events(ctx context.Context, tenantID uuid.UUID, eventType string, p Page) (List[models.Event], error) {
	q := s.db.WithContext(ctx).Model(&models.Event{}).Where("tenant_id = ?", tenantID)
	if eventType != "" {
		q = q.Where("type = ?", eventType)
	}
	return paginate[models.Event](q, "events", p)
}
