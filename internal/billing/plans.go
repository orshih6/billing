package billing

import (
	"context"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	"github.com/orshih6/billing/internal/database"
	"github.com/orshih6/billing/internal/domain"
	"github.com/orshih6/billing/internal/models"
)

var codeRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)

// PriceInput adds a price to a plan.
type PriceInput struct {
	Nickname      string `json:"nickname"`
	Amount        int64  `json:"amount"`
	Currency      string `json:"currency"`
	Interval      string `json:"interval"`
	IntervalCount int    `json:"interval_count"`
	TrialDays     int    `json:"trial_days"`
}

// PlanInput creates a plan, optionally with its first prices.
type PlanInput struct {
	Code        string `json:"code"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// Kind is "base" (default) or "addon".
	Kind     string         `json:"kind"`
	Metadata map[string]any `json:"metadata"`
	Prices   []PriceInput   `json:"prices"`
}

// CreatePlan adds a plan.
func (s *Service) CreatePlan(ctx context.Context, tenantID uuid.UUID, in PlanInput) (*models.Plan, error) {
	in.Code = strings.ToLower(strings.TrimSpace(in.Code))
	if !codeRE.MatchString(in.Code) {
		return nil, invalid("invalid_code", "code must be lowercase letters, digits, '.', '_' or '-'")
	}
	if strings.TrimSpace(in.Name) == "" {
		return nil, invalid("invalid_name", "name is required")
	}
	switch in.Kind {
	case "":
		in.Kind = models.PlanBase
	case models.PlanBase, models.PlanAddon:
	default:
		return nil, invalid("invalid_kind", "kind must be base or addon")
	}
	t, err := s.Tenant(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	p := &models.Plan{
		TenantID: tenantID, Code: in.Code, Name: strings.TrimSpace(in.Name), Kind: in.Kind,
		Description: in.Description, Active: true, Metadata: in.Metadata,
	}
	err = s.tx(ctx, func(tx *gorm.DB) error {
		if err := tx.Omit("Prices").Create(p).Error; err != nil {
			if database.IsUniqueViolation(err) {
				return conflict("code_taken", "a plan with code %q already exists", in.Code)
			}
			return err
		}
		for _, pi := range in.Prices {
			price, err := newPrice(t, p, pi)
			if err != nil {
				return err
			}
			if err := tx.Create(price).Error; err != nil {
				return err
			}
			p.Prices = append(p.Prices, *price)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return p, nil
}

func newPrice(t *models.Tenant, p *models.Plan, in PriceInput) (*models.Price, error) {
	if in.Currency == "" {
		in.Currency = t.DefaultCurrency
	}
	cur, err := domain.LookupCurrency(in.Currency)
	if err != nil {
		return nil, invalid("invalid_currency", "%v", err)
	}
	iv, err := domain.ParseInterval(in.Interval)
	if err != nil {
		return nil, invalid("invalid_interval", "%v", err)
	}
	if in.Amount < 0 {
		return nil, invalid("invalid_amount", "amount cannot be negative")
	}
	if in.IntervalCount <= 0 {
		in.IntervalCount = 1
	}
	if in.TrialDays < 0 || (in.TrialDays > 0 && !iv.Recurring()) {
		return nil, invalid("invalid_trial", "trial_days must be >= 0 and only on recurring prices")
	}
	return &models.Price{
		TenantID: t.ID, PlanID: p.ID, Nickname: in.Nickname, Amount: in.Amount, Currency: cur.Code,
		Interval: iv, IntervalCount: in.IntervalCount, TrialDays: in.TrialDays, Active: true,
	}, nil
}

// AddPrice adds a new price version to a plan. Existing subscriptions keep
// their price; move them with ChangePrice.
func (s *Service) AddPrice(ctx context.Context, tenantID, planID uuid.UUID, in PriceInput) (*models.Price, error) {
	t, err := s.Tenant(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	p, err := take[models.Plan](s.db.WithContext(ctx), tenantID, planID, "plan")
	if err != nil {
		return nil, err
	}
	price, err := newPrice(t, p, in)
	if err != nil {
		return nil, err
	}
	return price, s.db.WithContext(ctx).Create(price).Error
}

// SetPriceActive retires or re-enables a price. Inactive prices cannot start
// new subscriptions; existing ones keep renewing on them.
func (s *Service) SetPriceActive(ctx context.Context, tenantID, priceID uuid.UUID, active bool) (*models.Price, error) {
	pr, err := take[models.Price](s.db.WithContext(ctx), tenantID, priceID, "price")
	if err != nil {
		return nil, err
	}
	pr.Active = active
	return pr, s.db.WithContext(ctx).Model(pr).Update("active", active).Error
}

// UpdatePlanInput changes a plan. Nil fields are left alone.
type UpdatePlanInput struct {
	Name        *string        `json:"name"`
	Description *string        `json:"description"`
	Active      *bool          `json:"active"`
	Metadata    map[string]any `json:"metadata"`
}

// UpdatePlan changes descriptive fields.
func (s *Service) UpdatePlan(ctx context.Context, tenantID, planID uuid.UUID, in UpdatePlanInput) (*models.Plan, error) {
	p, err := take[models.Plan](s.db.WithContext(ctx), tenantID, planID, "plan")
	if err != nil {
		return nil, err
	}
	if in.Name != nil && strings.TrimSpace(*in.Name) != "" {
		p.Name = strings.TrimSpace(*in.Name)
	}
	if in.Description != nil {
		p.Description = *in.Description
	}
	if in.Active != nil {
		p.Active = *in.Active
	}
	if in.Metadata != nil {
		p.Metadata = datatypes.JSONMap(in.Metadata)
	}
	if err := s.db.WithContext(ctx).Omit("Prices").Save(p).Error; err != nil {
		return nil, err
	}
	return s.Plan(ctx, tenantID, planID)
}

// Plan loads a plan with its prices.
func (s *Service) Plan(ctx context.Context, tenantID, id uuid.UUID) (*models.Plan, error) {
	return take[models.Plan](s.db.WithContext(ctx).Preload("Prices", func(db *gorm.DB) *gorm.DB {
		return db.Order("created_at")
	}), tenantID, id, "plan")
}

// PlanByCode loads a plan by its code.
func (s *Service) PlanByCode(ctx context.Context, tenantID uuid.UUID, code string) (*models.Plan, error) {
	var p models.Plan
	if err := s.db.WithContext(ctx).Where("tenant_id = ? AND code = ?", tenantID, code).Take(&p).Error; err != nil {
		return nil, notFound("plan")
	}
	return s.Plan(ctx, tenantID, p.ID)
}

// ListPlans lists plans with their prices.
func (s *Service) ListPlans(ctx context.Context, tenantID uuid.UUID, activeOnly bool, p Page) (List[models.Plan], error) {
	q := s.db.WithContext(ctx).Model(&models.Plan{}).Where("tenant_id = ?", tenantID).
		Preload("Prices", func(db *gorm.DB) *gorm.DB { return db.Order("created_at") })
	if activeOnly {
		q = q.Where("active")
	}
	return paginate[models.Plan](q, "plans", p)
}

// Price loads one price.
func (s *Service) Price(ctx context.Context, tenantID, id uuid.UUID) (*models.Price, error) {
	return take[models.Price](s.db.WithContext(ctx), tenantID, id, "price")
}
