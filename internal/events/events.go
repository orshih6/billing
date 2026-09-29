// Package events is the outbox. Services call Emit inside the same database
// transaction as the state change, so an event exists if and only if the
// change committed. The Dispatcher then delivers each event to the tenant's
// webhook endpoints at least once, signed, with retries.
package events

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/orshih6/billing/internal/models"
	"github.com/orshih6/billing/internal/wire"
)

// Event types. Integrators subscribe to these by name.
const (
	CustomerCreated       = "customer.created"
	CustomerUpdated       = "customer.updated"
	CustomerCreditGranted = "customer.credit_granted"

	SubscriptionCreated   = "subscription.created"
	SubscriptionActivated = "subscription.activated"
	SubscriptionRenewed   = "subscription.renewed"
	SubscriptionUpdated   = "subscription.updated"
	SubscriptionPastDue   = "subscription.past_due"
	SubscriptionCanceled  = "subscription.canceled"
	SubscriptionExpired   = "subscription.expired"

	InvoiceCreated       = "invoice.created"
	InvoiceFinalized     = "invoice.finalized"
	InvoicePaid          = "invoice.paid"
	InvoicePartiallyPaid = "invoice.partially_paid"
	InvoiceVoided        = "invoice.voided"
	InvoiceUncollectible = "invoice.uncollectible"

	PaymentCreated   = "payment.created"
	PaymentSucceeded = "payment.succeeded"
	PaymentFailed    = "payment.failed"
	PaymentCanceled  = "payment.canceled"
	PaymentRefunded  = "payment.refunded"
)

// Types lists every event type.
func Types() []string {
	return []string{
		CustomerCreated, CustomerUpdated, CustomerCreditGranted,
		SubscriptionCreated, SubscriptionActivated, SubscriptionRenewed, SubscriptionUpdated,
		SubscriptionPastDue, SubscriptionCanceled, SubscriptionExpired,
		InvoiceCreated, InvoiceFinalized, InvoicePaid, InvoicePartiallyPaid, InvoiceVoided, InvoiceUncollectible,
		PaymentCreated, PaymentSucceeded, PaymentFailed, PaymentCanceled, PaymentRefunded,
	}
}

// Emit records an event and queues a delivery to every enabled endpoint of the
// tenant that wants it. Call it with the transaction of the state change.
func Emit(tx *gorm.DB, tenantID uuid.UUID, eventType string, object any) error {
	data, err := json.Marshal(wire.Of(object))
	if err != nil {
		return fmt.Errorf("events: marshal %s: %w", eventType, err)
	}
	ev := models.Event{TenantID: tenantID, Type: eventType, Data: data}
	if err := tx.Create(&ev).Error; err != nil {
		return fmt.Errorf("events: insert: %w", err)
	}
	var endpoints []models.WebhookEndpoint
	if err := tx.Where("tenant_id = ? AND enabled", tenantID).Find(&endpoints).Error; err != nil {
		return fmt.Errorf("events: endpoints: %w", err)
	}
	for i := range endpoints {
		if !endpoints[i].Wants(eventType) {
			continue
		}
		d := models.WebhookDelivery{
			TenantID: tenantID, EventID: ev.ID, EndpointID: endpoints[i].ID,
			Status: "pending", NextAttemptAt: time.Now().UTC(),
		}
		if err := tx.Create(&d).Error; err != nil {
			return fmt.Errorf("events: queue delivery: %w", err)
		}
	}
	return nil
}

// NewSecret returns a webhook signing secret.
func NewSecret() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return "whsec_" + hex.EncodeToString(b)
}

