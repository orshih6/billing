package database

import (
	"fmt"
	"strings"

	"gorm.io/gorm"

	"github.com/orshih6/billing/internal/models"
)

// This file holds the schema as the models describe it. It has two jobs:
//
//   - upgrading a database created before versioned migrations existed, once,
//     before it is baselined at version 1;
//   - the drift test, which fails when a model changes without a migration.
//
// New schema changes go in migrations/*.sql, never here alone.

// constraints are the invariants GORM tags cannot express. Each is idempotent
// so Migrate can run on every release.
var constraints = []string{
	// One live subscription per customer per plan.
	`CREATE UNIQUE INDEX IF NOT EXISTS ux_subscription_live
	   ON subscriptions (customer_id, plan_id)
	   WHERE status IN ('incomplete','trialing','active','past_due')`,
	// A subscription period is never invoiced twice.
	`CREATE UNIQUE INDEX IF NOT EXISTS ux_invoice_subscription_period
	   ON invoices (subscription_id, period_start)
	   WHERE subscription_id IS NOT NULL AND kind IN ('subscription_create','subscription_cycle')
	     AND status IN ('draft','open','paid')`,
	// A provider reference settles at most one payment per tenant. For manual
	// payments the reference is the bank statement / receipt id, which is what
	// stops the same transfer being recorded twice.
	`CREATE UNIQUE INDEX IF NOT EXISTS ux_payment_provider_ref
	   ON payments (tenant_id, provider, provider_ref)`,
	// The engine only ever scans pending deliveries.
	`CREATE INDEX IF NOT EXISTS ix_delivery_pending_due
	   ON webhook_deliveries (next_attempt_at) WHERE status = 'pending'`,
	`CREATE INDEX IF NOT EXISTS ix_payment_pending_expiry
	   ON payments (expires_at) WHERE status = 'pending'`,
	// The worker's queue of intents to cancel at the gateway.
	`CREATE INDEX IF NOT EXISTS ix_payment_provider_cancel_due
	   ON payments (provider_cancel_next_at) WHERE provider_cancel = 'pending'`,
	// Ledger legs: exactly one side, never negative.
	`DO $$ BEGIN
	   ALTER TABLE ledger_entries ADD CONSTRAINT ledger_entries_single_side
	     CHECK ((debit > 0 AND credit = 0) OR (credit > 0 AND debit = 0));
	 EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
	`DO $$ BEGIN
	   ALTER TABLE invoices ADD CONSTRAINT invoices_amounts_consistent
	     CHECK (total >= 0 AND amount_paid >= 0 AND credit_applied >= 0 AND amount_due >= 0
	            AND amount_due <= total);
	 EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
	`DO $$ BEGIN
	   ALTER TABLE invoice_lines ADD CONSTRAINT invoice_lines_non_negative
	     CHECK (quantity > 0 AND unit_amount >= 0 AND amount = quantity * unit_amount);
	 EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
	`DO $$ BEGIN
	   ALTER TABLE prices ADD CONSTRAINT prices_valid
	     CHECK (amount >= 0 AND interval_count >= 1 AND trial_days >= 0);
	 EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
	`DO $$ BEGIN
	   ALTER TABLE payments ADD CONSTRAINT payments_positive CHECK (amount > 0);
	 EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
	`DO $$ BEGIN
	   ALTER TABLE invoices ADD CONSTRAINT invoices_totals_consistent
	     CHECK (subtotal >= 0 AND discount_total >= 0 AND discount_total <= subtotal
	            AND tax_total >= 0 AND tax_rate_bps >= 0);
	 EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
	`DO $$ BEGIN
	   ALTER TABLE subscriptions ADD CONSTRAINT subscriptions_quantity_positive CHECK (quantity >= 1);
	 EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
	`DO $$ BEGIN
	   ALTER TABLE subscription_items ADD CONSTRAINT subscription_items_quantity_positive CHECK (quantity >= 1);
	 EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
	`DO $$ BEGIN
	   ALTER TABLE coupons ADD CONSTRAINT coupons_one_kind
	     CHECK ((percent_off IS NOT NULL AND amount_off IS NULL AND percent_off BETWEEN 1 AND 100)
	         OR (amount_off IS NOT NULL AND percent_off IS NULL AND amount_off > 0 AND currency <> ''));
	 EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
	// One active add-on row per plan on a subscription.
	`CREATE UNIQUE INDEX IF NOT EXISTS ux_subscription_item_plan
	   ON subscription_items (subscription_id, plan_id) WHERE removed_at IS NULL`,
	// Invoices created before subtotal existed: their subtotal was the total.
	`UPDATE invoices SET subtotal = total WHERE subtotal = 0 AND total > 0 AND discount_total = 0 AND tax_total = 0`,
	// Ledger rows are append-only: nothing may update or delete an entry.
	`CREATE OR REPLACE FUNCTION ledger_entries_append_only() RETURNS trigger AS $$
	 BEGIN RAISE EXCEPTION 'ledger_entries are append-only'; END; $$ LANGUAGE plpgsql`,
	`DROP TRIGGER IF EXISTS ledger_entries_no_mutation ON ledger_entries`,
	`CREATE TRIGGER ledger_entries_no_mutation BEFORE UPDATE OR DELETE ON ledger_entries
	   FOR EACH ROW EXECUTE FUNCTION ledger_entries_append_only()`,
}

// autoMigrate builds the schema from the GORM models: the pre-migrations way.
func autoMigrate(db *gorm.DB) error {
	if err := db.AutoMigrate(models.All()...); err != nil {
		return fmt.Errorf("database: automigrate: %w", err)
	}
	for _, s := range constraints {
		if err := db.Exec(s).Error; err != nil {
			return fmt.Errorf("database: constraint %q: %w", firstLine(s), err)
		}
	}
	return nil
}

// baselineLegacy adopts a database that AutoMigrate created: it completes the
// schema to the 00001 shape, then records version 1 as applied so goose does
// not try to create the tables again.
func baselineLegacy(db *gorm.DB) error {
	var hasGoose, hasTenants bool
	if err := db.Raw(`SELECT to_regclass('goose_db_version') IS NOT NULL`).Scan(&hasGoose).Error; err != nil {
		return err
	}
	if hasGoose {
		return nil
	}
	if err := db.Raw(`SELECT to_regclass('tenants') IS NOT NULL`).Scan(&hasTenants).Error; err != nil {
		return err
	}
	if !hasTenants {
		return nil // a fresh database: 00001 creates everything
	}
	if err := autoMigrate(db); err != nil {
		return fmt.Errorf("database: bring legacy schema up to date: %w", err)
	}
	return db.Exec(`
		CREATE TABLE goose_db_version (
			id serial PRIMARY KEY, version_id bigint NOT NULL,
			is_applied boolean NOT NULL, tstamp timestamp DEFAULT now());
		INSERT INTO goose_db_version (version_id, is_applied) VALUES (0, true), (1, true);`).Error
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i > 0 {
		return s[:i]
	}
	return s
}
