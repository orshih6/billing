package billing

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"gorm.io/datatypes"

	"github.com/orshih6/billing/internal/events"
	"github.com/orshih6/billing/internal/models"
	"github.com/orshih6/billing/internal/netguard"
)

// WebhookEndpointInput creates or updates an endpoint.
type WebhookEndpointInput struct {
	URL         *string  `json:"url"`
	Description *string  `json:"description"`
	EventTypes  []string `json:"event_types"`
	Enabled     *bool    `json:"enabled"`
}

// CreatedEndpoint carries the signing secret, shown once.
type CreatedEndpoint struct {
	Endpoint *models.WebhookEndpoint `json:"endpoint"`
	Secret   string                  `json:"secret"`
}

func (s *Service) validateEndpoint(ctx context.Context, in WebhookEndpointInput) error {
	if in.URL != nil {
		if err := netguard.CheckURL(ctx, strings.TrimSpace(*in.URL), s.WebhookAllowPrivate); err != nil {
			return invalid("invalid_url", "%v", err)
		}
	}
	known := map[string]bool{"*": true}
	for _, t := range events.Types() {
		known[t] = true
	}
	for _, t := range in.EventTypes {
		if !known[t] {
			return invalid("invalid_event_type", "unknown event type %q", t)
		}
	}
	return nil
}

// CreateWebhookEndpoint registers an endpoint.
func (s *Service) CreateWebhookEndpoint(ctx context.Context, tenantID uuid.UUID, in WebhookEndpointInput) (*CreatedEndpoint, error) {
	if in.URL == nil || strings.TrimSpace(*in.URL) == "" {
		return nil, invalid("invalid_url", "url is required")
	}
	if err := s.validateEndpoint(ctx, in); err != nil {
		return nil, err
	}
	ep := &models.WebhookEndpoint{
		TenantID: tenantID, URL: strings.TrimSpace(*in.URL), Secret: events.NewSecret(),
		EventTypes: datatypes.JSONSlice[string](nonNil(in.EventTypes)), Enabled: true,
	}
	if in.Description != nil {
		ep.Description = *in.Description
	}
	if err := s.db.WithContext(ctx).Create(ep).Error; err != nil {
		return nil, err
	}
	return &CreatedEndpoint{Endpoint: ep, Secret: ep.Secret}, nil
}

// UpdateWebhookEndpoint changes an endpoint.
func (s *Service) UpdateWebhookEndpoint(ctx context.Context, tenantID, id uuid.UUID, in WebhookEndpointInput) (*models.WebhookEndpoint, error) {
	if err := s.validateEndpoint(ctx, in); err != nil {
		return nil, err
	}
	ep, err := take[models.WebhookEndpoint](s.db.WithContext(ctx), tenantID, id, "webhook_endpoint")
	if err != nil {
		return nil, err
	}
	if in.URL != nil {
		ep.URL = strings.TrimSpace(*in.URL)
	}
	if in.Description != nil {
		ep.Description = *in.Description
	}
	if in.EventTypes != nil {
		ep.EventTypes = datatypes.JSONSlice[string](in.EventTypes)
	}
	if in.Enabled != nil {
		ep.Enabled = *in.Enabled
	}
	return ep, s.db.WithContext(ctx).Save(ep).Error
}

// DeleteWebhookEndpoint removes an endpoint; its queued deliveries fail.
func (s *Service) DeleteWebhookEndpoint(ctx context.Context, tenantID, id uuid.UUID) error {
	res := s.db.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenantID, id).Delete(&models.WebhookEndpoint{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return notFound("webhook_endpoint")
	}
	return nil
}

// ListWebhookEndpoints lists endpoints.
func (s *Service) ListWebhookEndpoints(ctx context.Context, tenantID uuid.UUID, p Page) (List[models.WebhookEndpoint], error) {
	return paginate[models.WebhookEndpoint](s.db.WithContext(ctx).Model(&models.WebhookEndpoint{}).Where("tenant_id = ?", tenantID), "webhook_endpoints", p)
}

// ListDeliveries lists deliveries, optionally for one endpoint.
func (s *Service) ListDeliveries(ctx context.Context, tenantID uuid.UUID, endpointID *uuid.UUID, p Page) (List[models.WebhookDelivery], error) {
	q := s.db.WithContext(ctx).Model(&models.WebhookDelivery{}).Where("tenant_id = ?", tenantID)
	if endpointID != nil {
		q = q.Where("endpoint_id = ?", *endpointID)
	}
	return paginate[models.WebhookDelivery](q, "webhook_deliveries", p)
}

// RetryDelivery queues a delivery again now.
func (s *Service) RetryDelivery(ctx context.Context, tenantID, id uuid.UUID) (*models.WebhookDelivery, error) {
	d, err := take[models.WebhookDelivery](s.db.WithContext(ctx), tenantID, id, "webhook_delivery")
	if err != nil {
		return nil, err
	}
	d.Status, d.NextAttemptAt, d.Attempts = "pending", s.now(), 0
	return d, s.db.WithContext(ctx).Save(d).Error
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
