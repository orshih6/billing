package ledger

import (
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/orshih6/billing/internal/models"
	"github.com/orshih6/billing/internal/testsupport"
)

func TestMain(m *testing.M) { os.Exit(testsupport.Run(m)) }

type fixture struct {
	db       *gorm.DB
	tenant   uuid.UUID
	customer uuid.UUID
}

func setup(t *testing.T) fixture {
	db := testsupport.DB(t)
	return fixture{db: db, tenant: uuid.New(), customer: uuid.New()}
}

func (f fixture) post(t *testing.T, key string, build func(tx *gorm.DB) []Line) (*models.LedgerTransaction, bool, error) {
	t.Helper()
	var (
		txn      *models.LedgerTransaction
		replayed bool
	)
	err := f.db.Transaction(func(tx *gorm.DB) error {
		var err error
		txn, replayed, err = Post(tx, Posting{
			TenantID: f.tenant, Kind: "test", IdempotencyKey: key, Currency: "MNT",
			Lines: build(tx),
		})
		return err
	})
	return txn, replayed, err
}

func acc(t *testing.T) func(*models.LedgerAccount, error) *models.LedgerAccount {
	return func(a *models.LedgerAccount, err error) *models.LedgerAccount {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
}

func TestPostBalancesAndReplays(t *testing.T) {
	f := setup(t)
	invoice := func(tx *gorm.DB) []Line {
		recv := acc(t)(Receivable(tx, f.tenant, f.customer, "MNT"))
		rev := acc(t)(System(tx, f.tenant, CodeRevenue, "MNT"))
		return []Line{Debit(recv, 5000), Credit(rev, 5000)}
	}
	if _, replayed, err := f.post(t, "inv-1", invoice); err != nil || replayed {
		t.Fatalf("first post: replayed=%v err=%v", replayed, err)
	}
	if _, replayed, err := f.post(t, "inv-1", invoice); err != nil || !replayed {
		t.Fatalf("second post must replay: replayed=%v err=%v", replayed, err)
	}

	var recv models.LedgerAccount
	f.db.Where("code = ?", ReceivableCode(f.customer)).Take(&recv)
	if recv.Balance != 5000 {
		t.Fatalf("receivable balance %d, want 5000 (posted once)", recv.Balance)
	}
	var rev models.LedgerAccount
	f.db.Where("code = ?", CodeRevenue).Take(&rev)
	if rev.Balance != 5000 {
		t.Fatalf("revenue (credit-normal) balance %d, want 5000", rev.Balance)
	}
	if mm, err := Reconcile(f.db, f.tenant); err != nil || len(mm) != 0 {
		t.Fatalf("reconcile: %v %+v", err, mm)
	}
}

func TestPostRejectsBadPostings(t *testing.T) {
	f := setup(t)
	cases := map[string]struct {
		lines func(tx *gorm.DB) []Line
		want  error
	}{
		"unbalanced": {func(tx *gorm.DB) []Line {
			recv := acc(t)(Receivable(tx, f.tenant, f.customer, "MNT"))
			rev := acc(t)(System(tx, f.tenant, CodeRevenue, "MNT"))
			return []Line{Debit(recv, 100), Credit(rev, 99)}
		}, ErrUnbalanced},
		"both sides": {func(tx *gorm.DB) []Line {
			rev := acc(t)(System(tx, f.tenant, CodeRevenue, "MNT"))
			return []Line{{AccountID: rev.ID, Debit: 1, Credit: 1}}
		}, ErrInvalidLine},
		"currency": {func(tx *gorm.DB) []Line {
			recv := acc(t)(Receivable(tx, f.tenant, f.customer, "USD"))
			rev := acc(t)(System(tx, f.tenant, CodeRevenue, "USD"))
			return []Line{Debit(recv, 1), Credit(rev, 1)}
		}, ErrCurrencyMismatch},
		"customer credit negative": {func(tx *gorm.DB) []Line {
			credit := acc(t)(CustomerCredit(tx, f.tenant, f.customer, "MNT"))
			recv := acc(t)(Receivable(tx, f.tenant, f.customer, "MNT"))
			return []Line{Debit(credit, 10), Credit(recv, 10)}
		}, ErrNegativeBalance},
		"other tenant": {func(tx *gorm.DB) []Line {
			theirs := acc(t)(System(tx, uuid.New(), CodeRevenue, "MNT"))
			ours := acc(t)(System(tx, f.tenant, CodeBadDebt, "MNT"))
			return []Line{Debit(ours, 1), Credit(theirs, 1)}
		}, ErrTenantMismatch},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := f.post(t, "bad-"+name, c.lines)
			if !errors.Is(err, c.want) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
		})
	}
}

func TestEntriesAreAppendOnly(t *testing.T) {
	f := setup(t)
	f.post(t, "x", func(tx *gorm.DB) []Line {
		recv := acc(t)(Receivable(tx, f.tenant, f.customer, "MNT"))
		rev := acc(t)(System(tx, f.tenant, CodeRevenue, "MNT"))
		return []Line{Debit(recv, 1), Credit(rev, 1)}
	})
	if err := f.db.Exec("UPDATE ledger_entries SET debit = 2 WHERE debit = 1").Error; err == nil {
		t.Fatal("update of a ledger entry must be refused")
	}
	if err := f.db.Exec("DELETE FROM ledger_entries").Error; err == nil {
		t.Fatal("delete of a ledger entry must be refused")
	}
}

// Concurrent posters with one key: exactly one posting, all see success.
func TestConcurrentSameKey(t *testing.T) {
	f := setup(t)
	f.db.Transaction(func(tx *gorm.DB) error {
		_, err := Receivable(tx, f.tenant, f.customer, "MNT")
		_, err2 := System(tx, f.tenant, CodeRevenue, "MNT")
		return errors.Join(err, err2)
	})
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := f.post(t, "race", func(tx *gorm.DB) []Line {
				recv, _ := Receivable(tx, f.tenant, f.customer, "MNT")
				rev, _ := System(tx, f.tenant, CodeRevenue, "MNT")
				return []Line{Debit(recv, 700), Credit(rev, 700)}
			})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent post failed: %v", err)
		}
	}
	var n int64
	f.db.Model(&models.LedgerTransaction{}).Count(&n)
	if n != 1 {
		t.Fatalf("%d transactions, want 1", n)
	}
	if mm, _ := Reconcile(f.db, f.tenant); len(mm) != 0 {
		t.Fatalf("books unbalanced: %+v", mm)
	}
}
