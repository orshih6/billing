# Billing model

This is the source of truth for how money moves in this service. The code
implements it; when the two disagree, fix the code or change this document on
purpose — never let them drift.

## Tenancy

- A **tenant** is one integrating system (a SaaS). Everything else — customers,
  plans, invoices, the ledger — belongs to exactly one tenant.
- The tenant of a request comes **from the API key**, never from the body. A
  tenant key that sends `X-Tenant-Id` for another tenant is refused (403), not
  silently corrected.
- A **platform key** (no tenant) manages tenants and may act inside any tenant by
  naming it in `X-Tenant-Id`.
- A suspended tenant's keys stop working without being revoked.

## Money

- Every amount is an `int64` in the currency's **minor unit** (ISO 4217
  exponent: MNT and USD have 2, JPY and KRW have 0). No float touches money.
- Everything with money has exactly one currency. There is **no FX**: a price, an
  invoice and a ledger account agree on currency or the operation is refused.
- Human input is parsed with `domain.ParseAmount`, which rejects extra decimals
  instead of rounding.

## The ledger

- Double-entry, **append-only** (a database trigger refuses UPDATE/DELETE of
  entries). Mistakes are corrected with a new posting.
- `internal/ledger.Post` is the only writer. It checks that the posting
  balances, that every account is the posting's tenant and currency, and that no
  **customer** account goes negative.
- Every posting has an idempotency key (e.g. `invoice-finalize:<id>`,
  `payment:<id>`); posting it again returns the original (`replayed`) and changes
  nothing.
- Postings happen inside the same database transaction as the state change they
  describe, so an invoice can never be "paid" without its posting, or vice versa.
- `GET /reports/ledger-reconciliation` recomputes every balance from entries and
  checks every transaction balances. It must always return `ok: true`.

| Event | Debit | Credit |
|---|---|---|
| Invoice finalized | customer receivable (total), discounts | revenue (subtotal net of inclusive tax), tax_payable |
| Customer credit applied to an invoice | customer credit | customer receivable |
| Payment received | `cash:<provider>` | customer receivable (up to what is due), customer credit (the rest) |
| Invoice voided | revenue, tax_payable (reversing the finalize) | discounts, customer receivable (still due), customer credit (already paid) |
| Invoice written off (uncollectible) | bad_debt | customer receivable (still due) |
| Refund | customer credit (unspent overpayment of that payment), refunds (rest) | `cash:<provider>` |
| Credit granted | credit_grants | customer credit |

## Invoices

- `draft → open → paid`, or `void` / `uncollectible`. Drafts post nothing.
- Finalizing assigns the next number (`PREFIX-000123`, per tenant, gap-free by row
  lock) and a 6-character **payment reference** from an alphabet without
  0/O/1/I, for bank transfer descriptions.
- On finalize, any credit the customer holds is applied immediately. An invoice
  whose amount due reaches 0 is paid at that moment.
- Money is never lost: paying more than is due, or paying an invoice that is no
  longer open, puts the excess in the customer's **credit**, which is then
  applied to their other open invoices, oldest due first.
- Voiding returns whatever was already paid on the invoice to credit.
- Paid invoices cannot be voided; refund the payment instead.

## Payments

- `pending → succeeded | failed | canceled`, and `succeeded → refunded`.
- `payments.settleTx` is the single place money is recognised. Settling twice
  is a no-op.
- Starting a payment while an unexpired pending one exists for the same provider
  and amount returns the existing one.
- Pending payments expire after `payment_expiry_hours`.
- **mock** is a fake checkout (`/checkout/mock/{id}`), for development and
  integration tests. It is disabled when `APP_ENV=production` unless the tenant
  sets `mock_enabled`.
- **manual** is money that arrived outside any gateway. Recording it needs the
  separate `payments:manual` scope, because it asserts that money was received.
  A manual payment's `reference` (bank statement id) is unique per tenant, so the
  same transfer can never be recorded twice.
