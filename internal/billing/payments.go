package billing

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/orshih6/billing/internal/database"
	"github.com/orshih6/billing/internal/domain"
	"github.com/orshih6/billing/internal/events"
	"github.com/orshih6/billing/internal/ledger"
	"github.com/orshih6/billing/internal/models"
	"github.com/orshih6/billing/internal/providers"
)

// StartPaymentInput starts collecting an invoice through a provider.
type StartPaymentInput struct {
	Provider  string `json:"provider"`
	ReturnURL string `json:"return_url"`
}

// StartPayment creates a pending payment for what is still due on an invoice
// and returns the provider's instructions (a checkout URL, bank details).
// Asking again while an unexpired pending payment for the same provider and
// amount exists returns that one instead of creating another.
//
// The gateway is called between two transactions, never inside one: a slow
// provider must not hold the invoice lock. If the invoice changed while the
// intent was being created, the new intent is closed again and the call fails
// with a conflict the caller can retry.
func (s *Service) StartPayment(ctx context.Context, tenantID, invoiceID uuid.UUID, in StartPaymentInput, keyID *uuid.UUID) (*models.Payment, error) {
	prov, ok := s.providers.Get(in.Provider)
	if !ok {
		return nil, invalid("unknown_provider", "unknown provider %q (available: %s)", in.Provider, strings.Join(s.providers.Names(), ", "))
	}
	var (
		existing *models.Payment
		inv      *models.Invoice
		set      models.TenantSettings
		acct     *providers.Account
		acctID   *uuid.UUID
	)
	// 1. Validate and look for a reusable intent.
	err := s.tx(ctx, func(tx *gorm.DB) error {
		var err error
		if _, set, err = s.settings(tx, tenantID); err != nil {
			return err
		}
		if prov.Name() == "mock" && s.Production && !set.MockEnabled {
			return forbidden("mock_disabled", "the mock provider is disabled in production for this tenant")
		}
		if inv, err = take[models.Invoice](tx, tenantID, invoiceID, "invoice"); err != nil {
			return err
		}
		if inv.Status != domain.InvoiceOpen || inv.AmountDue <= 0 {
			return conflict("invoice_not_payable", "invoice is %s with %d due", inv.Status, inv.AmountDue)
		}
		var p models.Payment
		err = tx.Where("invoice_id = ? AND provider = ? AND status = ? AND amount = ? AND (expires_at IS NULL OR expires_at > ?)",
			inv.ID, prov.Name(), domain.PaymentPending, inv.AmountDue, s.now()).
			Order("created_at DESC").Take(&p).Error
		if err == nil {
			existing = &p
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		acct, acctID, err = s.accountFor(tx, tenantID, prov)
		return err
	})
	if err != nil || existing != nil {
		return existing, err
	}

	// 2. Talk to the gateway with no transaction open.
	id := uuid.New()
	expires := s.now().Add(time.Duration(set.PaymentExpiryHours) * time.Hour)
	intent, err := prov.CreateIntent(ctx, acct, providers.IntentInput{
		PaymentID: id.String(), InvoiceNumber: deref(inv.Number), Reference: deref(inv.Reference),
		Amount: inv.AmountDue, Currency: inv.Currency, Description: "Invoice " + deref(inv.Number),
		ReturnURL: in.ReturnURL, ExpiresAt: expires, Instructions: set.ManualInstructions,
	})
	if err != nil {
		return nil, &Error{http.StatusBadGateway, "provider_error", err.Error()}
	}
	if !intent.ExpiresAt.IsZero() {
		expires = intent.ExpiresAt
	}

	// 3. Record it, provided the invoice still owes the same amount.
	pay := &models.Payment{
		Base:     models.Base{ID: id},
		TenantID: tenantID, InvoiceID: inv.ID, CustomerID: inv.CustomerID,
		Provider: prov.Name(), ProviderAccountID: acctID, ProviderRef: intent.ProviderRef, Status: domain.PaymentPending,
		Amount: inv.AmountDue, Currency: inv.Currency, Instructions: intent.Instructions,
		PayURL: intent.PayURL, ReturnURL: in.ReturnURL, ExpiresAt: &expires,
		CreatedByKeyID: keyID, Raw: intent.Raw,
	}
	err = s.tx(ctx, func(tx *gorm.DB) error {
		cur, err := lock[models.Invoice](tx, tenantID, invoiceID, "invoice")
		if err != nil {
			return err
		}
		if cur.Status != domain.InvoiceOpen || cur.AmountDue != inv.AmountDue {
			return conflict("invoice_changed", "the invoice changed while the payment was being created; try again")
		}
		if err := tx.Create(pay).Error; err != nil {
			return err
		}
		return events.Emit(tx, tenantID, events.PaymentCreated, pay)
	})
	if err != nil {
		if c, ok := providers.CanCancel(prov); ok {
			if cerr := c.CancelIntent(context.WithoutCancel(ctx), acct, intent.ProviderRef); cerr != nil {
				s.log.Warn("could not close an unused intent", "provider", prov.Name(), "ref", intent.ProviderRef, "error", cerr)
			}
		}
		return nil, err
	}
	return pay, nil
}

// ManualPaymentInput records money that arrived outside any provider.
type ManualPaymentInput struct {
	InvoiceID uuid.UUID `json:"invoice_id"`
	// Amount actually received, in minor units. It may differ from what is
	// due: less leaves the invoice open, more goes to the customer's credit.
	Amount int64 `json:"amount"`
	// Reference is the bank statement id / receipt number. It is unique per
	// tenant, so the same transfer cannot be recorded twice.
	Reference  string     `json:"reference"`
	Note       string     `json:"note"`
	ReceivedAt *time.Time `json:"received_at"`
}

// RecordManualPayment records and settles a manual payment in one step.
func (s *Service) RecordManualPayment(ctx context.Context, tenantID uuid.UUID, in ManualPaymentInput, keyID *uuid.UUID) (*models.Payment, error) {
	if in.Amount <= 0 {
		return nil, invalid("invalid_amount", "amount must be positive (minor units)")
	}
	var id uuid.UUID
	err := s.tx(ctx, func(tx *gorm.DB) error {
		inv, err := lock[models.Invoice](tx, tenantID, in.InvoiceID, "invoice")
		if err != nil {
			return err
		}
		if inv.Status == domain.InvoiceDraft {
			return conflict("invoice_not_finalized", "finalize the invoice before recording payments")
		}
		id = uuid.New()
		ref := "manual_" + id.String()
		if r := strings.TrimSpace(in.Reference); r != "" {
			ref = "manual:" + r
			var n int64
			if err := tx.Model(&models.Payment{}).Where("tenant_id = ? AND provider = 'manual' AND provider_ref = ?", tenantID, ref).
				Count(&n).Error; err != nil {
				return err
			}
			if n > 0 {
				return conflict("reference_already_recorded", "a payment with reference %q was already recorded", r)
			}
		}
		pay := &models.Payment{
			Base:     models.Base{ID: id},
			TenantID: tenantID, InvoiceID: inv.ID, CustomerID: inv.CustomerID,
			Provider: "manual", ProviderRef: ref, Status: domain.PaymentPending,
			Amount: in.Amount, Currency: inv.Currency, Note: in.Note, CreatedByKeyID: keyID,
		}
		if err := tx.Create(pay).Error; err != nil {
			if database.IsUniqueViolation(err) {
				return conflict("reference_already_recorded", "a payment with that reference was already recorded")
			}
			return err
		}
		return s.settleTx(tx, tenantID, pay.ID, in.Amount, in.ReceivedAt)
	})
	if err != nil {
		return nil, err
	}
	return s.Payment(ctx, tenantID, id)
}

// ConfirmManualPayment settles a pending manual payment with the amount that
// actually arrived (0 = the amount requested).
func (s *Service) ConfirmManualPayment(ctx context.Context, tenantID, paymentID uuid.UUID, amount int64, note string) (*models.Payment, error) {
	err := s.tx(ctx, func(tx *gorm.DB) error {
		pay, err := lock[models.Payment](tx, tenantID, paymentID, "payment")
		if err != nil {
			return err
		}
		if pay.Provider != "manual" {
			return conflict("not_manual", "only manual payments are confirmed by hand")
		}
		if amount <= 0 {
			amount = pay.Amount
		}
		if note != "" {
			tx.Model(pay).Update("note", note)
		}
		return s.settleTx(tx, tenantID, pay.ID, amount, nil)
	})
	if err != nil {
		return nil, err
	}
	return s.Payment(ctx, tenantID, paymentID)
}

// MockOutcome drives a mock payment: "succeed" or "fail".
func (s *Service) MockOutcome(ctx context.Context, tenantID, paymentID uuid.UUID, outcome string) (*models.Payment, error) {
	err := s.tx(ctx, func(tx *gorm.DB) error {
		pay, err := lock[models.Payment](tx, tenantID, paymentID, "payment")
		if err != nil {
			return err
		}
		if pay.Provider != "mock" {
			return conflict("not_mock", "only mock payments can be simulated")
		}
		// Behave like a real gateway: once the cancel has reached it, the
		// intent can no longer be paid. Before that, a payment can still
		// slip through (the race settleTx handles).
		if pay.Status == domain.PaymentCanceled && pay.ProviderCancel != models.ProviderCancelPending {
			return conflict("payment_canceled", "this payment was canceled (%s)", pay.CancelReason)
		}
		switch outcome {
		case "succeed", "success", "paid":
			return s.settleTx(tx, tenantID, pay.ID, pay.Amount, nil)
		case "fail", "failed":
			return s.failTx(tx, tenantID, pay.ID, "declined (simulated)")
		}
		return invalid("invalid_outcome", "outcome must be succeed or fail")
	})
	if err != nil {
		return nil, err
	}
	return s.Payment(ctx, tenantID, paymentID)
}

// PaymentForCheckout loads a payment by id alone, for the public mock checkout
// page (the unguessable id is the capability; mock moves no money).
func (s *Service) PaymentForCheckout(ctx context.Context, paymentID uuid.UUID) (*models.Payment, *models.Invoice, *models.Tenant, error) {
	var pay models.Payment
	if err := s.db.WithContext(ctx).Take(&pay, "id = ? AND provider = 'mock'", paymentID).Error; err != nil {
		return nil, nil, nil, notFound("payment")
	}
	inv, err := s.Invoice(ctx, pay.TenantID, pay.InvoiceID)
	if err != nil {
		return nil, nil, nil, err
	}
	t, err := s.Tenant(ctx, pay.TenantID)
	return &pay, inv, t, err
}

// settleTx is the single place money is recognised as received:
//
//   - the payment becomes succeeded (settling twice is a no-op);
//   - cash ← receivable for what the invoice still owed;
//   - anything beyond that (overpayment, or a payment on an invoice that is no
//     longer open) goes to the customer's credit balance — never lost, never
//     applied twice;
//   - a fully covered invoice becomes paid, which may activate or renew a
//     subscription.
func (s *Service) settleTx(tx *gorm.DB, tenantID, paymentID uuid.UUID, amount int64, at *time.Time) error {
	pay, err := lock[models.Payment](tx, tenantID, paymentID, "payment")
	if err != nil {
		return err
	}
	lateAfterCancel := false
	switch pay.Status {
	case domain.PaymentSucceeded:
		return nil
	case domain.PaymentPending:
	case domain.PaymentCanceled:
		// The customer paid an intent we had already given up on (it
		// expired, or the invoice was paid another way before the gateway
		// learned of the cancel). The money is real, so it is recorded; if
		// the invoice is no longer open it becomes customer credit.
		lateAfterCancel = true
	default:
		return conflict("payment_not_pending", "payment is %s", pay.Status)
	}
	if amount <= 0 {
		return invalid("invalid_amount", "amount must be positive")
	}
	inv, err := lock[models.Invoice](tx, tenantID, pay.InvoiceID, "invoice")
	if err != nil {
		return err
	}
	now := s.now()
	if at == nil {
		at = &now
	}

	apply := int64(0)
	if inv.Status == domain.InvoiceOpen {
		apply = min(amount, inv.AmountDue)
	}
	cash, err := ledger.System(tx, tenantID, ledger.CashCode(pay.Provider), pay.Currency)
	if err != nil {
		return err
	}
	recv, err := ledger.Receivable(tx, tenantID, pay.CustomerID, pay.Currency)
	if err != nil {
		return err
	}
	credit, err := ledger.CustomerCredit(tx, tenantID, pay.CustomerID, pay.Currency)
	if err != nil {
		return err
	}
	if _, _, err := ledger.Post(tx, ledger.Posting{
		TenantID: tenantID, Kind: "payment_received", IdempotencyKey: "payment:" + pay.ID.String(),
		Currency: pay.Currency, InvoiceID: &inv.ID, PaymentID: &pay.ID, CustomerID: &pay.CustomerID,
		Description: "Payment via " + pay.Provider + " for " + deref(inv.Number), EffectiveAt: *at,
		Lines: []ledger.Line{
			ledger.Debit(cash, amount),
			ledger.Credit(recv, apply),
			ledger.Credit(credit, amount-apply),
		},
	}); err != nil {
		return err
	}

	pay.Status = domain.PaymentSucceeded
	pay.Amount = amount
	pay.SettledAt = at
	updates := map[string]any{"status": pay.Status, "amount": amount, "settled_at": *at}
	if lateAfterCancel {
		pay.Note = strings.TrimSpace(pay.Note + " paid after it was canceled (" + pay.CancelReason + ")")
		updates["note"] = pay.Note
		if pay.ProviderCancel == models.ProviderCancelPending {
			// No point closing an intent that has been paid.
			pay.ProviderCancel = models.ProviderCancelAlreadyPaid
			updates["provider_cancel"] = pay.ProviderCancel
		}
	}
	if err := tx.Model(&models.Payment{}).Where("id = ?", pay.ID).Updates(updates).Error; err != nil {
		return err
	}
	if err := events.Emit(tx, tenantID, events.PaymentSucceeded, pay); err != nil {
		return err
	}

	if apply > 0 {
		inv.AmountPaid += apply
		inv.AmountDue -= apply
		if err := tx.Model(&models.Invoice{}).Where("id = ?", inv.ID).
			Updates(map[string]any{"amount_paid": inv.AmountPaid, "amount_due": inv.AmountDue}).Error; err != nil {
			return err
		}
		if inv.AmountDue == 0 {
			if err := s.markPaid(tx, inv); err != nil {
				return err
			}
		} else if err := events.Emit(tx, tenantID, events.InvoicePartiallyPaid, inv); err != nil {
			return err
		}
	}
	// Other pending attempts on a now-paid invoice are moot.
	if inv.Status == domain.InvoicePaid {
		if err := s.cancelPendingPayments(tx, inv.ID, "invoice paid by another payment"); err != nil {
			return err
		}
	}
	if amount-apply > 0 {
		return s.applyCreditToOpenInvoices(tx, tenantID, pay.CustomerID, pay.Currency)
	}
	return nil
}

func (s *Service) failTx(tx *gorm.DB, tenantID, paymentID uuid.UUID, reason string) error {
	pay, err := lock[models.Payment](tx, tenantID, paymentID, "payment")
	if err != nil {
		return err
	}
	if pay.Status != domain.PaymentPending {
		return conflict("payment_not_pending", "payment is %s", pay.Status)
	}
	pay.Status = domain.PaymentFailed
	pay.FailureReason = reason
	if err := tx.Model(&models.Payment{}).Where("id = ?", pay.ID).
		Updates(map[string]any{"status": pay.Status, "failure_reason": reason}).Error; err != nil {
		return err
	}
	return events.Emit(tx, tenantID, events.PaymentFailed, pay)
}

// CancelPayment abandons a pending payment.
func (s *Service) CancelPayment(ctx context.Context, tenantID, paymentID uuid.UUID) (*models.Payment, error) {
	err := s.tx(ctx, func(tx *gorm.DB) error {
		pay, err := lock[models.Payment](tx, tenantID, paymentID, "payment")
		if err != nil {
			return err
		}
		if pay.Status != domain.PaymentPending {
			return conflict("payment_not_pending", "payment is %s", pay.Status)
		}
		return s.cancelPaymentTx(tx, pay, "canceled on request")
	})
	if err != nil {
		return nil, err
	}
	return s.Payment(ctx, tenantID, paymentID)
}

// cancelPaymentTx cancels a pending payment. If its provider can close intents
// (providers.Canceler), the gateway cancel is queued for the worker rather than
// called here: a network call must not hold this transaction open, and must
// not happen at all if the transaction rolls back.
func (s *Service) cancelPaymentTx(tx *gorm.DB, pay *models.Payment, reason string) error {
	pay.Status = domain.PaymentCanceled
	pay.CancelReason = reason
	updates := map[string]any{"status": pay.Status, "cancel_reason": reason}
	if prov, ok := s.providers.Get(pay.Provider); ok {
		if _, ok := providers.CanCancel(prov); ok {
			now := s.now()
			pay.ProviderCancel = models.ProviderCancelPending
			pay.ProviderCancelNextAt = &now
			updates["provider_cancel"] = pay.ProviderCancel
			updates["provider_cancel_next_at"] = now
		}
	}
	if err := tx.Model(&models.Payment{}).Where("id = ?", pay.ID).Updates(updates).Error; err != nil {
		return err
	}
	return events.Emit(tx, pay.TenantID, events.PaymentCanceled, pay)
}

// cancelPendingPayments cancels every other pending attempt on an invoice,
// e.g. once it has been paid through a different payment.
func (s *Service) cancelPendingPayments(tx *gorm.DB, invoiceID uuid.UUID, reason string) error {
	var pending []models.Payment
	if err := tx.Where("invoice_id = ? AND status = ?", invoiceID, domain.PaymentPending).Find(&pending).Error; err != nil {
		return err
	}
	for i := range pending {
		if err := s.cancelPaymentTx(tx, &pending[i], reason); err != nil {
			return err
		}
	}
	return nil
}

// RefundPayment returns a succeeded payment in full. The money comes back out
// of the provider's cash account; it is taken first from whatever credit the
// customer still holds and otherwise booked as a refund expense. The invoice
// stays paid: a refund is a separate decision, not an undo.
func (s *Service) RefundPayment(ctx context.Context, tenantID, paymentID uuid.UUID, reason string) (*models.Payment, error) {
	err := s.tx(ctx, func(tx *gorm.DB) error {
		pay, err := lock[models.Payment](tx, tenantID, paymentID, "payment")
		if err != nil {
			return err
		}
		if pay.Status != domain.PaymentSucceeded {
			return conflict("payment_not_refundable", "only a succeeded payment can be refunded (status is %s)", pay.Status)
		}
		cash, err := ledger.System(tx, tenantID, ledger.CashCode(pay.Provider), pay.Currency)
		if err != nil {
			return err
		}
		credit, err := ledger.CustomerCredit(tx, tenantID, pay.CustomerID, pay.Currency)
		if err != nil {
			return err
		}
		refunds, err := ledger.System(tx, tenantID, ledger.CodeRefunds, pay.Currency)
		if err != nil {
			return err
		}
		// Only the part of this payment that became credit (an overpayment)
		// is taken back from credit, and only while the customer still holds
		// it; the rest was spent on the invoice and is a refund expense.
		var overpaid int64
		if err := tx.Raw(`
			SELECT COALESCE(SUM(e.credit), 0) FROM ledger_entries e
			  JOIN ledger_transactions t ON t.id = e.transaction_id
			 WHERE t.tenant_id = ? AND t.idempotency_key = ? AND e.account_id = ?`,
			tenantID, "payment:"+pay.ID.String(), credit.ID).Scan(&overpaid).Error; err != nil {
			return err
		}
		fromCredit := min(overpaid, credit.Balance, pay.Amount)
		if _, _, err := ledger.Post(tx, ledger.Posting{
			TenantID: tenantID, Kind: "payment_refunded", IdempotencyKey: "refund:" + pay.ID.String(),
			Currency: pay.Currency, InvoiceID: &pay.InvoiceID, PaymentID: &pay.ID, CustomerID: &pay.CustomerID,
			Description: "Refund: " + reason, EffectiveAt: s.now(),
			Lines: []ledger.Line{
				ledger.Debit(credit, fromCredit),
				ledger.Debit(refunds, pay.Amount-fromCredit),
				ledger.Credit(cash, pay.Amount),
			},
		}); err != nil {
			return err
		}
		now := s.now()
		pay.Status = domain.PaymentRefunded
		pay.RefundedAt = &now
		if reason != "" {
			pay.Note = strings.TrimSpace(pay.Note + " refund: " + reason)
		}
		if err := tx.Model(&models.Payment{}).Where("id = ?", pay.ID).
			Updates(map[string]any{"status": pay.Status, "refunded_at": now, "note": pay.Note}).Error; err != nil {
			return err
		}
		return events.Emit(tx, tenantID, events.PaymentRefunded, pay)
	})
	if err != nil {
		return nil, err
	}
	return s.Payment(ctx, tenantID, paymentID)
}

// HandleWebhook processes a provider callback sent to an account's webhook
// URL (/webhooks/{provider}/{accountID}). The account decides the tenant and
// the secret the callback is verified with; the body is only used to find the
// payment, and the provider is then asked for the truth.
func (s *Service) HandleWebhook(ctx context.Context, providerName string, accountID uuid.UUID, r *http.Request) error {
	prov, ok := s.providers.Get(providerName)
	if !ok {
		return notFound("provider")
	}
	var a models.ProviderAccount
	if err := s.db.WithContext(ctx).Where("id = ? AND provider = ?", accountID, providerName).Take(&a).Error; err != nil {
		return notFound("provider_account")
	}
	acct, err := s.toAccount(&a)
	if err != nil {
		return err
	}
	ref, err := prov.ParseWebhook(r, acct)
	if errors.Is(err, providers.ErrUnsupported) {
		return invalid("webhooks_unsupported", "provider %s does not use webhooks", providerName)
	}
	if err != nil {
		return invalid("invalid_webhook", "%v", err)
	}
	var pay models.Payment
	if err := s.db.WithContext(ctx).Where("tenant_id = ? AND provider = ? AND provider_ref = ?", a.TenantID, providerName, ref).
		Take(&pay).Error; err != nil {
		return notFound("payment")
	}
	res, err := prov.Check(ctx, acct, ref)
	if err != nil {
		return &Error{http.StatusBadGateway, "provider_error", err.Error()}
	}
	return s.tx(ctx, func(tx *gorm.DB) error {
		switch res.Status {
		case providers.CheckPaid:
			return s.settleTx(tx, pay.TenantID, pay.ID, res.Amount, nil)
		case providers.CheckFailed:
			return s.failTx(tx, pay.TenantID, pay.ID, res.Reason)
		}
		return nil
	})
}

// Payment loads a payment.
func (s *Service) Payment(ctx context.Context, tenantID, id uuid.UUID) (*models.Payment, error) {
	return take[models.Payment](s.db.WithContext(ctx), tenantID, id, "payment")
}

// PaymentFilter narrows ListPayments.
type PaymentFilter struct {
	InvoiceID  *uuid.UUID
	CustomerID *uuid.UUID
	Status     string
	Provider   string
	// Query matches the provider reference or the note.
	Query string
	Common
}

// ListPayments lists payments, newest first.
func (s *Service) ListPayments(ctx context.Context, tenantID uuid.UUID, f PaymentFilter, p Page) (List[models.Payment], error) {
	q := s.db.WithContext(ctx).Model(&models.Payment{}).Where("tenant_id = ?", tenantID)
	if f.InvoiceID != nil {
		q = q.Where("invoice_id = ?", *f.InvoiceID)
	}
	if f.CustomerID != nil {
		q = q.Where("customer_id = ?", *f.CustomerID)
	}
	if f.Status != "" {
		q = q.Where("status = ?", f.Status)
	}
	if f.Provider != "" {
		q = q.Where("provider = ?", f.Provider)
	}
	if f.Query != "" {
		like := likePattern(f.Query)
		q = q.Where("provider_ref ILIKE ? OR note ILIKE ?", like, like)
	}
	q, err := f.Common.apply(q, "payments", false)
	if err != nil {
		return List[models.Payment]{}, err
	}
	return paginate[models.Payment](q, "payments", p)
}
