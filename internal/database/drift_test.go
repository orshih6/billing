package database_test

import (
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"sort"
	"strings"
	"testing"

	"gorm.io/gorm"

	"github.com/orshih6/billing/internal/config"
	"github.com/orshih6/billing/internal/database"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func schemaDB(t *testing.T, admin *gorm.DB, dsn, name string) *gorm.DB {
	t.Helper()
	if err := admin.Exec("CREATE SCHEMA " + name).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec("DROP SCHEMA " + name + " CASCADE") })
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	db, err := database.Open(config.DatabaseConfig{DSN: dsn + sep + "search_path=" + name, MaxOpenConns: 2}, quiet())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close(db) })
	return db
}

// describe renders a schema as sorted lines: columns, indexes, constraints.
func describe(t *testing.T, db *gorm.DB, schema string) []string {
	t.Helper()
	var out []string
	var cols []struct{ TableName, ColumnName, DataType, IsNullable string }
	db.Raw(`SELECT table_name, column_name, data_type, is_nullable FROM information_schema.columns
	         WHERE table_schema = ? AND table_name <> 'goose_db_version'`, schema).Scan(&cols)
	for _, c := range cols {
		out = append(out, fmt.Sprintf("column %s.%s %s null=%s", c.TableName, c.ColumnName, c.DataType, c.IsNullable))
	}
	var idx []struct{ Indexname, Indexdef string }
	db.Raw(`SELECT indexname, indexdef FROM pg_indexes WHERE schemaname = ? AND tablename <> 'goose_db_version'`, schema).Scan(&idx)
	for _, i := range idx {
		out = append(out, "index "+normalize(i.Indexdef, schema))
	}
	var cons []struct{ Conname, Def string }
	db.Raw(`SELECT c.conname, pg_get_constraintdef(c.oid) AS def FROM pg_constraint c
	          JOIN pg_namespace n ON n.oid = c.connamespace
	          JOIN pg_class r ON r.oid = c.conrelid
	         WHERE n.nspname = ? AND r.relname <> 'goose_db_version'`, schema).Scan(&cons)
	for _, c := range cons {
		out = append(out, "constraint "+c.Conname+" "+normalize(c.Def, schema))
	}
	sort.Strings(out)
	return out
}

// The SQL migrations and the GORM models must describe the same schema. When
// this fails you changed a model without adding a migration (or the reverse):
// write migrations/000NN_<change>.sql so they agree again.
func TestMigrationsMatchModels(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	admin, err := database.Open(config.DatabaseConfig{DSN: dsn, MaxOpenConns: 2}, quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(admin)
	suffix := fmt.Sprintf("%d_%d", os.Getpid(), rand.IntN(1_000_000))
	fromSQL, fromModels := "drift_sql_"+suffix, "drift_models_"+suffix

	a := schemaDB(t, admin, dsn, fromSQL)
	if err := database.Migrate(a); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	b := schemaDB(t, admin, dsn, fromModels)
	if err := database.AutoMigrate(b); err != nil {
		t.Fatalf("automigrate: %v", err)
	}

	want, got := describe(t, admin, fromModels), describe(t, admin, fromSQL)
	missing, extra := diff(want, got), diff(got, want)
	if len(missing)+len(extra) > 0 {
		t.Fatalf("schema drift between models and migrations:\n  in models, not migrations:\n    %s\n  in migrations, not models:\n    %s",
			strings.Join(missing, "\n    "), strings.Join(extra, "\n    "))
	}
	if len(want) < 100 {
		t.Fatalf("suspiciously small schema description (%d lines)", len(want))
	}

	// Down then up again works.
	if err := database.MigrateDown(a); err != nil {
		t.Fatalf("down: %v", err)
	}
	if err := database.Migrate(a); err != nil {
		t.Fatalf("up again: %v", err)
	}
}

// A database created by the old AutoMigrate is adopted, not recreated.
func TestLegacyDatabaseIsBaselined(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	admin, err := database.Open(config.DatabaseConfig{DSN: dsn, MaxOpenConns: 2}, quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(admin)
	db := schemaDB(t, admin, dsn, fmt.Sprintf("drift_legacy_%d_%d", os.Getpid(), rand.IntN(1_000_000)))
	if err := database.AutoMigrate(db); err != nil {
		t.Fatal(err)
	}
	db.Exec(`INSERT INTO tenants (id, slug, name, default_currency, settings, status, created_at, updated_at)
	         VALUES (gen_random_uuid(), 'keep-me', 'Keep', 'MNT', '{}', 'active', now(), now())`)
	if err := database.Migrate(db); err != nil {
		t.Fatalf("migrating a legacy database: %v", err)
	}
	var n int64
	db.Raw(`SELECT count(*) FROM tenants WHERE slug = 'keep-me'`).Scan(&n)
	if n != 1 {
		t.Fatal("legacy data lost")
	}
	st, err := database.MigrationStatus(db)
	if err != nil || len(st) == 0 || !strings.Contains(st[0], "applied") {
		t.Fatalf("status after baseline: %v %v", st, err)
	}
}

// normalize drops what differs only in how Postgres prints a definition: the
// schema name and redundant casts and parentheses in partial-index conditions.
func normalize(def, schema string) string {
	def = strings.ReplaceAll(def, schema+".", "")
	for _, noise := range []string{"::character varying", "::text", "[]", "(", ")", " "} {
		def = strings.ReplaceAll(def, noise, "")
	}
	return def
}

func diff(a, b []string) []string {
	in := map[string]bool{}
	for _, x := range b {
		in[x] = true
	}
	var out []string
	for _, x := range a {
		if !in[x] {
			out = append(out, x)
		}
	}
	return out
}