- Provider webhooks (`POST /webhooks/{provider}`) are never trusted: the body is
  only used to find the payment; the provider is then asked for the truth.
- **Provider accounts**: each tenant connects its own merchant account per
  gateway. Credentials are stored AES-256-GCM encrypted under `SECRETS_KEY`,
  bound to their row, and never returned (secrets are shown masked). A payment
  records the account it went through. Callbacks arrive at
  `/webhooks/{provider}/{accountID}`, so the account determines the tenant and
  the verification secret. Gateways are never called inside a database
  transaction: starting a payment validates first, calls the gateway with no
  transaction open, then records the intent after re-checking the invoice.
- **Canceling**: when an invoice is paid (or voided, or written off), every other
  pending payment on it is canceled with a `cancel_reason`. So are expired
  payments and ones canceled on request.
  - Providers that implement the optional `providers.Canceler` also get the
    intent closed at the gateway. This is queued on the payment
    (`provider_cancel = pending`) and sent by the worker **after** the
    transaction commits, never inside it.
  - Failed sends retry with backoff (1 min doubling, capped at 1 h, 8 attempts),
    then end as `failed`. `POST /payments/{id}/retry-provider-cancel` re-queues
    one.
  - Providers without `Canceler` (manual) are left alone.
- **Canceled is not final for money**: if a canceled payment turns out to be paid
  (the customer beat the cancel, or a manual transfer arrived after expiry), it is
  still settled. The money goes to credit when the invoice is no longer open.
  - A gateway that answers the cancel with `ErrAlreadyPaid` triggers `Check`, and
    the amount it reports is recorded this way.
  - Only `failed` payments cannot be settled.
- Refunds are full-payment only and leave the invoice paid.

## Subscriptions

- `incomplete → active`, `trialing → active`, `active ⇄ past_due`, and
  `→ canceled | expired`. At most **one live subscription per customer per
  plan** (partial unique index).
- **Entitled** (should have access) = `trialing`, `active` or `past_due`.
  `past_due` is the grace window.
- **No trial**: the first invoice is issued at once. The subscription is
  `incomplete` (no access) until it is paid; the first period starts **when the
  money arrives**. Unpaid by its due date (`incomplete_expiry_hours`) it expires
  and the invoice is voided.
- **Trial**: `trialing` immediately; the first invoice is raised like a renewal.
- **Free prices** (amount 0) and invoices fully covered by credit activate
  immediately.
- **Periods are anchored**: period *n* ends at `anchor + n × interval`, with month
  ends clamped. A customer who started on 31 January renews on 28 February, then
  31 March — not 28 March.
- **Renewal**: `renewal_lead_days` before the period ends, the engine raises the
  next period's invoice, due at the period end. A period is never invoiced twice
  (partial unique index).
  - Paying early changes nothing until the period ends.
  - Paying late (during grace) renews from the **old** period end, so a late
    payer gets no free days.
- **Lapse**: unpaid at period end → `past_due`. Still unpaid `grace_days` later
  → `expired`, and the unpaid invoice is voided (or written off, per
  `lapse_action`).
- **Cancel at period end**: any unpaid renewal is voided. The subscription ends
  (`canceled`) at the period end, and `resume` undoes it until then.
  **Cancel now** ends it immediately and voids its open invoices. Nothing is
  refunded automatically.
- **Plan changes**:
  - An increase on the same billing interval applies now. The difference for the
    rest of the period is invoiced, prorated by time and truncated in the
    customer's favour.
  - Anything else (a downgrade, or a different interval) is scheduled for the next
    renewal, which bills the new price.
  - Changing currency is refused.

## Seats and add-ons

- A subscription has **one base plan × quantity** (seats) and any number of
  **add-ons**. An add-on is a plan of kind `addon`, and its price × quantity is
  an item on the subscription. Every add-on must bill on exactly the base
  price's interval and currency, so everything renews on one invoice.
