# Contributing

Thanks for helping. Bug reports, docs fixes and payment providers are all
welcome.

## Before you start

- For anything bigger than a small fix, open an issue first so we can agree on
  the approach. Billing rules are subtle, and some behaviour that looks wrong is
  deliberate.
- Read [docs/billing-model.md](docs/billing-model.md) before changing anything
  that moves money. When code and that document disagree, one of them is a bug.

## Development

```bash
make db-up              # Postgres 17 on localhost:5434 (docker compose)
make migrate            # apply migrations
make run                # API + UI + worker on :8080 (uses .env)
make check              # gofmt, go vet, full test suite against Postgres; run before a PR
```

Database tests use `TEST_DATABASE_URL`; `make test-integration` sets it up.
Each test binary gets its own Postgres schema, so packages run in parallel.

## Rules the code relies on

- Every exported service method takes the tenant ID and scopes every query by it.
- `internal/ledger.Post` is the only writer of ledger rows. A state change, its
  ledger posting and its event commit in one transaction.
- Never call a payment provider inside a database transaction. Queue the work
  on the row and let the worker do it (see `provider_cancel.go`).
- Time comes from `Service.Now`, never `time.Now()`, in billing logic.
- **Schema changes** go in a new `internal/database/migrations/000NN_name.sql`
  (with `-- +goose Up` and `-- +goose Down`) *and* in the model.
  `TestMigrationsMatchModels` fails if the two disagree.
- **API shape** lives in `internal/wire`. Changing a field there changes the
  public contract. Keep `client/` in step (`client_test.go` checks it), and note
  it in `CHANGELOG.md`.
- UI templates run under a strict CSP: no inline `style=` or `<script>`.
- Every billing test ends by asserting that the ledger reconciles.

## Pull requests

- Keep them focused, with tests for the behaviour you change.
- `make check` must pass.
- Describe any user-visible change (API, webhook payload or UI) in the PR and in
  `CHANGELOG.md` under *Unreleased*.
