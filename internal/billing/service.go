// Package billing is the business core: tenants, API keys, customers, plans,
// subscriptions, invoices and payments. Every exported method takes the tenant
// explicitly and scopes every query by it. The HTTP API and the web UI are thin
// layers over this package, so both enforce exactly the same rules.
//
// The rules themselves are written down in docs/billing-model.md; read that
// before changing anything here.
package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/orshih6/billing/internal/models"
	"github.com/orshih6/billing/internal/providers"
	"github.com/orshih6/billing/internal/secretbox"
)

// Service is the billing core.
type Service struct {
	db        *gorm.DB
	providers *providers.Registry
	log       *slog.Logger
	// Now is the clock. Tests and the worker's --now flag replace it.
	Now func() time.Time
	// Production disables the mock provider unless a tenant opts in.
	Production bool
	// WebhookAllowPrivate skips the SSRF guard on webhook URLs (dev only).
	WebhookAllowPrivate bool
	// Secrets encrypts provider credentials; nil means none can be stored.
	Secrets *secretbox.Box
}

// New builds a service.
func New(db *gorm.DB, reg *providers.Registry, log *slog.Logger, production bool) *Service {
	return &Service{
		db: db, providers: reg, log: log, Production: production,
		Now: func() time.Time { return time.Now().UTC() },
	}
}

// DB exposes the handle for read-only views (ledger, reports).
func (s *Service) DB() *gorm.DB { return s.db }

// Providers exposes the registry.
func (s *Service) Providers() *providers.Registry { return s.providers }

// now is truncated to Postgres' microsecond precision, so a time written and
// read back compares equal to the in-memory value (period boundaries are
// matched by equality).
func (s *Service) now() time.Time { return s.Now().UTC().Truncate(time.Microsecond) }

// Error is a failure the caller can act on. The API maps it to an HTTP status
// and {"error":{"code","message"}}.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func notFound(what string) error {
	return &Error{http.StatusNotFound, what + "_not_found", what + " not found"}
}

func invalid(code, format string, args ...any) error {
	return &Error{http.StatusUnprocessableEntity, code, fmt.Sprintf(format, args...)}
}

func conflict(code, format string, args ...any) error {
	return &Error{http.StatusConflict, code, fmt.Sprintf(format, args...)}
}

func forbidden(code, format string, args ...any) error {
	return &Error{http.StatusForbidden, code, fmt.Sprintf(format, args...)}
}

// AsError extracts a *Error.
func AsError(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}

// Page asks for one page of a list, newest first.
type Page struct {
	Limit int
	// StartingAfter is the id of the last item of the previous page.
	StartingAfter *uuid.UUID
}

// Common filters every list accepts.
type Common struct {
	CreatedAfter  *time.Time
	CreatedBefore *time.Time
	// Metadata matches rows whose metadata contains every key=value (only
	// on resources that have metadata).
	Metadata map[string]string
}

func (c Common) apply(q *gorm.DB, table string, hasMetadata bool) (*gorm.DB, error) {
	if c.CreatedAfter != nil {
		q = q.Where(table+".created_at >= ?", *c.CreatedAfter)
	}
	if c.CreatedBefore != nil {
		q = q.Where(table+".created_at < ?", *c.CreatedBefore)
	}
	if len(c.Metadata) > 0 {
		if !hasMetadata {
			return nil, invalid("invalid_filter", "%s cannot be filtered by metadata", table)
		}
		b, _ := json.Marshal(c.Metadata)
		q = q.Where(table+".metadata @> ?::jsonb", string(b))
	}
	return q, nil
}

// likePattern escapes a user's search text for ILIKE.
func likePattern(s string) string {
	s = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(strings.TrimSpace(s))
	return "%" + s + "%"
}

// List is one page of results.
type List[T any] struct {
	Data    []T  `json:"data"`
	HasMore bool `json:"has_more"`
}

// paginate applies keyset pagination over (created_at, id) descending on the
// named table.
func paginate[T any](q *gorm.DB, table string, p Page) (List[T], error) {
	limit := p.Limit
	if limit <= 0 || limit > 100 {
		limit = 25
	}
	if p.StartingAfter != nil {
		q = q.Where(fmt.Sprintf("(%[1]s.created_at, %[1]s.id) < (SELECT created_at, id FROM %[1]s WHERE id = ?)", table), *p.StartingAfter)
	}
	var rows []T
	if err := q.Order(table + ".created_at DESC").Order(table + ".id DESC").Limit(limit + 1).Find(&rows).Error; err != nil {
		return List[T]{}, err
	}
	out := List[T]{Data: rows}
	if len(rows) > limit {
		out.Data = rows[:limit]
		out.HasMore = true
	}
	if out.Data == nil {
		out.Data = []T{}
	}
	return out, nil
}

// take loads one tenant-scoped row or returns a typed not-found.
func take[T any](q *gorm.DB, tenantID, id uuid.UUID, what string) (*T, error) {
	var row T
	err := q.Where("tenant_id = ? AND id = ?", tenantID, id).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, notFound(what)
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// lock is take with SELECT … FOR UPDATE.
func lock[T any](tx *gorm.DB, tenantID, id uuid.UUID, what string) (*T, error) {
	return take[T](tx.Clauses(clause.Locking{Strength: "UPDATE"}), tenantID, id, what)
}

func (s *Service) tx(ctx context.Context, fn func(tx *gorm.DB) error) error {
	return s.db.WithContext(ctx).Transaction(fn)
}

// Tenant loads a tenant.
func (s *Service) Tenant(ctx context.Context, id uuid.UUID) (*models.Tenant, error) {
	var t models.Tenant
	err := s.db.WithContext(ctx).Take(&t, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, notFound("tenant")
	}
	return &t, err
}

func (s *Service) settings(tx *gorm.DB, tenantID uuid.UUID) (*models.Tenant, models.TenantSettings, error) {
	var t models.Tenant
	if err := tx.Take(&t, "id = ?", tenantID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, models.TenantSettings{}, notFound("tenant")
		}
		return nil, models.TenantSettings{}, err
	}
	return &t, t.EffectiveSettings(), nil
}

func ptr[T any](v T) *T { return &v }