- **More applies now**: an upgrade, more seats, or an added or increased add-on.
  - The per-period difference for the rest of the current period is invoiced
    (prorated by time, truncated).
  - If the next period's invoice is already **paid**, the full difference for
    that period is invoiced too.
  - If it is raised but unpaid, it is voided and reissued at the new size.
- **Less applies later**: a downgrade, fewer seats, or a removed or decreased
  add-on. It is recorded as pending with `pending_from` = the start of the first
  period **not yet paid for**, and applied when the subscription rolls into that
  period. Nothing is refunded.
- An incomplete (unpaid) or cancel-at-period-end subscription cannot be
  changed; pay or resume first. Changing the base billing interval requires
  removing add-ons first.
- Entitlements list the base plan (`kind: base`, `quantity` = seats) and each
  active add-on (`kind: addon`).

## Coupons and promo codes

- A coupon is either `percent_off` (1–100) or `amount_off` + currency. Its code
  is case-insensitive (stored uppercase).
- Optional limits: `plan_ids` (discount only lines of those plans),
  `max_redemptions`, `redeem_by`. Inactive, expired, used-up and inapplicable
  codes are refused with a specific error code.
- Where it applies:
  - **On a subscription** (at creation, or `POST /subscriptions/{id}/discount`):
    `once` discounts the first period's invoice, `repeating` N periods, `forever`
    every period. Periods are counted from the live invoices that carried the
    discount, so voiding and reissuing an invoice never uses a period twice.
    Mid-period adjustment invoices are discounted only if the current period
    was.
  - **On a one-off invoice** (`promo_code` when creating it): that invoice only.
- Amount-off is capped at the eligible subtotal. A fully discounted invoice is
  paid at once.
- Discounts are booked to a `discounts` account, so revenue shows gross sales.

## Tax (optional)

- Off unless the tenant sets `settings.tax = {enabled, name, rate_bps,
  inclusive}` (`rate_bps` 1000 = 10%). One rate per invoice; no tax engine.
- A customer can be `tax_exempt`, or carry its own `tax_rate_bps`. That override
  applies even when tenant tax is off (for example, a customer in another
  country).
- Tax is computed at finalize on the **discounted** amount, rounded half-up once
  per invoice:
  - exclusive: `tax = base × rate`, `total = base + tax`;
  - inclusive: `tax = base − base / (1 + rate)`, `total = base`.
- Tax collected is credited to `tax_payable`. A void reverses it. A write-off or
  a refund does not; the tenant settles that with the tax authority.

## Events and webhooks

- Every state change writes an event in the same transaction (outbox). Each
  enabled endpoint that wants the event type gets a delivery.
- Deliveries are POSTed with `X-Billing-Signature: t=<unix>,v1=<hex
  HMAC-SHA256(secret, "<t>.<body>")>` and retried with exponential backoff
  (30 s … 6 h, up to `WEBHOOK_MAX_ATTEMPTS`) until a 2xx arrives. Delivery is
  **at least once**; receivers must be idempotent on the event id.
- **SSRF guard**: webhook URLs may only reach public addresses.
  - Refused: private, loopback, link-local/metadata, CGNAT/Tailscale, and
    reserved ranges.
  - The check runs on the IP actually dialled, after DNS, so DNS rebinding cannot
    bypass it. Redirects are not followed and HTTP proxies are ignored.
  - Registration also rejects internal names and addresses early.
  - `WEBHOOK_ALLOW_PRIVATE=true` disables the guard, for local development only.

## Engine

- `RunEngine` performs every time-driven transition, and only the ones that are
  due at the clock.
- Each item runs in its own transaction and re-checks its state under a row
  lock, so replicas and re-runs are safe.
- Order: expire payments → expire incomplete subscriptions → raise renewals →
  process period ends → lapse.
- `billing worker --now <RFC3339>` runs it once at a pretend time, for testing.
