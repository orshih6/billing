# Changelog

All notable changes are listed here. The project follows
[Semantic Versioning](https://semver.org). Before 1.0, a minor version may
change the API; such changes are called out under **Breaking**.

## Unreleased

## 0.1.1

- Container images are now built for `linux/amd64` and `linux/arm64`, and are
  also tagged with a `v` prefix (`ghcr.io/orshih6/billing:v0.1.1`), matching the
  docs. No code changes.

## 0.1.0

First public release.

- Multi-tenant service with platform keys and scoped, per-tenant API keys
  (`X-API-Key`).
- Customers; plans with immutable prices, trials, **add-ons** and **seats**.
- Subscriptions:
  - anchored billing periods, renewal invoices raised in advance, then grace
    and lapse;
  - cancel now or at period end;
  - prorated upgrades and scheduled downgrades.
- Coupons and promo codes: percent or amount off; once, repeating or forever;
  plan, usage and expiry limits.
- Optional tax: one rate per tenant, inclusive or exclusive, with per-customer
  exemptions and overrides.
- Invoices: finalize, void, write-off, automatic customer credit.
- Payments:
  - `mock` and `manual` providers;
  - per-tenant provider accounts with credentials encrypted at rest;
  - automatic cancellation of superseded gateway intents;
  - verified inbound webhooks.
- Double-entry, append-only ledger with a reconciliation report.
- Outbound webhooks: signed with HMAC and retried, with SSRF protection.
- Search (`GET /search`); `q`, date-range and metadata filters on lists.
- `Idempotency-Key` on every POST.
- Plain-language operator web UI, embedded in the binary.
- Versioned SQL migrations (`billing migrate up|down|status`).
- Go client: `github.com/orshih6/billing/client`.
