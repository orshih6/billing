// Package ledger is the only code that writes ledger_transactions and
// ledger_entries. Everything that moves money calls Post inside the caller's
// database transaction, so the business state change (an invoice becoming
// paid) and its posting commit or roll back together.
//
// Invariants enforced here:
//
//   - every posting balances: Σ debit = Σ credit;
//   - every leg's account belongs to the posting's tenant and currency (there
//     is no FX; an account holds exactly one currency);
//   - a posting is identified by (tenant, idempotency key): posting the same
//     key twice returns the original and changes nothing (replayed = true);
//   - accounts that belong to a customer never go negative — a customer can
//     neither owe a negative amount nor hold negative credit.
//
// Entries are append-only (a database trigger refuses UPDATE and DELETE). A
// mistake is corrected with a reversing posting, never by editing history.
package ledger

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/orshih6/billing/internal/database"
	"github.com/orshih6/billing/internal/models"
)

// Well-known account codes. System accounts exist once per tenant and
// currency; customer accounts once per customer and currency.
const (
	CodeRevenue      = "revenue"
	CodeBadDebt      = "bad_debt"
	CodeRefunds      = "refunds"
	CodeCreditGrants = "credit_grants"
	CodeDiscounts    = "discounts"
	CodeTaxPayable   = "tax_payable"
)

// CashCode is the clearing account money lands in for a provider.
func CashCode(provider string) string { return "cash:" + provider }

// ReceivableCode is what a customer owes.
func ReceivableCode(customerID uuid.UUID) string {
	return "customer:" + customerID.String() + ":receivable"
}

// CreditCode is the customer's prepaid/credit balance (a liability: money we
// hold on their behalf).
func CreditCode(customerID uuid.UUID) string { return "customer:" + customerID.String() + ":credit" }

var systemAccounts = map[string]struct {
	typ  models.LedgerAccountType
	name string
}{
	CodeRevenue:      {models.AccountRevenue, "Revenue"},
	CodeBadDebt:      {models.AccountExpense, "Bad debt (uncollectible invoices)"},
	CodeRefunds:      {models.AccountExpense, "Refunds"},
	CodeCreditGrants: {models.AccountExpense, "Credit granted to customers"},
	CodeDiscounts:    {models.AccountExpense, "Discounts (coupons)"},
	CodeTaxPayable:   {models.AccountLiability, "Tax collected, owed to the tax authority"},
}

// Errors.
var (
	ErrUnbalanced       = errors.New("ledger: posting does not balance")
	ErrInvalidLine      = errors.New("ledger: every line needs exactly one positive side")
	ErrCurrencyMismatch = errors.New("ledger: account currency differs from the posting")
	ErrTenantMismatch   = errors.New("ledger: account belongs to another tenant")
	ErrNegativeBalance  = errors.New("ledger: posting would make a customer account negative")
	ErrNoKey            = errors.New("ledger: an idempotency key is required")
)

// Line is one leg of a posting.
type Line struct {
	AccountID uuid.UUID
	Debit     int64
	Credit    int64
}

// Debit and Credit build lines.
func Debit(account *models.LedgerAccount, amount int64) Line {
	return Line{AccountID: account.ID, Debit: amount}
}

func Credit(account *models.LedgerAccount, amount int64) Line {
	return Line{AccountID: account.ID, Credit: amount}
}

// Posting describes one balanced movement.
type Posting struct {
	TenantID       uuid.UUID
	Kind           string
	IdempotencyKey string
	Currency       string
	InvoiceID      *uuid.UUID
	PaymentID      *uuid.UUID
	CustomerID     *uuid.UUID
	Description    string
	EffectiveAt    time.Time
	Lines          []Line
}