// Sign computes the signature header value for a body:
//
//	X-Billing-Signature: t=<unix seconds>,v1=<hex HMAC-SHA256(secret, "<t>.<body>")>
//
// Receivers recompute it and reject stale timestamps to stop replays.
func Sign(secret string, ts time.Time, body []byte) string {
	t := strconv.FormatInt(ts.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(t + "."))
	mac.Write(body)
	return "t=" + t + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

// Envelope is the body POSTed to an endpoint.
type Envelope struct {
	ID        uuid.UUID       `json:"id"`
	Type      string          `json:"type"`
	TenantID  uuid.UUID       `json:"tenant_id"`
	CreatedAt time.Time       `json:"created_at"`
	Data      json.RawMessage `json:"data"`
}

// Dispatcher delivers queued webhook deliveries.
type Dispatcher struct {
	DB          *gorm.DB
	Client      *http.Client
	MaxAttempts int
	UserAgent   string
	Log         *slog.Logger
	Batch       int
}

// RunOnce claims due deliveries and attempts them. Several workers can run at
// once: rows are claimed with FOR UPDATE SKIP LOCKED and pushed forward before
// the HTTP call, so a crash mid-call only delays a retry.
func (d *Dispatcher) RunOnce(ctx context.Context) (int, error) {
	batch := d.Batch
	if batch <= 0 {
		batch = 50
	}
	var claimed []models.WebhookDelivery
	err := d.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where("status = 'pending' AND next_attempt_at <= ?", time.Now().UTC()).
			Order("next_attempt_at").Limit(batch).Find(&claimed).Error; err != nil {
			return err
		}
		for _, c := range claimed {
			// Lease: if this worker dies, the row becomes due again later.
			if err := tx.Model(&models.WebhookDelivery{}).Where("id = ?", c.ID).
				Update("next_attempt_at", time.Now().UTC().Add(5*time.Minute)).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	for i := range claimed {
		d.attempt(ctx, &claimed[i])
	}
	return len(claimed), nil
}

func (d *Dispatcher) attempt(ctx context.Context, del *models.WebhookDelivery) {
	del.Attempts++
	var (
		ev models.Event
		ep models.WebhookEndpoint
	)
	if err := d.DB.WithContext(ctx).Take(&ev, "id = ?", del.EventID).Error; err != nil {
		d.Log.Error("webhook: load event", "delivery", del.ID, "error", err)
		return
	}
	if err := d.DB.WithContext(ctx).Take(&ep, "id = ?", del.EndpointID).Error; err != nil || !ep.Enabled {
		d.finish(ctx, del, "failed", 0, "endpoint deleted or disabled")
		return
	}

	body, _ := json.Marshal(Envelope{ID: ev.ID, Type: ev.Type, TenantID: ev.TenantID, CreatedAt: ev.CreatedAt, Data: json.RawMessage(ev.Data)})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.URL, bytes.NewReader(body))
	if err != nil {
		d.finish(ctx, del, "failed", 0, "invalid endpoint URL: "+err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", d.UserAgent)
	req.Header.Set("X-Billing-Event", ev.Type)
	req.Header.Set("X-Billing-Event-Id", ev.ID.String())
	req.Header.Set("X-Billing-Delivery-Id", del.ID.String())
	req.Header.Set("X-Billing-Signature", Sign(ep.Secret, time.Now(), body))

	resp, err := d.Client.Do(req)
	status := 0
	msg := ""
	if err != nil {
		msg = err.Error()
	} else {
		status = resp.StatusCode
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		resp.Body.Close()
		if status >= 200 && status < 300 {
			d.finish(ctx, del, "succeeded", status, "")
			return
		}
		msg = fmt.Sprintf("HTTP %d: %s", status, snippet)
	}

	if del.Attempts >= d.MaxAttempts {
		d.finish(ctx, del, "failed", status, msg)
		return
	}
	// Exponential backoff: 30s, 1m, 2m, 4m … capped at 6h.
	backoff := 30 * time.Second << (del.Attempts - 1)
	if backoff > 6*time.Hour || backoff <= 0 {
		backoff = 6 * time.Hour
	}
	d.DB.WithContext(ctx).Model(&models.WebhookDelivery{}).Where("id = ?", del.ID).Updates(map[string]any{
		"attempts": del.Attempts, "next_attempt_at": time.Now().UTC().Add(backoff),
		"last_status_code": status, "last_error": truncate(msg, 1000),
	})
}

func (d *Dispatcher) finish(ctx context.Context, del *models.WebhookDelivery, status string, code int, msg string) {
	updates := map[string]any{
		"status": status, "attempts": del.Attempts, "last_status_code": code, "last_error": truncate(msg, 1000),
	}
	if status == "succeeded" {
		updates["delivered_at"] = time.Now().UTC()
	}
	if err := d.DB.WithContext(ctx).Model(&models.WebhookDelivery{}).Where("id = ?", del.ID).Updates(updates).Error; err != nil {
		d.Log.Error("webhook: record result", "delivery", del.ID, "error", err)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
