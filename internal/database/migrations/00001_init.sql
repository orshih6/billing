-- Initial schema: tenants, keys, customers, catalog (plans, prices, coupons),
-- subscriptions and add-ons, invoices, payments, provider accounts, the
-- double-entry ledger, idempotency records and the event/webhook outbox.
--
-- Invariants enforced here rather than in code: one live subscription per
-- customer and plan, no period invoiced twice, single-sided positive ledger
-- legs, append-only ledger entries (trigger), consistent invoice totals.

-- +goose Up
-- +goose StatementBegin
CREATE FUNCTION ledger_entries_append_only() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
	 BEGIN RAISE EXCEPTION 'ledger_entries are append-only'; END; $$;
-- +goose StatementEnd

CREATE TABLE api_keys (
    id uuid NOT NULL,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    tenant_id uuid,
    name character varying(120) NOT NULL,
    prefix character varying(32) NOT NULL,
    last_four character varying(4) NOT NULL,
    hash character varying(64) NOT NULL,
    scopes jsonb NOT NULL,
    expires_at timestamp with time zone,
    revoked_at timestamp with time zone,
    last_used_at timestamp with time zone
);

CREATE TABLE coupons (
    id uuid NOT NULL,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    tenant_id uuid NOT NULL,
    code character varying(64) NOT NULL,
    name character varying(200) NOT NULL,
    percent_off bigint,
    amount_off bigint,
    currency character varying(3),
    duration character varying(16) NOT NULL,
    duration_periods bigint,
    plan_ids jsonb NOT NULL,
    max_redemptions bigint,
    times_redeemed bigint DEFAULT 0 NOT NULL,
    redeem_by timestamp with time zone,
    active boolean DEFAULT true NOT NULL,
    CONSTRAINT coupons_one_kind CHECK ((((percent_off IS NOT NULL) AND (amount_off IS NULL) AND ((percent_off >= 1) AND (percent_off <= 100))) OR ((amount_off IS NOT NULL) AND (percent_off IS NULL) AND (amount_off > 0) AND ((currency)::text <> ''::text))))
);

CREATE TABLE customers (
    id uuid NOT NULL,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    tenant_id uuid NOT NULL,
    external_id character varying(200),
    name character varying(200) NOT NULL,
    email character varying(320),
    phone character varying(40),
    metadata jsonb,
    archived_at timestamp with time zone,
    tax_exempt boolean DEFAULT false NOT NULL,
    tax_rate_bps bigint
);

CREATE TABLE discounts (
    id uuid NOT NULL,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    tenant_id uuid NOT NULL,
    coupon_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    subscription_id uuid,
    invoice_id uuid,
    ended_at timestamp with time zone
);

CREATE TABLE events (
    id uuid NOT NULL,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    tenant_id uuid NOT NULL,
    type character varying(64) NOT NULL,
    data jsonb NOT NULL
);

CREATE TABLE idempotency_records (
    tenant_id uuid NOT NULL,
    key character varying(200) NOT NULL,
    method character varying(8) NOT NULL,
    path character varying(500) NOT NULL,
    request_hash character varying(64) NOT NULL,
    status bigint DEFAULT 0 NOT NULL,
    response bytea,
    created_at timestamp with time zone NOT NULL
);

CREATE TABLE invoice_lines (
    id uuid NOT NULL,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    invoice_id uuid NOT NULL,
    price_id uuid,
    plan_id uuid,
    subscription_item_id uuid,
    description character varying(500) NOT NULL,
    quantity bigint NOT NULL,
    unit_amount bigint NOT NULL,
    amount bigint NOT NULL,
    period_start timestamp with time zone,
    period_end timestamp with time zone,
    CONSTRAINT invoice_lines_non_negative CHECK (((quantity > 0) AND (unit_amount >= 0) AND (amount = (quantity * unit_amount))))
);