// Post writes a balanced posting and updates account balances. tx must be a
// transaction. Zero-amount lines are dropped; a posting that ends up empty is
// a no-op and returns (nil, false, nil).
func Post(tx *gorm.DB, p Posting) (*models.LedgerTransaction, bool, error) {
	if p.IdempotencyKey == "" {
		return nil, false, ErrNoKey
	}

	lines := make([]Line, 0, len(p.Lines))
	var debits, credits int64
	for _, l := range p.Lines {
		if l.Debit == 0 && l.Credit == 0 {
			continue
		}
		if l.Debit < 0 || l.Credit < 0 || (l.Debit > 0 && l.Credit > 0) {
			return nil, false, ErrInvalidLine
		}
		debits += l.Debit
		credits += l.Credit
		lines = append(lines, l)
	}
	if debits != credits {
		return nil, false, fmt.Errorf("%w: debits %d, credits %d", ErrUnbalanced, debits, credits)
	}
	if len(lines) == 0 {
		return nil, false, nil
	}

	if existing, err := find(tx, p.TenantID, p.IdempotencyKey); err != nil || existing != nil {
		return existing, existing != nil, err
	}

	// Lock every touched account in id order, so two postings over the same
	// accounts cannot deadlock.
	ids := make([]uuid.UUID, 0, len(lines))
	seen := map[uuid.UUID]bool{}
	for _, l := range lines {
		if !seen[l.AccountID] {
			seen[l.AccountID] = true
			ids = append(ids, l.AccountID)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })

	var accounts []models.LedgerAccount
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id IN ?", ids).Order("id").Find(&accounts).Error; err != nil {
		return nil, false, fmt.Errorf("ledger: lock accounts: %w", err)
	}
	if len(accounts) != len(ids) {
		return nil, false, fmt.Errorf("ledger: %d of %d accounts not found", len(ids)-len(accounts), len(ids))
	}
	byID := make(map[uuid.UUID]*models.LedgerAccount, len(accounts))
	for i := range accounts {
		a := &accounts[i]
		if a.TenantID != p.TenantID {
			return nil, false, ErrTenantMismatch
		}
		if a.Currency != p.Currency {
			return nil, false, fmt.Errorf("%w: %s is %s, posting is %s", ErrCurrencyMismatch, a.Code, a.Currency, p.Currency)
		}
		byID[a.ID] = a
	}

	delta := map[uuid.UUID]int64{}
	for _, l := range lines {
		a := byID[l.AccountID]
		if a.Type.DebitNormal() {
			delta[a.ID] += l.Debit - l.Credit
		} else {
			delta[a.ID] += l.Credit - l.Debit
		}
	}
	for id, d := range delta {
		a := byID[id]
		if a.CustomerID != nil && a.Balance+d < 0 {
			return nil, false, fmt.Errorf("%w: %s would be %d", ErrNegativeBalance, a.Code, a.Balance+d)
		}
	}

	if p.EffectiveAt.IsZero() {
		p.EffectiveAt = time.Now().UTC()
	}
	txn := &models.LedgerTransaction{
		TenantID:       p.TenantID,
		Kind:           p.Kind,
		IdempotencyKey: p.IdempotencyKey,
		Currency:       p.Currency,
		InvoiceID:      p.InvoiceID,
		PaymentID:      p.PaymentID,
		CustomerID:     p.CustomerID,
		Description:    p.Description,
		EffectiveAt:    p.EffectiveAt,
	}

	// A concurrent poster with the same key wins the unique index; roll back
	// to the savepoint and return theirs, so the caller's transaction stays
	// usable.
	sp := "ledger_" + uuid.NewString()[:8]
	if err := tx.SavePoint(sp).Error; err != nil {
		return nil, false, err
	}
	if err := tx.Create(txn).Error; err != nil {
		if database.IsUniqueViolation(err) {
			if rbErr := tx.RollbackTo(sp).Error; rbErr != nil {
				return nil, false, rbErr
			}
			existing, ferr := find(tx, p.TenantID, p.IdempotencyKey)
			if ferr != nil {
				return nil, false, ferr
			}
			return existing, true, nil
		}
		return nil, false, fmt.Errorf("ledger: insert transaction: %w", err)
	}

	entries := make([]models.LedgerEntry, len(lines))
	for i, l := range lines {
		entries[i] = models.LedgerEntry{
			TransactionID: txn.ID,
			TenantID:      p.TenantID,
			AccountID:     l.AccountID,
			Debit:         l.Debit,
			Credit:        l.Credit,
			CreatedAt:     txn.CreatedAt,
		}
	}
	if err := tx.Create(&entries).Error; err != nil {
		return nil, false, fmt.Errorf("ledger: insert entries: %w", err)
	}
	for id, d := range delta {
		if d == 0 {
			continue
		}
		if err := tx.Model(&models.LedgerAccount{}).Where("id = ?", id).
			Updates(map[string]any{"balance": gorm.Expr("balance + ?", d), "updated_at": time.Now().UTC()}).Error; err != nil {
			return nil, false, fmt.Errorf("ledger: update balance: %w", err)
		}
	}
	txn.Entries = entries
	return txn, false, nil
}

func find(tx *gorm.DB, tenantID uuid.UUID, key string) (*models.LedgerTransaction, error) {
	var existing models.LedgerTransaction
	err := tx.Preload("Entries").Where("tenant_id = ? AND idempotency_key = ?", tenantID, key).Take(&existing).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &existing, nil
}

