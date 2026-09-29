package billing

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	"github.com/orshih6/billing/internal/auth"
	"github.com/orshih6/billing/internal/database"
	"github.com/orshih6/billing/internal/domain"
	"github.com/orshih6/billing/internal/models"
)

var slugRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)

// CreateTenantInput creates a tenant.
type CreateTenantInput struct {
	Slug            string                 `json:"slug"`
	Name            string                 `json:"name"`
	DefaultCurrency string                 `json:"default_currency"`
	Settings        *models.TenantSettings `json:"settings"`
	// CreateAdminKey also mints an admin key for the new tenant and returns
	// its token once, so onboarding is one call.
	CreateAdminKey bool `json:"create_admin_key"`
}

// CreatedTenant is the result of CreateTenant.
type CreatedTenant struct {
	Tenant *models.Tenant `json:"tenant"`
	APIKey *CreatedKey    `json:"api_key,omitempty"`
}

// CreateTenant registers an integrating system.
func (s *Service) CreateTenant(ctx context.Context, in CreateTenantInput) (*CreatedTenant, error) {
	in.Slug = strings.ToLower(strings.TrimSpace(in.Slug))
	if !slugRE.MatchString(in.Slug) {
		return nil, invalid("invalid_slug", "slug must be 2-63 lowercase letters, digits or dashes")
	}
	if strings.TrimSpace(in.Name) == "" {
		return nil, invalid("invalid_name", "name is required")
	}
	if in.DefaultCurrency == "" {
		in.DefaultCurrency = domain.DefaultCurrency
	}
	cur, err := domain.LookupCurrency(in.DefaultCurrency)
	if err != nil {
		return nil, invalid("invalid_currency", "%v", err)
	}
	settings := models.TenantSettings{}
	if in.Settings != nil {
		settings = *in.Settings
	}
	t := &models.Tenant{
		Slug: in.Slug, Name: strings.TrimSpace(in.Name), DefaultCurrency: cur.Code,
		Settings: datatypes.NewJSONType(settings), Status: "active",
	}
	out := &CreatedTenant{Tenant: t}
	err = s.tx(ctx, func(tx *gorm.DB) error {
		if err := tx.Create(t).Error; err != nil {
			if database.IsUniqueViolation(err) {
				return conflict("slug_taken", "a tenant with slug %q already exists", in.Slug)
			}
			return err
		}
		if in.CreateAdminKey {
			k, err := createKey(tx, &t.ID, "admin", []auth.Scope{auth.ScopeAdmin}, nil)
			if err != nil {
				return err
			}
			out.APIKey = k
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListTenants lists every tenant (platform keys only).
func (s *Service) ListTenants(ctx context.Context, p Page) (List[models.Tenant], error) {
	return paginate[models.Tenant](s.db.WithContext(ctx).Model(&models.Tenant{}), "tenants", p)
}

// UpdateTenantInput changes a tenant. Nil fields are left alone.
type UpdateTenantInput struct {
	Name            *string                `json:"name"`
	DefaultCurrency *string                `json:"default_currency"`
	Settings        *models.TenantSettings `json:"settings"`
	// Status is platform-only: "active" or "suspended". A suspended tenant's
	// keys stop working.
	Status *string `json:"status"`
}

// UpdateTenant applies changes.
func (s *Service) UpdateTenant(ctx context.Context, id uuid.UUID, in UpdateTenantInput) (*models.Tenant, error) {
	var out *models.Tenant
	err := s.tx(ctx, func(tx *gorm.DB) error {
		var t models.Tenant
		if err := tx.Take(&t, "id = ?", id).Error; err != nil {
			return notFound("tenant")
		}
		if in.Name != nil && strings.TrimSpace(*in.Name) != "" {
			t.Name = strings.TrimSpace(*in.Name)
		}
		if in.DefaultCurrency != nil {
			c, err := domain.LookupCurrency(*in.DefaultCurrency)
			if err != nil {
				return invalid("invalid_currency", "%v", err)
			}
			t.DefaultCurrency = c.Code
		}
		if in.Settings != nil {
			if la := in.Settings.LapseAction; la != "" && la != "void" && la != "uncollectible" {
				return invalid("invalid_settings", "lapse_action must be void or uncollectible")
			}
			if tax := in.Settings.Tax; tax != nil && (tax.RateBps < 0 || tax.RateBps > 10000) {
				return invalid("invalid_settings", "tax.rate_bps must be 0–10000 (basis points: 1000 = 10%%)")
			}
			t.Settings = datatypes.NewJSONType(*in.Settings)
		}
		if in.Status != nil {
			if *in.Status != "active" && *in.Status != "suspended" {
				return invalid("invalid_status", "status must be active or suspended")
			}
			t.Status = *in.Status
		}
		if err := tx.Save(&t).Error; err != nil {
			return err
		}
		out = &t
		return nil
	})
	return out, err
}

// CreatedKey is a new key; Token is shown only here.
type CreatedKey struct {
	Key   *models.APIKey `json:"key"`
	Token string         `json:"token"`
}

// CreateKeyInput mints a key.
type CreateKeyInput struct {
	Name      string   `json:"name"`
	Scopes    []string `json:"scopes"`
	ExpiresIn string   `json:"expires_in"` // Go duration or "" for never, e.g. "2160h"
}

// CreateKey mints a key inside tenantID (nil = platform key). The creator can
// never grant a scope it does not itself hold.
func (s *Service) CreateKey(ctx context.Context, creator *auth.Principal, tenantID *uuid.UUID, in CreateKeyInput) (*CreatedKey, error) {
	if strings.TrimSpace(in.Name) == "" {
		return nil, invalid("invalid_name", "name is required")
	}
	scopes, err := auth.ParseAll(in.Scopes)
	if err != nil {
		return nil, invalid("invalid_scopes", "%v", err)
	}
	if creator != nil {
		for _, sc := range scopes {
			if !creator.Has(sc) {
				return nil, forbidden("scope_escalation", "you cannot grant %s: your key does not hold it", sc)
			}
		}
		if tenantID == nil && !creator.IsPlatform() {
			return nil, forbidden("platform_only", "only platform keys can mint platform keys")
		}
	}
	var expires *time.Time
	if in.ExpiresIn != "" && in.ExpiresIn != "never" {
		d, err := time.ParseDuration(in.ExpiresIn)
		if err != nil || d <= 0 {
			return nil, invalid("invalid_expires_in", "expires_in must be a positive duration like 720h, or never")
		}
		expires = ptr(s.now().Add(d))
	}
	var out *CreatedKey
	err = s.tx(ctx, func(tx *gorm.DB) error {
		if tenantID != nil {
			if _, _, err := s.settings(tx, *tenantID); err != nil {
				return err
			}
		}
		out, err = createKey(tx, tenantID, strings.TrimSpace(in.Name), scopes, expires)
		return err
	})
	return out, err
}

func createKey(tx *gorm.DB, tenantID *uuid.UUID, name string, scopes []auth.Scope, expires *time.Time) (*CreatedKey, error) {
	g, err := auth.Generate()
	if err != nil {
		return nil, err
	}
	k := &models.APIKey{
		TenantID: tenantID, Name: name, Prefix: g.Prefix, LastFour: g.LastFour, Hash: g.Hash,
		Scopes: datatypes.JSONSlice[string](auth.Strings(scopes)), ExpiresAt: expires,
	}
	if err := tx.Create(k).Error; err != nil {
		return nil, err
	}
	return &CreatedKey{Key: k, Token: g.Token}, nil
}

// ListKeys lists a tenant's keys (nil tenant: platform keys).
func (s *Service) ListKeys(ctx context.Context, tenantID *uuid.UUID, p Page) (List[models.APIKey], error) {
	q := s.db.WithContext(ctx).Model(&models.APIKey{})
	if tenantID == nil {
		q = q.Where("tenant_id IS NULL")
	} else {
		q = q.Where("tenant_id = ?", *tenantID)
	}
	return paginate[models.APIKey](q, "api_keys", p)
}

// RevokeKey revokes a key that belongs to tenantID.
func (s *Service) RevokeKey(ctx context.Context, tenantID *uuid.UUID, keyID uuid.UUID) (*models.APIKey, error) {
	var k models.APIKey
	q := s.db.WithContext(ctx).Where("id = ?", keyID)
	if tenantID == nil {
		q = q.Where("tenant_id IS NULL")
	} else {
		q = q.Where("tenant_id = ?", *tenantID)
	}
	if err := q.Take(&k).Error; err != nil {
		return nil, notFound("api_key")
	}
	if k.RevokedAt == nil {
		k.RevokedAt = ptr(s.now())
		if err := s.db.WithContext(ctx).Model(&k).Update("revoked_at", k.RevokedAt).Error; err != nil {
			return nil, err
		}
	}
	return &k, nil
}

// HasUsableKey reports whether any unrevoked platform key exists; production
// refuses to start without one (or a bootstrap key).
func (s *Service) HasUsableKey(ctx context.Context) (bool, error) {
	var n int64
	err := s.db.WithContext(ctx).Model(&models.APIKey{}).
		Where("tenant_id IS NULL AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?)", s.now()).
		Count(&n).Error
	return n > 0, err
}