CREATE TABLE invoices (
    id uuid NOT NULL,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    tenant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    subscription_id uuid,
    number character varying(40),
    reference character varying(16),
    kind character varying(24) NOT NULL,
    status character varying(16) NOT NULL,
    currency character varying(3) NOT NULL,
    subtotal bigint DEFAULT 0 NOT NULL,
    discount_total bigint DEFAULT 0 NOT NULL,
    discount_id uuid,
    discount_description character varying(200),
    tax_total bigint DEFAULT 0 NOT NULL,
    tax_rate_bps bigint DEFAULT 0 NOT NULL,
    tax_name character varying(40),
    tax_inclusive boolean DEFAULT false NOT NULL,
    total bigint DEFAULT 0 NOT NULL,
    amount_paid bigint DEFAULT 0 NOT NULL,
    credit_applied bigint DEFAULT 0 NOT NULL,
    amount_due bigint DEFAULT 0 NOT NULL,
    period_start timestamp with time zone,
    period_end timestamp with time zone,
    due_at timestamp with time zone,
    finalized_at timestamp with time zone,
    paid_at timestamp with time zone,
    voided_at timestamp with time zone,
    memo character varying(2000),
    metadata jsonb,
    CONSTRAINT invoices_amounts_consistent CHECK (((total >= 0) AND (amount_paid >= 0) AND (credit_applied >= 0) AND (amount_due >= 0) AND (amount_due <= total))),
    CONSTRAINT invoices_totals_consistent CHECK (((subtotal >= 0) AND (discount_total >= 0) AND (discount_total <= subtotal) AND (tax_total >= 0) AND (tax_rate_bps >= 0)))
);

CREATE TABLE ledger_accounts (
    id uuid NOT NULL,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    tenant_id uuid NOT NULL,
    code character varying(120) NOT NULL,
    currency character varying(3) NOT NULL,
    type character varying(16) NOT NULL,
    name character varying(200) NOT NULL,
    customer_id uuid,
    balance bigint DEFAULT 0 NOT NULL
);

CREATE TABLE ledger_entries (
    id bigint NOT NULL,
    transaction_id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    account_id uuid NOT NULL,
    debit bigint DEFAULT 0 NOT NULL,
    credit bigint DEFAULT 0 NOT NULL,
    created_at timestamp with time zone NOT NULL,
    CONSTRAINT ledger_entries_single_side CHECK ((((debit > 0) AND (credit = 0)) OR ((credit > 0) AND (debit = 0))))
);

CREATE SEQUENCE ledger_entries_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE ledger_entries_id_seq OWNED BY ledger_entries.id;

CREATE TABLE ledger_transactions (
    id uuid NOT NULL,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    tenant_id uuid NOT NULL,
    kind character varying(40) NOT NULL,
    idempotency_key character varying(200) NOT NULL,
    currency character varying(3) NOT NULL,
    invoice_id uuid,
    payment_id uuid,
    customer_id uuid,
    description character varying(500),
    effective_at timestamp with time zone NOT NULL,
    metadata jsonb
);

CREATE TABLE payments (
    id uuid NOT NULL,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    tenant_id uuid NOT NULL,
    invoice_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    provider character varying(32) NOT NULL,
    provider_account_id uuid,
    provider_ref character varying(200) NOT NULL,
    status character varying(16) NOT NULL,
    amount bigint NOT NULL,
    currency character varying(3) NOT NULL,
    instructions character varying(2000),
    pay_url character varying(1000),
    return_url character varying(1000),
    expires_at timestamp with time zone,
    settled_at timestamp with time zone,
    refunded_at timestamp with time zone,
    failure_reason character varying(500),
    note character varying(1000),
    created_by_key_id uuid,
    raw jsonb,
    cancel_reason character varying(200),
    provider_cancel character varying(16) DEFAULT ''::character varying NOT NULL,
    provider_cancel_attempts bigint DEFAULT 0 NOT NULL,
    provider_cancel_next_at timestamp with time zone,
    provider_cancel_error character varying(500),
    provider_canceled_at timestamp with time zone,
    CONSTRAINT payments_positive CHECK ((amount > 0))
);

CREATE TABLE plans (
    id uuid NOT NULL,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    tenant_id uuid NOT NULL,
    code character varying(64) NOT NULL,
    name character varying(200) NOT NULL,
    description character varying(2000),
    active boolean DEFAULT true NOT NULL,
    kind character varying(8) DEFAULT 'base'::character varying NOT NULL,
    metadata jsonb
);

CREATE TABLE prices (
    id uuid NOT NULL,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    tenant_id uuid NOT NULL,
    plan_id uuid NOT NULL,
    nickname character varying(120),
    amount bigint NOT NULL,
    currency character varying(3) NOT NULL,
    "interval" character varying(16) NOT NULL,
    interval_count bigint DEFAULT 1 NOT NULL,
    trial_days bigint DEFAULT 0 NOT NULL,
    active boolean DEFAULT true NOT NULL,
    CONSTRAINT prices_valid CHECK (((amount >= 0) AND (interval_count >= 1) AND (trial_days >= 0)))
);

CREATE TABLE provider_accounts (
    id uuid NOT NULL,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    tenant_id uuid NOT NULL,
    provider character varying(32) NOT NULL,
    mode character varying(8) DEFAULT 'test'::character varying NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    config bytea NOT NULL,
    hints jsonb
);

