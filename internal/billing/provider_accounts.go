package billing

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/orshih6/billing/internal/models"
	"github.com/orshih6/billing/internal/providers"
	"github.com/orshih6/billing/internal/secretbox"
)

// ProviderView is one payment provider as a tenant sees it.
type ProviderView struct {
	Name      string            `json:"name"`
	Automatic bool              `json:"automatic"`
	CanCancel bool              `json:"can_cancel"`
	Fields    []providers.Field `json:"fields"`
	// NeedsAccount: the tenant must connect an account before using it.
	NeedsAccount bool                 `json:"needs_account"`
	Ready        bool                 `json:"ready"`
	Account      *ProviderAccountView `json:"account,omitempty"`
}

// ProviderAccountView never contains secrets, only masked hints.
type ProviderAccountView struct {
	ID      uuid.UUID      `json:"id"`
	Mode    string         `json:"mode"`
	Enabled bool           `json:"enabled"`
	Hints   map[string]any `json:"hints"`
	// WebhookPath is where the gateway must send callbacks for this account,
	// relative to the service's public URL.
	WebhookPath string    `json:"webhook_path"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// ProviderAccountInput connects or updates a tenant's account. Secret fields
// left empty keep their stored value, so a form can be re-saved without
// re-entering every secret.
type ProviderAccountInput struct {
	Mode    string            `json:"mode"`
	Enabled *bool             `json:"enabled"`
	Config  map[string]string `json:"config"`
}

func accountView(a *models.ProviderAccount) *ProviderAccountView {
	return &ProviderAccountView{ID: a.ID, Mode: a.Mode, Enabled: a.Enabled, Hints: a.Hints,
		WebhookPath: "/webhooks/" + a.Provider + "/" + a.ID.String(), UpdatedAt: a.UpdatedAt}
}

// PaymentProviders lists every provider with the tenant's setup state.
func (s *Service) PaymentProviders(ctx context.Context, tenantID uuid.UUID) ([]ProviderView, error) {
	var accts []models.ProviderAccount
	if err := s.db.WithContext(ctx).Where("tenant_id = ?", tenantID).Find(&accts).Error; err != nil {
		return nil, err
	}
	byName := map[string]*models.ProviderAccount{}
	for i := range accts {
		byName[accts[i].Provider] = &accts[i]
	}
	names := s.providers.Names()
	sort.Strings(names)
	out := make([]ProviderView, 0, len(names))
	for _, n := range names {
		p, _ := s.providers.Get(n)
		_, canCancel := providers.CanCancel(p)
		v := ProviderView{Name: n, Automatic: p.Automatic(), CanCancel: canCancel, Fields: p.Fields(), NeedsAccount: len(p.Fields()) > 0}
		if v.Fields == nil {
			v.Fields = []providers.Field{}
		}
		if a := byName[n]; a != nil {
			v.Account = accountView(a)
		}
		v.Ready = !v.NeedsAccount || (v.Account != nil && v.Account.Enabled)
		out = append(out, v)
	}
	return out, nil
}

func (s *Service) box() (*secretbox.Box, error) {
	if s.Secrets == nil {
		return nil, &Error{Status: 503, Code: "secrets_key_missing", Message: "the server has no SECRETS_KEY, so provider credentials cannot be stored"}
	}
	return s.Secrets, nil
}

// ConfigureProvider connects or updates the tenant's account for a provider.
func (s *Service) ConfigureProvider(ctx context.Context, tenantID uuid.UUID, name string, in ProviderAccountInput) (*ProviderAccountView, error) {
	prov, ok := s.providers.Get(name)
	if !ok {
		return nil, notFound("provider")
	}
	fields := prov.Fields()
	if len(fields) == 0 {
		return nil, invalid("nothing_to_configure", "provider %s needs no account", name)
	}
	box, err := s.box()
	if err != nil {
		return nil, err
	}
	known := map[string]providers.Field{}
	for _, f := range fields {
		known[f.Key] = f
	}
	for k := range in.Config {
		if _, ok := known[k]; !ok {
			return nil, invalid("unknown_field", "provider %s has no setting %q", name, k)
		}
	}
	if in.Mode == "" {
		in.Mode = "test"
	}
	if in.Mode != "test" && in.Mode != "live" {
		return nil, invalid("invalid_mode", "mode must be test or live")
	}

	var out *ProviderAccountView
	err = s.tx(ctx, func(tx *gorm.DB) error {
		var acct models.ProviderAccount
		err := tx.Where("tenant_id = ? AND provider = ?", tenantID, name).Take(&acct).Error
		isNew := errors.Is(err, gorm.ErrRecordNotFound)
		if err != nil && !isNew {
			return err
		}
		config := map[string]string{}
		if isNew {
			acct = models.ProviderAccount{Base: models.Base{ID: uuid.New()}, TenantID: tenantID, Provider: name, Enabled: true}
		} else if config, err = s.decryptConfig(box, &acct); err != nil {
			return err
		}
		for k, v := range in.Config {
			v = strings.TrimSpace(v)
			if v == "" && known[k].Secret {
				continue // keep the stored secret
			}
			config[k] = v
		}
		for _, f := range fields {
			if f.Required && config[f.Key] == "" {
				return invalid("missing_field", "%s is required", f.Label)
			}
		}
		plain, _ := json.Marshal(config)
		sealed, err := box.Seal(plain, []byte(acct.ID.String()))
		if err != nil {
			return err
		}
		hints := map[string]any{}
		for _, f := range fields {
			v := config[f.Key]
			switch {
			case v == "":
			case f.Secret && len(v) > 4:
				hints[f.Key] = "••••" + v[len(v)-4:]
			case f.Secret:
				hints[f.Key] = "••••"
			default:
				hints[f.Key] = v
			}
		}
		acct.Config, acct.Hints, acct.Mode = sealed, hints, in.Mode
		if in.Enabled != nil {
			acct.Enabled = *in.Enabled
		}
		if err := tx.Save(&acct).Error; err != nil {
			return err
		}
		out = accountView(&acct)
		return nil
	})
	return out, err
}

// DisconnectProvider deletes the tenant's account for a provider. Payments
// already made through it keep their history.
func (s *Service) DisconnectProvider(ctx context.Context, tenantID uuid.UUID, name string) error {
	res := s.db.WithContext(ctx).Where("tenant_id = ? AND provider = ?", tenantID, name).Delete(&models.ProviderAccount{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return notFound("provider_account")
	}
	return nil
}

func (s *Service) decryptConfig(box *secretbox.Box, a *models.ProviderAccount) (map[string]string, error) {
	plain, err := box.Open(a.Config, []byte(a.ID.String()))
	if err != nil {
		return nil, errors.New("billing: provider credentials cannot be decrypted; was SECRETS_KEY changed?")
	}
	cfg := map[string]string{}
	return cfg, json.Unmarshal(plain, &cfg)
}

func (s *Service) toAccount(a *models.ProviderAccount) (*providers.Account, error) {
	box, err := s.box()
	if err != nil {
		return nil, err
	}
	cfg, err := s.decryptConfig(box, a)
	if err != nil {
		return nil, err
	}
	return &providers.Account{ID: a.ID.String(), TenantID: a.TenantID.String(), Mode: a.Mode, Config: cfg}, nil
}

// accountFor returns the tenant's enabled account for a provider that needs
// one, or nil for a provider that does not.
func (s *Service) accountFor(tx *gorm.DB, tenantID uuid.UUID, prov providers.Provider) (*providers.Account, *uuid.UUID, error) {
	if len(prov.Fields()) == 0 {
		return nil, nil, nil
	}
	var a models.ProviderAccount
	if err := tx.Where("tenant_id = ? AND provider = ? AND enabled", tenantID, prov.Name()).Take(&a).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil, invalid("provider_not_configured", "connect your %s account first (Settings → Payment methods)", prov.Name())
		}
		return nil, nil, err
	}
	acct, err := s.toAccount(&a)
	return acct, &a.ID, err
}

// paymentAccount loads the account a payment was made through (nil if none).
func (s *Service) paymentAccount(ctx context.Context, pay *models.Payment) (*providers.Account, error) {
	if pay.ProviderAccountID == nil {
		return nil, nil
	}
	var a models.ProviderAccount
	if err := s.db.WithContext(ctx).Take(&a, "id = ?", *pay.ProviderAccountID).Error; err != nil {
		return nil, err
	}
	return s.toAccount(&a)
}
