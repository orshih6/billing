# CLAUDE.md

A standalone, multi-tenant billing service: tenants, API keys, customers, plans and
prices, subscriptions, invoices, payments (mock + manual for now) and a double-entry
ledger, with an embedded operator web UI. Any system that needs to charge for a SaaS
integrates over HTTP with an API key.

**Read `docs/billing-model.md` before changing anything that moves money.** It is
the source of truth; several rules are deliberate and not inferable from the code
(period starts at payment, late payers renew from the old period end, overpayment
becomes credit, refunds leave the invoice paid, …). `docs/api.md` is the
integrator-facing contract.

## Commands

```bash
make db-up              # Postgres 17 on localhost:5434 (docker compose)
make migrate            # schema (never runs on server start)
make run                # serve API + UI + worker, .env loaded (see .env.example)
make test               # unit only; DB tests skip
make test-integration   # full suite against Postgres (creates billing_test)
make check              # fmt + vet + full suite — run before finishing work
go test ./internal/billing -run TestLapseAfterGrace -v   # one test (needs TEST_DATABASE_URL)
./bin/billing worker --now 2026-12-01T00:00:00Z          # run the engine at a pretend time
BASE=… PLATFORM_KEY=… ./scripts/smoke.sh                 # end-to-end over HTTP
```

## Layout

- `cmd/billing` — one binary: `serve`, `worker`, `migrate`, `tenants`, `keys`, `version`.
- `internal/domain` — pure rules (money, currencies, anchored periods, proration,
  statuses). Imports nothing from this module.
- `internal/ledger` — **the only writer of ledger rows** (`Post`). Never insert into
  `ledger_transactions` / `ledger_entries` elsewhere.
- `internal/billing` — the business core. Every exported method takes the tenant id
  and scopes every query by it. `settleTx` is the only place money is recognised;
  `RunEngine` is every time-driven transition.
- `internal/api` — chi router; every route's scope is declared in `router.go`.
  Handlers only decode → call service → encode.
- `internal/web` — server-rendered UI (`templates/`, `static/` via `go:embed`). It
  calls the same service with the logged-in key's principal, so it can never do
  more than the key can.
- `internal/events` — outbox (`Emit` inside the state-change transaction) and the
  signed webhook dispatcher.
- `internal/wire` — **the public shape** of every API response and webhook
  payload. Models never go over the wire directly; `wire.Of` converts. Changing a
  type here changes the contract: update `client/` and `CHANGELOG.md`.
- `internal/database/migrations` — versioned SQL (goose), embedded. Every schema
  change is a new `000NN_name.sql` **and** a model change;
  `TestMigrationsMatchModels` fails if they drift. `legacy.go` (AutoMigrate) exists
  only to adopt pre-migration databases and for that test.
- `internal/netguard` — SSRF guard for outbound webhooks (checked at dial time).
- `internal/secretbox` — AES-GCM for provider credentials (`SECRETS_KEY`).
- `internal/providers` — `Provider` interface; `mock`, `manual`. New gateways
  implement it and register in `cmd/billing/main.go:newService`; each tenant
  connects its own account (`Fields` → encrypted `provider_accounts`). Guide:
  `docs/providers.md`.
  Extra capabilities are **optional interfaces** checked with a type assertion
  (`Canceler` today); a provider that lacks one is simply skipped. Never call a
  provider inside a DB transaction — queue the work on the row and let the worker
  do it (see `provider_cancel.go`).
- `client/` — public Go client with its own wire types; `client_test.go` is the
  contract test against the real router.

## Invariants

- Tenant comes from the API key, never the request body. A tenant key never sees
  another tenant (tests: `internal/api` `TestTenantIsolationAcrossRoutes`).
- Auth header is `X-API-Key` only. Routes are unversioned.
- Amounts are `int64` minor units; no floats, no FX.
- State change + ledger posting + event commit in **one** transaction.
- Anything that can race takes a row lock (`lock[T]`) and re-checks state.
- A unique violation inside a transaction aborts it in Postgres: check first under
  a lock, or use a savepoint (see `ledger.Post`).
- Time comes from `Service.Now` (truncated to µs), never `time.Now()` in billing
  logic — tests and `worker --now` depend on it.
- Templates run under a strict CSP: no inline `style=` or `<script>`.
- `GET /reports/ledger-reconciliation` must stay `ok: true`; every billing test
  ends by asserting it.

## Deploy

`deploy/kubernetes/` is a generic example; `docker-compose.yml --profile full` runs
everything locally. Production needs `APP_ENV=production`, `SECRETS_KEY`,
`UI_COOKIE_KEY` and a platform key. Run `billing migrate` (a Job) before each rollout.
