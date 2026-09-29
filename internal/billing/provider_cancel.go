package billing

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/orshih6/billing/internal/models"
	"github.com/orshih6/billing/internal/providers"
)

const (
	// providerCancelMaxAttempts before a cancel is marked failed.
	providerCancelMaxAttempts = 8
	// providerCancelLease keeps a row away from other workers while one of
	// them is talking to the gateway.
	providerCancelLease = 5 * time.Minute
)

// ProviderCancelReport counts one pass of RunProviderCancels.
type ProviderCancelReport struct {
	Done        int `json:"done"`
	AlreadyPaid int `json:"already_paid"`
	Retrying    int `json:"retrying"`
	Failed      int `json:"failed"`
}

// RunProviderCancels closes, at the gateway, the intents of payments that were
// canceled here (see cancelPaymentTx). It runs outside any transaction because
// it makes network calls:
//
//  1. lease the row (push provider_cancel_next_at forward), so a second
//     worker skips it;
//  2. call the provider's CancelIntent;
//  3. record the outcome — done, already_paid (the money is then recorded via
//     Check → settle), or a retry with backoff (1m, 2m, 4m … capped at 1h)
//     until providerCancelMaxAttempts, then failed.
func (s *Service) RunProviderCancels(ctx context.Context) (ProviderCancelReport, error) {
	var r ProviderCancelReport
	now := s.now()
	var due []models.Payment
	if err := s.db.WithContext(ctx).
		Where("provider_cancel = ? AND provider_cancel_next_at <= ?", models.ProviderCancelPending, now).
		Order("provider_cancel_next_at").Limit(100).Find(&due).Error; err != nil {
		return r, err
	}
	for i := range due {
		if ctx.Err() != nil {
			break
		}
		pay := &due[i]
		leased := s.db.WithContext(ctx).Model(&models.Payment{}).
			Where("id = ? AND provider_cancel = ? AND provider_cancel_next_at <= ?", pay.ID, models.ProviderCancelPending, now).
			Update("provider_cancel_next_at", now.Add(providerCancelLease))
		if leased.Error != nil {
			return r, leased.Error
		}
		if leased.RowsAffected == 0 {
			continue // another worker has it
		}
		switch outcome := s.cancelAtProvider(ctx, pay); outcome {
		case models.ProviderCancelDone:
			r.Done++
		case models.ProviderCancelAlreadyPaid:
			r.AlreadyPaid++
		case models.ProviderCancelFailed:
			r.Failed++
		default:
			r.Retrying++
		}
	}
	return r, nil
}

// cancelAtProvider makes one attempt and records it. It returns the new state
// ("pending" means it will be retried).
func (s *Service) cancelAtProvider(ctx context.Context, pay *models.Payment) string {
	prov, ok := s.providers.Get(pay.Provider)
	var canceler providers.Canceler
	if ok {
		canceler, ok = providers.CanCancel(prov)
	}
	if !ok {
		// The provider was removed or lost the capability since the cancel
		// was queued; nothing can be done.
		s.recordProviderCancel(ctx, pay.ID, models.ProviderCancelFailed, pay.ProviderCancelAttempts, "provider cannot cancel intents")
		return models.ProviderCancelFailed
	}

	attempts := pay.ProviderCancelAttempts + 1
	acct, err := s.paymentAccount(ctx, pay)
	if err == nil {
		err = canceler.CancelIntent(ctx, acct, pay.ProviderRef)
	}
	switch {
	case err == nil:
		s.recordProviderCancel(ctx, pay.ID, models.ProviderCancelDone, attempts, "")
		return models.ProviderCancelDone

	case errors.Is(err, providers.ErrAlreadyPaid):
		// The customer beat the cancel. Ask the provider what was paid and
		// record it; settleTx accepts a canceled payment and puts the money
		// where it belongs (credit, if the invoice was paid another way).
		res, cerr := prov.Check(ctx, acct, pay.ProviderRef)
		if cerr == nil && res.Status == providers.CheckPaid && res.Amount > 0 {
			serr := s.tx(ctx, func(tx *gorm.DB) error { return s.settleTx(tx, pay.TenantID, pay.ID, res.Amount, nil) })
			if serr == nil {
				s.recordProviderCancel(ctx, pay.ID, models.ProviderCancelAlreadyPaid, attempts, "")
				return models.ProviderCancelAlreadyPaid
			}
			err = serr
		} else if cerr != nil {
			err = cerr
		} else {
			err = errors.New("provider says already paid but Check reports " + string(res.Status))
		}
	}

	s.log.Warn("provider cancel failed", "payment_id", pay.ID, "provider", pay.Provider, "attempt", attempts, "error", err)
	if attempts >= providerCancelMaxAttempts {
		s.recordProviderCancel(ctx, pay.ID, models.ProviderCancelFailed, attempts, err.Error())
		return models.ProviderCancelFailed
	}
	backoff := time.Minute << (attempts - 1)
	if backoff > time.Hour {
		backoff = time.Hour
	}
	next := s.now().Add(backoff)
	s.db.WithContext(ctx).Model(&models.Payment{}).Where("id = ? AND provider_cancel = ?", pay.ID, models.ProviderCancelPending).
		Updates(map[string]any{"provider_cancel_attempts": attempts, "provider_cancel_next_at": next, "provider_cancel_error": truncate(err.Error(), 500)})
	return models.ProviderCancelPending
}

func (s *Service) recordProviderCancel(ctx context.Context, id uuid.UUID, state string, attempts int, msg string) {
	updates := map[string]any{
		"provider_cancel": state, "provider_cancel_attempts": attempts,
		"provider_cancel_error": truncate(msg, 500), "provider_cancel_next_at": nil,
	}
	if state == models.ProviderCancelDone {
		updates["provider_canceled_at"] = s.now()
	}
	// Only move a row that is still pending: a late settle may already have
	// marked it already_paid.
	if err := s.db.WithContext(ctx).Model(&models.Payment{}).
		Where("id = ? AND provider_cancel IN ?", id, []string{models.ProviderCancelPending, models.ProviderCancelAlreadyPaid}).
		Updates(updates).Error; err != nil {
		s.log.Error("record provider cancel", "payment_id", id, "error", err)
	}
}

// RetryProviderCancel re-queues a failed gateway cancel now.
func (s *Service) RetryProviderCancel(ctx context.Context, tenantID, paymentID uuid.UUID) (*models.Payment, error) {
	pay, err := s.Payment(ctx, tenantID, paymentID)
	if err != nil {
		return nil, err
	}
	if pay.ProviderCancel != models.ProviderCancelFailed {
		return nil, conflict("provider_cancel_not_failed", "only a failed provider cancel can be retried (state is %q)", pay.ProviderCancel)
	}
	if err := s.db.WithContext(ctx).Model(&models.Payment{}).Where("id = ?", pay.ID).
		Updates(map[string]any{"provider_cancel": models.ProviderCancelPending, "provider_cancel_attempts": 0, "provider_cancel_next_at": s.now()}).Error; err != nil {
		return nil, err
	}
	return s.Payment(ctx, tenantID, paymentID)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
