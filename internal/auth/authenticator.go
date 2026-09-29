package auth

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/orshih6/billing/internal/models"
)

// Authentication outcomes. They are distinct because the caller already holds
// the token, so saying it expired rather than "wrong" costs nothing.
var (
	ErrNoCredentials = errors.New("auth: no credentials presented")
	ErrInvalidKey    = errors.New("auth: unknown key")
	ErrExpiredKey    = errors.New("auth: key has expired")
	ErrRevokedKey    = errors.New("auth: key has been revoked")
)

const lastUsedInterval = time.Minute

// Principal is the authenticated caller.
type Principal struct {
	// KeyID is the database key id, or uuid.Nil for a bootstrap key.
	KeyID     uuid.UUID
	Name      string
	Bootstrap bool
	Scopes    []Scope
	// KeyTenantID is the tenant the key belongs to; nil for a platform key.
	KeyTenantID *uuid.UUID
}

// Has reports whether the principal may perform an action.
func (p *Principal) Has(scope Scope) bool {
	return p != nil && Satisfies(p.Scopes, scope)
}

// IsPlatform reports whether this is a platform key (no tenant of its own).
func (p *Principal) IsPlatform() bool { return p != nil && p.KeyTenantID == nil }

// ResolveTenant decides which tenant a request acts on. A tenant key always
// acts on its own tenant, whatever the request says; asking for a different
// one is refused rather than silently ignored. A platform key must name the
// tenant (X-Tenant-Id header, or the UI's tenant switcher).
func (p *Principal) ResolveTenant(requested string) (uuid.UUID, error) {
	if p.KeyTenantID != nil {
		if requested != "" && requested != p.KeyTenantID.String() {
			return uuid.Nil, ErrTenantMismatch
		}
		return *p.KeyTenantID, nil
	}
	if requested == "" {
		return uuid.Nil, ErrTenantRequired
	}
	id, err := uuid.Parse(requested)
	if err != nil {
		return uuid.Nil, ErrTenantRequired
	}
	return id, nil
}

var (
	// ErrTenantRequired: a platform key called a tenant route without saying
	// which tenant.
	ErrTenantRequired = errors.New("auth: platform keys must name a tenant with X-Tenant-Id")
	// ErrTenantMismatch: a tenant key asked for another tenant.
	ErrTenantMismatch = errors.New("auth: this key belongs to a different tenant")
)

// Authenticator resolves a presented token to a Principal.
type Authenticator struct {
	db        *gorm.DB
	bootstrap []string
	log       *slog.Logger

	mu        sync.Mutex
	lastTouch map[uuid.UUID]time.Time
}

// NewAuthenticator builds an authenticator. Bootstrap keys come from the
// environment and are platform admins; they solve needing a key to create the
// first key.
func NewAuthenticator(db *gorm.DB, bootstrap []string, log *slog.Logger) *Authenticator {
	return &Authenticator{db: db, bootstrap: bootstrap, log: log, lastTouch: map[uuid.UUID]time.Time{}}
}

// Authenticate verifies a presented token.
func (a *Authenticator) Authenticate(ctx context.Context, presented string) (*Principal, error) {
	if presented == "" {
		return nil, ErrNoCredentials
	}
	for i, key := range a.bootstrap {
		if key != "" && fixedTimeEqual(presented, key) {
			return &Principal{Name: "bootstrap-" + strconv.Itoa(i+1), Bootstrap: true, Scopes: []Scope{ScopeAdmin}}, nil
		}
	}
	if !LooksLikeToken(presented) {
		return nil, ErrInvalidKey
	}

	var key models.APIKey
	err := a.db.WithContext(ctx).Where("hash = ?", Hash(presented)).Take(&key).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrInvalidKey
	}
	if err != nil {
		return nil, err
	}
	if key.RevokedAt != nil {
		return nil, ErrRevokedKey
	}
	if key.Expired() {
		return nil, ErrExpiredKey
	}
	if key.TenantID != nil {
		// A suspended tenant's keys stop working without being revoked.
		var status string
		if err := a.db.WithContext(ctx).Model(&models.Tenant{}).Where("id = ?", *key.TenantID).
			Pluck("status", &status).Error; err != nil {
			return nil, err
		}
		if status != "active" {
			return nil, ErrRevokedKey
		}
	}
	a.touch(ctx, key.ID)

	return &Principal{
		KeyID:       key.ID,
		Name:        key.Name,
		Scopes:      FromStrings(key.Scopes),
		KeyTenantID: key.TenantID,
	}, nil
}

// touch records usage at most once a minute per key, off the request path.
func (a *Authenticator) touch(ctx context.Context, id uuid.UUID) {
	now := time.Now().UTC()
	a.mu.Lock()
	if last, ok := a.lastTouch[id]; ok && now.Sub(last) < lastUsedInterval {
		a.mu.Unlock()
		return
	}
	a.lastTouch[id] = now
	a.mu.Unlock()

	go func() {
		wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := a.db.WithContext(wctx).Model(&models.APIKey{}).Where("id = ?", id).
			Update("last_used_at", now).Error; err != nil {
			a.log.Debug("could not record api key usage", "api_key_id", id, "error", err)
		}
	}()
}
