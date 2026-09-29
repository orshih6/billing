package billing

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/orshih6/billing/internal/domain"
	"github.com/orshih6/billing/internal/events"
	"github.com/orshih6/billing/internal/models"
)

// EngineReport counts what one engine pass did.
type EngineReport struct {
	PaymentsExpired   int `json:"payments_expired"`
	IncompleteExpired int `json:"incomplete_expired"`
	RenewalInvoices   int `json:"renewal_invoices"`
	Renewed           int `json:"renewed"`
	Canceled          int `json:"canceled"`
	PastDue           int `json:"past_due"`
	Lapsed            int `json:"lapsed"`
	// ProviderCancels is the pass that closes canceled intents at gateways.
	ProviderCancels ProviderCancelReport `json:"provider_cancels"`
	Errors          int                  `json:"errors"`
}

// RunEngine performs every time-driven transition that is due at s.Now().
// Each item runs in its own transaction and re-checks its state under a row
// lock, so it is safe to run on several replicas at once and to run again
// after a crash. Order matters: renewals are raised before period ends are
// processed, so a worker that was down past a period end still bills it.
func (s *Service) RunEngine(ctx context.Context) (EngineReport, error) {
	var r EngineReport
	now := s.now()

	var tenants []models.Tenant
	if err := s.db.WithContext(ctx).Where("status = 'active'").Find(&tenants).Error; err != nil {
		return r, err
	}

	s.each(ctx, &r.Errors, &r.PaymentsExpired,
		s.db.Model(&models.Payment{}).Where("status = ? AND expires_at <= ?", domain.PaymentPending, now),
		func(tx *gorm.DB, tenantID, id uuid.UUID) (bool, error) {
			pay, err := lock[models.Payment](tx, tenantID, id, "payment")
			if err != nil || pay.Status != domain.PaymentPending || pay.ExpiresAt == nil || pay.ExpiresAt.After(now) {
				return false, err
			}
			return true, s.cancelPaymentTx(tx, pay, "expired")
		})

	for _, t := range tenants {
		set := t.EffectiveSettings()
		tid := t.ID

		s.each(ctx, &r.Errors, &r.IncompleteExpired,
			// Expire when the first invoice passes its due date — the deadline
			// the customer was shown (created + incomplete_expiry_hours).
			s.db.Model(&models.Subscription{}).Where("tenant_id = ? AND status = ?", tid, domain.SubIncomplete).
				Where(`EXISTS (SELECT 1 FROM invoices i WHERE i.subscription_id = subscriptions.id
				        AND i.kind = 'subscription_create' AND i.status = 'open' AND i.due_at <= ?)`, now),
			func(tx *gorm.DB, tenantID, id uuid.UUID) (bool, error) {
				return true, s.expireIncompleteTx(tx, tenantID, id, true)
			})

		s.each(ctx, &r.Errors, &r.RenewalInvoices,
			s.db.Model(&models.Subscription{}).
				Where("tenant_id = ? AND status IN ? AND NOT cancel_at_period_end AND current_period_end <= ?",
					tid, []domain.SubscriptionStatus{domain.SubTrialing, domain.SubActive}, now.AddDate(0, 0, set.RenewalLeadDays)).
				Where(`NOT EXISTS (SELECT 1 FROM invoices i WHERE i.subscription_id = subscriptions.id
				        AND i.period_start = subscriptions.current_period_end
				        AND i.kind IN ('subscription_create','subscription_cycle') AND i.status IN ('draft','open','paid'))`),
			func(tx *gorm.DB, tenantID, id uuid.UUID) (bool, error) {
				sub, err := lock[models.Subscription](tx, tenantID, id, "subscription")
				if err != nil || (sub.Status != domain.SubTrialing && sub.Status != domain.SubActive) || sub.CancelAtPeriodEnd {
					return false, err
				}
				if existing, err := s.periodInvoice(tx, sub.ID, sub.CurrentPeriodEnd); err != nil || existing != nil {
					return false, err
				}
				_, err = s.ensureRenewalInvoice(tx, sub)
				return err == nil, err
			})

		s.each(ctx, &r.Errors, nil,
			s.db.Model(&models.Subscription{}).Where("tenant_id = ? AND status IN ? AND current_period_end <= ?",
				tid, []domain.SubscriptionStatus{domain.SubTrialing, domain.SubActive}, now),
			func(tx *gorm.DB, tenantID, id uuid.UUID) (bool, error) {
				sub, err := lock[models.Subscription](tx, tenantID, id, "subscription")
				if err != nil || (sub.Status != domain.SubTrialing && sub.Status != domain.SubActive) || now.Before(sub.CurrentPeriodEnd) {
					return false, err
				}
				if sub.CancelAtPeriodEnd {
					end := sub.CurrentPeriodEnd
					sub.Status = domain.SubCanceled
					sub.EndedAt = &end
					if err := tx.Save(sub).Error; err != nil {
						return false, err
					}
					if err := s.voidOpenSubscriptionInvoices(tx, sub, "subscription canceled at period end"); err != nil {
						return false, err
					}
					r.Canceled++
					return true, events.Emit(tx, tenantID, events.SubscriptionCanceled, sub)
				}
				if _, err := s.ensureRenewalInvoice(tx, sub); err != nil && !errors.Is(err, errAlreadyInvoiced) {
					return false, err
				}
				before := sub.CurrentPeriodEnd
				if err := s.rollIfDue(tx, sub); err != nil {
					return false, err
				}
				if !sub.CurrentPeriodEnd.Equal(before) {
					r.Renewed++
					return true, nil
				}
				sub.Status = domain.SubPastDue
				if err := tx.Save(sub).Error; err != nil {
					return false, err
				}
				r.PastDue++
				return true, events.Emit(tx, tenantID, events.SubscriptionPastDue, sub)
			})

		s.each(ctx, &r.Errors, &r.Lapsed,
			s.db.Model(&models.Subscription{}).Where("tenant_id = ? AND status = ? AND current_period_end <= ?",
				tid, domain.SubPastDue, now.AddDate(0, 0, -set.Grace())),
			func(tx *gorm.DB, tenantID, id uuid.UUID) (bool, error) {
				sub, err := lock[models.Subscription](tx, tenantID, id, "subscription")
				if err != nil || sub.Status != domain.SubPastDue {
					return false, err
				}
				// Paid in the meantime? Then it renews instead of lapsing.
				if err := s.rollIfDue(tx, sub); err != nil || sub.Status != domain.SubPastDue {
					return false, err
				}
				if now.Before(sub.CurrentPeriodEnd.AddDate(0, 0, set.Grace())) {
					return false, nil
				}
				end := now
				sub.Status = domain.SubExpired
				sub.EndedAt = &end
				if err := tx.Save(sub).Error; err != nil {
					return false, err
				}
				if inv, err := s.periodInvoice(tx, sub.ID, sub.CurrentPeriodEnd); err != nil {
					return false, err
				} else if inv != nil && inv.Status == domain.InvoiceOpen {
					if set.LapseAction == "uncollectible" {
						err = s.uncollectibleTx(tx, tenantID, inv.ID)
					} else {
						err = s.voidTx(tx, tenantID, inv.ID, "subscription lapsed unpaid")
					}
					if err != nil {
						return false, err
					}
				}
				return true, events.Emit(tx, tenantID, events.SubscriptionExpired, sub)
			})
	}

	// Last, so cancels queued by this pass (expired payments, voided
	// invoices) reach the gateways straight away.
	pc, err := s.RunProviderCancels(ctx)
	if err != nil {
		s.log.Error("engine: provider cancels", "error", err)
		r.Errors++
	}
	r.ProviderCancels = pc
	return r, nil
}

// each runs fn for every row of q (by id), one transaction per row. Failures
// are logged and counted, not fatal: one bad row must not stall the rest.
func (s *Service) each(ctx context.Context, errs *int, counter *int, q *gorm.DB,
	fn func(tx *gorm.DB, tenantID, id uuid.UUID) (bool, error)) {
	var rows []struct {
		ID       uuid.UUID
		TenantID uuid.UUID
	}
	if err := q.WithContext(ctx).Select("id, tenant_id").Limit(1000).Scan(&rows).Error; err != nil {
		s.log.Error("engine: query", "error", err)
		*errs++
		return
	}
	for _, row := range rows {
		if ctx.Err() != nil {
			return
		}
		var did bool
		err := s.tx(ctx, func(tx *gorm.DB) error {
			var err error
			did, err = fn(tx, row.TenantID, row.ID)
			return err
		})
		if err != nil {
			s.log.Error("engine: item failed", "id", row.ID, "tenant_id", row.TenantID, "error", err)
			*errs++
			continue
		}
		if did && counter != nil {
			*counter++
		}
	}
}
