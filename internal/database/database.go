// Package database wires GORM to Postgres and owns schema migration.
package database

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pressly/goose/v3"
	goosedb "github.com/pressly/goose/v3/database"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/orshih6/billing/internal/config"
)

// Open connects to Postgres and applies connection pool settings.
func Open(cfg config.DatabaseConfig, log *slog.Logger) (*gorm.DB, error) {
	level := logger.Warn
	if cfg.LogQueries {
		level = logger.Info
	}
	db, err := gorm.Open(postgres.Open(cfg.DSN), &gorm.Config{
		Logger: logger.New(slogWriter{log}, logger.Config{
			SlowThreshold:             500 * time.Millisecond,
			LogLevel:                  level,
			IgnoreRecordNotFoundError: true,
			ParameterizedQueries:      true,
		}),
		NowFunc:                func() time.Time { return time.Now().UTC() },
		SkipDefaultTransaction: true,
	})
	if err != nil {
		return nil, fmt.Errorf("database: open: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("database: sql handle: %w", err)
	}
	if cfg.MaxOpenConns > 0 {
		sqlDB.SetMaxOpenConns(cfg.MaxOpenConns)
	}
	if cfg.MaxIdleConns > 0 {
		sqlDB.SetMaxIdleConns(cfg.MaxIdleConns)
	}
	if cfg.ConnMaxLifetime > 0 {
		sqlDB.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("database: ping: %w", err)
	}
	return db, nil
}

//go:embed migrations/*.sql
var migrationsFS embed.FS

func migrationProvider(db *gorm.DB) (*goose.Provider, error) {
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	fsys, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return nil, err
	}
	return goose.NewProvider(goosedb.DialectPostgres, sqlDB, fsys)
}

// Migrate applies every pending migration (migrations/*.sql, embedded in the
// binary). It is run by `billing migrate` (a Job in Kubernetes), never on
// server start, so replicas cannot race.
//
// A database created before versioned migrations existed (by AutoMigrate) is
// first brought to the 00001 shape and marked as being at version 1.
func Migrate(db *gorm.DB) error {
	if err := baselineLegacy(db); err != nil {
		return err
	}
	p, err := migrationProvider(db)
	if err != nil {
		return err
	}
	if _, err := p.Up(context.Background()); err != nil {
		return fmt.Errorf("database: migrate: %w", err)
	}
	return nil
}

// MigrateDown rolls back the most recent migration.
func MigrateDown(db *gorm.DB) error {
	p, err := migrationProvider(db)
	if err != nil {
		return err
	}
	_, err = p.Down(context.Background())
	return err
}

// MigrationStatus lists every migration and whether it is applied.
func MigrationStatus(db *gorm.DB) ([]string, error) {
	p, err := migrationProvider(db)
	if err != nil {
		return nil, err
	}
	st, err := p.Status(context.Background())
	if err != nil {
		return nil, err
	}
	out := make([]string, len(st))
	for i, m := range st {
		applied := "pending"
		if m.State == goose.StateApplied {
			applied = "applied " + m.AppliedAt.Format("2006-01-02 15:04")
		}
		out[i] = fmt.Sprintf("%05d  %-28s %s", m.Source.Version, m.Source.Path, applied)
	}
	return out, nil
}

// Close releases the pool.
func Close(db *gorm.DB) error {
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// IsUniqueViolation reports whether err is a Postgres unique violation,
// optionally on a specific constraint or index.
func IsUniqueViolation(err error, constraint ...string) bool {
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "23505" {
		return false
	}
	if len(constraint) == 0 {
		return true
	}
	for _, c := range constraint {
		if pg.ConstraintName == c {
			return true
		}
	}
	return false
}

type slogWriter struct{ log *slog.Logger }

func (w slogWriter) Printf(format string, args ...any) {
	w.log.Debug(fmt.Sprintf(format, args...))
}