// System returns (creating on first use) a tenant-wide account.
func System(tx *gorm.DB, tenantID uuid.UUID, code, currency string) (*models.LedgerAccount, error) {
	if def, ok := systemAccounts[code]; ok {
		return ensure(tx, tenantID, code, currency, def.typ, def.name, nil)
	}
	if len(code) > 5 && code[:5] == "cash:" {
		return ensure(tx, tenantID, code, currency, models.AccountAsset, "Cash via "+code[5:], nil)
	}
	return nil, fmt.Errorf("ledger: unknown system account %q", code)
}

// Receivable returns the customer's receivable account in a currency.
func Receivable(tx *gorm.DB, tenantID, customerID uuid.UUID, currency string) (*models.LedgerAccount, error) {
	return ensure(tx, tenantID, ReceivableCode(customerID), currency, models.AccountAsset, "Receivable", &customerID)
}

// CustomerCredit returns the customer's credit balance account in a currency.
func CustomerCredit(tx *gorm.DB, tenantID, customerID uuid.UUID, currency string) (*models.LedgerAccount, error) {
	return ensure(tx, tenantID, CreditCode(customerID), currency, models.AccountLiability, "Credit balance", &customerID)
}

func ensure(tx *gorm.DB, tenantID uuid.UUID, code, currency string, typ models.LedgerAccountType, name string, customerID *uuid.UUID) (*models.LedgerAccount, error) {
	now := time.Now().UTC()
	acc := models.LedgerAccount{
		Base:       models.Base{ID: uuid.New(), CreatedAt: now, UpdatedAt: now},
		TenantID:   tenantID,
		Code:       code,
		Currency:   currency,
		Type:       typ,
		Name:       name,
		CustomerID: customerID,
	}
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&acc).Error; err != nil {
		return nil, fmt.Errorf("ledger: ensure account %s: %w", code, err)
	}
	var out models.LedgerAccount
	if err := tx.Where("tenant_id = ? AND code = ? AND currency = ?", tenantID, code, currency).Take(&out).Error; err != nil {
		return nil, fmt.Errorf("ledger: load account %s: %w", code, err)
	}
	return &out, nil
}

// Mismatch is an account whose stored balance disagrees with its entries, or
// a transaction whose entries do not balance.
type Mismatch struct {
	Kind          string    `json:"kind"` // account_balance | transaction_unbalanced
	ID            uuid.UUID `json:"id"`
	Code          string    `json:"code,omitempty"`
	Stored        int64     `json:"stored,omitempty"`
	FromEntries   int64     `json:"from_entries,omitempty"`
	DebitsCredits [2]int64  `json:"debits_credits,omitempty"`
}

// Reconcile recomputes every balance of a tenant from its entries and checks
// every transaction balances. An empty result means the books are clean.
func Reconcile(db *gorm.DB, tenantID uuid.UUID) ([]Mismatch, error) {
	var out []Mismatch

	var accounts []struct {
		ID      uuid.UUID
		Code    string
		Type    models.LedgerAccountType
		Balance int64
		Debits  int64
		Credits int64
	}
	if err := db.Raw(`
		SELECT a.id, a.code, a.type, a.balance,
		       COALESCE(SUM(e.debit), 0) AS debits, COALESCE(SUM(e.credit), 0) AS credits
		  FROM ledger_accounts a
		  LEFT JOIN ledger_entries e ON e.account_id = a.id
		 WHERE a.tenant_id = ?
		 GROUP BY a.id`, tenantID).Scan(&accounts).Error; err != nil {
		return nil, err
	}
	for _, a := range accounts {
		computed := a.Credits - a.Debits
		if a.Type.DebitNormal() {
			computed = -computed
		}
		if computed != a.Balance {
			out = append(out, Mismatch{Kind: "account_balance", ID: a.ID, Code: a.Code, Stored: a.Balance, FromEntries: computed})
		}
	}

	var unbalanced []struct {
		TransactionID uuid.UUID
		Debits        int64
		Credits       int64
	}
	if err := db.Raw(`
		SELECT transaction_id, SUM(debit) AS debits, SUM(credit) AS credits
		  FROM ledger_entries WHERE tenant_id = ?
		 GROUP BY transaction_id HAVING SUM(debit) <> SUM(credit)`, tenantID).Scan(&unbalanced).Error; err != nil {
		return nil, err
	}
	for _, u := range unbalanced {
		out = append(out, Mismatch{Kind: "transaction_unbalanced", ID: u.TransactionID, DebitsCredits: [2]int64{u.Debits, u.Credits}})
	}
	return out, nil
}