CREATE TABLE subscription_items (
    id uuid NOT NULL,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    tenant_id uuid NOT NULL,
    subscription_id uuid NOT NULL,
    plan_id uuid NOT NULL,
    price_id uuid NOT NULL,
    quantity bigint NOT NULL,
    pending_quantity bigint,
    pending_from timestamp with time zone,
    removed_at timestamp with time zone,
    CONSTRAINT subscription_items_quantity_positive CHECK ((quantity >= 1))
);

CREATE TABLE subscriptions (
    id uuid NOT NULL,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    tenant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    plan_id uuid NOT NULL,
    price_id uuid NOT NULL,
    pending_price_id uuid,
    quantity bigint DEFAULT 1 NOT NULL,
    pending_quantity bigint,
    pending_from timestamp with time zone,
    discount_id uuid,
    status character varying(16) NOT NULL,
    billing_anchor timestamp with time zone NOT NULL,
    cycle bigint DEFAULT 0 NOT NULL,
    current_period_start timestamp with time zone NOT NULL,
    current_period_end timestamp with time zone NOT NULL,
    trial_end timestamp with time zone,
    cancel_at_period_end boolean DEFAULT false NOT NULL,
    canceled_at timestamp with time zone,
    ended_at timestamp with time zone,
    metadata jsonb,
    CONSTRAINT subscriptions_quantity_positive CHECK ((quantity >= 1))
);

CREATE TABLE tenants (
    id uuid NOT NULL,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    slug character varying(64) NOT NULL,
    name character varying(200) NOT NULL,
    default_currency character varying(3) NOT NULL,
    settings jsonb NOT NULL,
    status character varying(16) DEFAULT 'active'::character varying NOT NULL,
    invoice_seq bigint DEFAULT 0 NOT NULL
);

CREATE TABLE webhook_deliveries (
    id uuid NOT NULL,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    tenant_id uuid NOT NULL,
    event_id uuid NOT NULL,
    endpoint_id uuid NOT NULL,
    status character varying(16) NOT NULL,
    attempts bigint DEFAULT 0 NOT NULL,
    next_attempt_at timestamp with time zone NOT NULL,
    last_status_code bigint,
    last_error character varying(1000),
    delivered_at timestamp with time zone
);

CREATE TABLE webhook_endpoints (
    id uuid NOT NULL,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    tenant_id uuid NOT NULL,
    url character varying(1000) NOT NULL,
    description character varying(200),
    secret character varying(100) NOT NULL,
    event_types jsonb NOT NULL,
    enabled boolean DEFAULT true NOT NULL
);

ALTER TABLE ONLY ledger_entries ALTER COLUMN id SET DEFAULT nextval('ledger_entries_id_seq'::regclass);

ALTER TABLE ONLY api_keys
    ADD CONSTRAINT api_keys_pkey PRIMARY KEY (id);

ALTER TABLE ONLY coupons
    ADD CONSTRAINT coupons_pkey PRIMARY KEY (id);

ALTER TABLE ONLY customers
    ADD CONSTRAINT customers_pkey PRIMARY KEY (id);

ALTER TABLE ONLY discounts
    ADD CONSTRAINT discounts_pkey PRIMARY KEY (id);

ALTER TABLE ONLY events
    ADD CONSTRAINT events_pkey PRIMARY KEY (id);

ALTER TABLE ONLY idempotency_records
    ADD CONSTRAINT idempotency_records_pkey PRIMARY KEY (tenant_id, key);

ALTER TABLE ONLY invoice_lines
    ADD CONSTRAINT invoice_lines_pkey PRIMARY KEY (id);

ALTER TABLE ONLY invoices
    ADD CONSTRAINT invoices_pkey PRIMARY KEY (id);

ALTER TABLE ONLY ledger_accounts
    ADD CONSTRAINT ledger_accounts_pkey PRIMARY KEY (id);

ALTER TABLE ONLY ledger_entries
    ADD CONSTRAINT ledger_entries_pkey PRIMARY KEY (id);

ALTER TABLE ONLY ledger_transactions
    ADD CONSTRAINT ledger_transactions_pkey PRIMARY KEY (id);

ALTER TABLE ONLY payments
    ADD CONSTRAINT payments_pkey PRIMARY KEY (id);

ALTER TABLE ONLY plans
    ADD CONSTRAINT plans_pkey PRIMARY KEY (id);

ALTER TABLE ONLY prices
    ADD CONSTRAINT prices_pkey PRIMARY KEY (id);

ALTER TABLE ONLY provider_accounts
    ADD CONSTRAINT provider_accounts_pkey PRIMARY KEY (id);

