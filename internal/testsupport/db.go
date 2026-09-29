// Package testsupport gives database-backed tests an isolated Postgres schema.
//
// Go runs different packages' tests in parallel, so each test binary gets its
// own schema, created in TestMain and dropped afterwards.
//
//	func TestMain(m *testing.M) { os.Exit(testsupport.Run(m)) }
//	func TestSomething(t *testing.T) { db := testsupport.DB(t); … }
package testsupport

import (
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"strings"
	"testing"

	"gorm.io/gorm"

	"github.com/orshih6/billing/internal/config"
	"github.com/orshih6/billing/internal/database"
	"github.com/orshih6/billing/internal/models"
)

// EnvDSN names the environment variable holding the test database URL.
const EnvDSN = "TEST_DATABASE_URL"

var shared *gorm.DB

// Run sets up an isolated schema, runs the tests and tears it down.
func Run(m *testing.M) int {
	dsn := os.Getenv(EnvDSN)
	if dsn == "" {
		return m.Run() // the tests themselves t.Skip
	}
	schema := fmt.Sprintf("billtest_%d_%06d", os.Getpid(), rand.IntN(1_000_000))

	admin, err := database.Open(config.DatabaseConfig{DSN: dsn, MaxOpenConns: 2}, Logger())
	if err != nil {
		fmt.Fprintf(os.Stderr, "testsupport: connect: %v\n", err)
		return 1
	}
	defer database.Close(admin)
	if err := admin.Exec("CREATE SCHEMA " + schema).Error; err != nil {
		fmt.Fprintf(os.Stderr, "testsupport: create schema: %v\n", err)
		return 1
	}
	defer admin.Exec("DROP SCHEMA " + schema + " CASCADE")

	shared, err = database.Open(config.DatabaseConfig{DSN: withSearchPath(dsn, schema), MaxOpenConns: 20}, Logger())
	if err != nil {
		fmt.Fprintf(os.Stderr, "testsupport: connect to schema: %v\n", err)
		return 1
	}
	defer database.Close(shared)
	if err := database.Migrate(shared); err != nil {
		fmt.Fprintf(os.Stderr, "testsupport: migrate: %v\n", err)
		return 1
	}
	return m.Run()
}

// DB returns the isolated handle with every table emptied, or skips the test
// when no database is configured.
func DB(t *testing.T) *gorm.DB {
	t.Helper()
	if os.Getenv(EnvDSN) == "" {
		t.Skip(EnvDSN + " is not set")
	}
	if shared == nil {
		t.Fatal("testsupport: TestMain must call testsupport.Run")
	}
	names := make([]string, 0, len(models.All()))
	for _, m := range models.All() {
		stmt := &gorm.Statement{DB: shared}
		if err := stmt.Parse(m); err != nil {
			t.Fatalf("testsupport: parse model: %v", err)
		}
		names = append(names, stmt.Schema.Table)
	}
	if err := shared.Exec("TRUNCATE TABLE " + strings.Join(names, ", ") + " RESTART IDENTITY CASCADE").Error; err != nil {
		t.Fatalf("testsupport: truncate: %v", err)
	}
	return shared
}

// Logger discards output unless TEST_LOG is set.
func Logger() *slog.Logger {
	if os.Getenv("TEST_LOG") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func withSearchPath(dsn, schema string) string {
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + "search_path=" + schema
}
