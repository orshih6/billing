package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/orshih6/billing/internal/auth"
)

type ctxKey string

const (
	requestIDKey ctxKey = "request_id"
	principalKey ctxKey = "principal"
	tenantKey    ctxKey = "tenant"
)

// HeaderAPIKey carries the key. Authorization is deliberately not read.
const HeaderAPIKey = "X-API-Key"

// HeaderTenant lets a platform key choose the tenant it acts on.
const HeaderTenant = "X-Tenant-Id"

// PrincipalFrom returns the authenticated caller, or nil.
func PrincipalFrom(ctx context.Context) *auth.Principal {
	p, _ := ctx.Value(principalKey).(*auth.Principal)
	return p
}

// TenantFrom returns the tenant the request acts on.
func TenantFrom(ctx context.Context) uuid.UUID {
	id, _ := ctx.Value(tenantKey).(uuid.UUID)
	return id
}

func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" || len(id) > 100 {
			id = uuid.NewString()
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

// RequestIDFrom returns the request id.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

func requestLogger(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			started := time.Now()
			sw := &statusWriter{ResponseWriter: w}
			next.ServeHTTP(sw, r)
			if sw.status == 0 {
				sw.status = http.StatusOK
			}
			level := slog.LevelInfo
			if sw.status >= 500 {
				level = slog.LevelError
			} else if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" || strings.HasPrefix(r.URL.Path, "/ui/static/") {
				level = slog.LevelDebug
			}
			log.Log(r.Context(), level, "http request",
				"method", r.Method, "path", r.URL.Path, "status", sw.status, "bytes", sw.bytes,
				"duration_ms", time.Since(started).Milliseconds(), "request_id", RequestIDFrom(r.Context()))
		})
	}
}

func recoverPanics(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					log.Error("panic serving request", "error", rec, "path", r.URL.Path,
						"request_id", RequestIDFrom(r.Context()), "stack", string(debug.Stack()))
					respondError(w, http.StatusInternalServerError, "internal_error", "internal server error")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// authenticate resolves X-API-Key to a principal.
func (h *Handler) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, err := h.auth.Authenticate(r.Context(), strings.TrimSpace(r.Header.Get(HeaderAPIKey)))
		if err != nil {
			switch {
			case errors.Is(err, auth.ErrNoCredentials):
				respondError(w, http.StatusUnauthorized, "unauthorized", "an API key is required in the X-API-Key header")
			case errors.Is(err, auth.ErrExpiredKey):
				respondError(w, http.StatusUnauthorized, "key_expired", "this API key has expired")
			case errors.Is(err, auth.ErrRevokedKey):
				respondError(w, http.StatusUnauthorized, "key_revoked", "this API key has been revoked or its tenant suspended")
			case errors.Is(err, auth.ErrInvalidKey):
				respondError(w, http.StatusUnauthorized, "unauthorized", "the API key is not valid")
			default:
				h.log.Error("api key lookup failed", "error", err, "request_id", RequestIDFrom(r.Context()))
				respondError(w, http.StatusInternalServerError, "internal_error", "could not verify the API key")
			}
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey, principal)))
	})
}

// withTenant resolves the tenant the request acts on: a tenant key's own, or
// the X-Tenant-Id a platform key names.
func (h *Handler) withTenant(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := PrincipalFrom(r.Context())
		id, err := p.ResolveTenant(strings.TrimSpace(r.Header.Get(HeaderTenant)))
		if err != nil {
			switch {
			case errors.Is(err, auth.ErrTenantMismatch):
				respondError(w, http.StatusForbidden, "tenant_mismatch", "this key belongs to a different tenant")
			default:
				respondError(w, http.StatusBadRequest, "tenant_required", "platform keys must name a tenant in the X-Tenant-Id header")
			}
			return
		}
		if p.IsPlatform() {
			t, err := h.svc.Tenant(r.Context(), id)
			if err != nil {
				respondServiceError(w, r, h.log, err)
				return
			}
			if t.Status != "active" {
				respondError(w, http.StatusForbidden, "tenant_suspended", "tenant is suspended")
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), tenantKey, id)))
	})
}

// requireScope rejects a key without the permission.
func requireScope(scope auth.Scope) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !PrincipalFrom(r.Context()).Has(scope) {
				respondError(w, http.StatusForbidden, "insufficient_scope", "this API key needs the "+string(scope)+" scope")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// requirePlatform restricts a route to platform admin keys.
func requirePlatform(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := PrincipalFrom(r.Context())
		if !p.IsPlatform() || !p.Has(auth.ScopeAdmin) {
			respondError(w, http.StatusForbidden, "platform_only", "this route needs a platform admin key")
			return
		}
		next.ServeHTTP(w, r)
	})
}