ALTER TABLE ONLY subscription_items
    ADD CONSTRAINT subscription_items_pkey PRIMARY KEY (id);

ALTER TABLE ONLY subscriptions
    ADD CONSTRAINT subscriptions_pkey PRIMARY KEY (id);

ALTER TABLE ONLY tenants
    ADD CONSTRAINT tenants_pkey PRIMARY KEY (id);

ALTER TABLE ONLY webhook_deliveries
    ADD CONSTRAINT webhook_deliveries_pkey PRIMARY KEY (id);

ALTER TABLE ONLY webhook_endpoints
    ADD CONSTRAINT webhook_endpoints_pkey PRIMARY KEY (id);

CREATE UNIQUE INDEX idx_api_keys_hash ON api_keys USING btree (hash);

CREATE INDEX idx_api_keys_tenant_id ON api_keys USING btree (tenant_id);

CREATE INDEX idx_discounts_coupon_id ON discounts USING btree (coupon_id);

CREATE INDEX idx_discounts_customer_id ON discounts USING btree (customer_id);

CREATE INDEX idx_discounts_invoice_id ON discounts USING btree (invoice_id);

CREATE INDEX idx_discounts_subscription_id ON discounts USING btree (subscription_id);

CREATE INDEX idx_discounts_tenant_id ON discounts USING btree (tenant_id);

CREATE INDEX idx_events_tenant_id ON events USING btree (tenant_id);

CREATE INDEX idx_events_type ON events USING btree (type);

CREATE INDEX idx_idempotency_records_created_at ON idempotency_records USING btree (created_at);

CREATE INDEX idx_invoice_lines_invoice_id ON invoice_lines USING btree (invoice_id);

CREATE INDEX idx_invoices_customer_id ON invoices USING btree (customer_id);

CREATE INDEX idx_invoices_discount_id ON invoices USING btree (discount_id);

CREATE INDEX idx_invoices_status ON invoices USING btree (status);

CREATE INDEX idx_invoices_subscription_id ON invoices USING btree (subscription_id);

CREATE INDEX idx_invoices_tenant_id ON invoices USING btree (tenant_id);

CREATE INDEX idx_ledger_accounts_customer_id ON ledger_accounts USING btree (customer_id);

CREATE INDEX idx_ledger_entries_account_id ON ledger_entries USING btree (account_id);

CREATE INDEX idx_ledger_entries_tenant_id ON ledger_entries USING btree (tenant_id);

CREATE INDEX idx_ledger_entries_transaction_id ON ledger_entries USING btree (transaction_id);

CREATE INDEX idx_ledger_transactions_customer_id ON ledger_transactions USING btree (customer_id);

CREATE INDEX idx_ledger_transactions_invoice_id ON ledger_transactions USING btree (invoice_id);

CREATE INDEX idx_ledger_transactions_payment_id ON ledger_transactions USING btree (payment_id);

CREATE INDEX idx_ledger_transactions_tenant_id ON ledger_transactions USING btree (tenant_id);

CREATE INDEX idx_payments_customer_id ON payments USING btree (customer_id);

CREATE INDEX idx_payments_invoice_id ON payments USING btree (invoice_id);

CREATE INDEX idx_payments_status ON payments USING btree (status);

CREATE INDEX idx_payments_tenant_id ON payments USING btree (tenant_id);

CREATE INDEX idx_prices_plan_id ON prices USING btree (plan_id);

CREATE INDEX idx_prices_tenant_id ON prices USING btree (tenant_id);

CREATE INDEX idx_subscription_items_subscription_id ON subscription_items USING btree (subscription_id);

CREATE INDEX idx_subscription_items_tenant_id ON subscription_items USING btree (tenant_id);

CREATE INDEX idx_subscriptions_current_period_end ON subscriptions USING btree (current_period_end);

CREATE INDEX idx_subscriptions_customer_id ON subscriptions USING btree (customer_id);

CREATE INDEX idx_subscriptions_plan_id ON subscriptions USING btree (plan_id);

CREATE INDEX idx_subscriptions_status ON subscriptions USING btree (status);

CREATE INDEX idx_subscriptions_tenant_id ON subscriptions USING btree (tenant_id);

CREATE UNIQUE INDEX idx_tenants_slug ON tenants USING btree (slug);

CREATE INDEX idx_webhook_deliveries_endpoint_id ON webhook_deliveries USING btree (endpoint_id);

CREATE INDEX idx_webhook_deliveries_event_id ON webhook_deliveries USING btree (event_id);

CREATE INDEX idx_webhook_deliveries_tenant_id ON webhook_deliveries USING btree (tenant_id);

