package billing

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"github.com/orshih6/billing/internal/models"
	"github.com/orshih6/billing/internal/wire"
)

// SearchIn says which resources a search may look at (the caller's scopes).
type SearchIn struct {
	Customers, Invoices, Payments, Subscriptions bool
}

// SearchResult groups matches by resource, at most 10 each, newest first.
type SearchResult struct {
	Query         string             `json:"query"`
	Customers     []models.Customer  `json:"customers,omitempty"`
	Invoices      []models.Invoice   `json:"invoices,omitempty"`
	Payments      []models.Payment   `json:"payments,omitempty"`
	Subscriptions []SubscriptionView `json:"subscriptions,omitempty"`
}

// Search finds anything matching q: a pasted id matches that exact object;
// otherwise customers by name/email/phone/external id, invoices by number or
// payment reference, payments by provider reference, subscriptions by plan code
// or their customer.
func (s *Service) Search(ctx context.Context, tenantID uuid.UUID, q string, in SearchIn) (*SearchResult, error) {
	q = strings.TrimSpace(q)
	out := &SearchResult{Query: q}
	if len(q) < 2 {
		return out, invalid("query_too_short", "search for at least 2 characters")
	}
	db := s.db.WithContext(ctx)
	const limit = 10
	id, idErr := uuid.Parse(q)
	like := likePattern(q)

	if in.Customers {
		cq := db.Where("tenant_id = ?", tenantID)
		if idErr == nil {
			cq = cq.Where("id = ?", id)
		} else {
			cq = cq.Where("name ILIKE ? OR email ILIKE ? OR phone ILIKE ? OR external_id ILIKE ?", like, like, like, like)
		}
		if err := cq.Order("created_at DESC").Limit(limit).Find(&out.Customers).Error; err != nil {
			return nil, err
		}
	}
	if in.Invoices {
		iq := db.Preload("Lines").Where("tenant_id = ?", tenantID)
		if idErr == nil {
			iq = iq.Where("id = ? OR customer_id = ? OR subscription_id = ?", id, id, id)
		} else {
			iq = iq.Where("number ILIKE ? OR reference ILIKE ?", like, like)
		}
		if err := iq.Order("created_at DESC").Limit(limit).Find(&out.Invoices).Error; err != nil {
			return nil, err
		}
	}
	if in.Payments {
		pq := db.Where("tenant_id = ?", tenantID)
		if idErr == nil {
			pq = pq.Where("id = ? OR invoice_id = ? OR customer_id = ?", id, id, id)
		} else {
			pq = pq.Where("provider_ref ILIKE ? OR note ILIKE ?", like, like)
		}
		if err := pq.Order("created_at DESC").Limit(limit).Find(&out.Payments).Error; err != nil {
			return nil, err
		}
	}
	if in.Subscriptions {
		var subs []models.Subscription
		sq := db.Where("tenant_id = ?", tenantID)
		if idErr == nil {
			sq = sq.Where("id = ? OR customer_id = ?", id, id)
		} else {
			sq = sq.Where(`plan_id IN (SELECT id FROM plans WHERE tenant_id = ? AND (code ILIKE ? OR name ILIKE ?))
			    OR customer_id IN (SELECT id FROM customers WHERE tenant_id = ? AND (name ILIKE ? OR email ILIKE ? OR external_id ILIKE ?))`,
				tenantID, like, like, tenantID, like, like, like)
		}
		if err := sq.Order("created_at DESC").Limit(limit).Find(&subs).Error; err != nil {
			return nil, err
		}
		for _, sub := range subs {
			out.Subscriptions = append(out.Subscriptions, s.view(ctx, sub))
		}
	}
	return out, nil
}

// Wire gives the search result its public shape.
func (r *SearchResult) Wire() any {
	return map[string]any{
		"object":        "search_result",
		"query":         r.Query,
		"customers":     wire.ListOf(r.Customers, false).Data,
		"invoices":      wire.ListOf(r.Invoices, false).Data,
		"payments":      wire.ListOf(r.Payments, false).Data,
		"subscriptions": wire.ListOf(r.Subscriptions, false).Data,
	}
}