CREATE INDEX idx_webhook_endpoints_tenant_id ON webhook_endpoints USING btree (tenant_id);

CREATE INDEX ix_delivery_pending_due ON webhook_deliveries USING btree (next_attempt_at) WHERE ((status)::text = 'pending'::text);

CREATE INDEX ix_payment_pending_expiry ON payments USING btree (expires_at) WHERE ((status)::text = 'pending'::text);

CREATE INDEX ix_payment_provider_cancel_due ON payments USING btree (provider_cancel_next_at) WHERE ((provider_cancel)::text = 'pending'::text);

CREATE UNIQUE INDEX ux_coupon_code ON coupons USING btree (tenant_id, code);

CREATE UNIQUE INDEX ux_customer_external ON customers USING btree (tenant_id, external_id);

CREATE UNIQUE INDEX ux_invoice_number ON invoices USING btree (tenant_id, number);

CREATE UNIQUE INDEX ux_invoice_reference ON invoices USING btree (tenant_id, reference);

CREATE UNIQUE INDEX ux_invoice_subscription_period ON invoices USING btree (subscription_id, period_start) WHERE ((subscription_id IS NOT NULL) AND ((kind)::text = ANY ((ARRAY['subscription_create'::character varying, 'subscription_cycle'::character varying])::text[])) AND ((status)::text = ANY ((ARRAY['draft'::character varying, 'open'::character varying, 'paid'::character varying])::text[])));

CREATE UNIQUE INDEX ux_ledger_account ON ledger_accounts USING btree (tenant_id, code, currency);

CREATE UNIQUE INDEX ux_ledger_idem ON ledger_transactions USING btree (tenant_id, idempotency_key);

CREATE UNIQUE INDEX ux_payment_provider_ref ON payments USING btree (tenant_id, provider, provider_ref);

CREATE UNIQUE INDEX ux_plan_code ON plans USING btree (tenant_id, code);

CREATE UNIQUE INDEX ux_provider_account ON provider_accounts USING btree (tenant_id, provider);

CREATE UNIQUE INDEX ux_subscription_item_plan ON subscription_items USING btree (subscription_id, plan_id) WHERE (removed_at IS NULL);

CREATE UNIQUE INDEX ux_subscription_live ON subscriptions USING btree (customer_id, plan_id) WHERE ((status)::text = ANY ((ARRAY['incomplete'::character varying, 'trialing'::character varying, 'active'::character varying, 'past_due'::character varying])::text[]));

CREATE TRIGGER ledger_entries_no_mutation BEFORE DELETE OR UPDATE ON ledger_entries FOR EACH ROW EXECUTE FUNCTION ledger_entries_append_only();

ALTER TABLE ONLY invoice_lines
    ADD CONSTRAINT fk_invoices_lines FOREIGN KEY (invoice_id) REFERENCES invoices(id);

ALTER TABLE ONLY ledger_entries
    ADD CONSTRAINT fk_ledger_transactions_entries FOREIGN KEY (transaction_id) REFERENCES ledger_transactions(id);

ALTER TABLE ONLY prices
    ADD CONSTRAINT fk_plans_prices FOREIGN KEY (plan_id) REFERENCES plans(id);

-- +goose Down
DROP TRIGGER IF EXISTS ledger_entries_no_mutation ON ledger_entries;
DROP TABLE IF EXISTS webhook_endpoints CASCADE;
DROP TABLE IF EXISTS webhook_deliveries CASCADE;
DROP TABLE IF EXISTS tenants CASCADE;
DROP TABLE IF EXISTS subscriptions CASCADE;
DROP TABLE IF EXISTS subscription_items CASCADE;
DROP TABLE IF EXISTS provider_accounts CASCADE;
DROP TABLE IF EXISTS prices CASCADE;
DROP TABLE IF EXISTS plans CASCADE;
DROP TABLE IF EXISTS payments CASCADE;
DROP TABLE IF EXISTS ledger_transactions CASCADE;
DROP TABLE IF EXISTS ledger_entries CASCADE;
DROP TABLE IF EXISTS ledger_accounts CASCADE;
DROP TABLE IF EXISTS invoices CASCADE;
DROP TABLE IF EXISTS invoice_lines CASCADE;
DROP TABLE IF EXISTS idempotency_records CASCADE;
DROP TABLE IF EXISTS events CASCADE;
DROP TABLE IF EXISTS discounts CASCADE;
DROP TABLE IF EXISTS customers CASCADE;
DROP TABLE IF EXISTS coupons CASCADE;
DROP TABLE IF EXISTS api_keys CASCADE;
DROP FUNCTION IF EXISTS ledger_entries_append_only();
