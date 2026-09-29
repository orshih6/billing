# Security

## Reporting a vulnerability

Please **do not open a public issue** for a security problem. Report it
privately through GitHub's
[private vulnerability reporting](https://github.com/orshih6/billing/security/advisories/new).
Include what you found, how to reproduce it, and the impact you expect.

You should get a first reply within a few days. Fixes are released as a patch
version, and the advisory is published once users have had time to upgrade.

## Supported versions

Only the latest minor release receives security fixes while the project is
pre-1.0.

## What the service defends, and how

These are the properties we treat as security bugs if broken.

- **Tenant isolation.**
  - The tenant always comes from the API key, never from the request.
  - A tenant key can never read or change another tenant's data.
  - A test walks every route to check this (`TestTenantIsolationAcrossRoutes`).
- **API keys.**
  - 256-bit random tokens; only a SHA-256 hash is stored, and each token is shown
    once.
  - Keys are scoped, expirable and revocable.
  - A key cannot mint a key with scopes it does not hold.
- **Provider credentials.**
  - Encrypted with AES-256-GCM under `SECRETS_KEY`, and bound to their row.
  - Secret values are write-only through the API and UI.
- **Webhooks out.**
  - Signed with HMAC-SHA256 and a timestamp.
  - Tenant-supplied URLs cannot reach private, loopback, link-local (cloud
    metadata) or CGNAT addresses. The check runs on the resolved IP at connect
    time.
  - Redirects are not followed.
- **Webhooks in.** Never trusted on their own. The body only identifies the
  payment, and the gateway is asked for the truth.
- **Money.**
  - Integer minor units only, and every ledger posting must balance.
  - Ledger entries are append-only, enforced by a database trigger.
  - Settlement is idempotent.
- **Web UI.**
  - The session cookie is encrypted, HttpOnly and SameSite=Strict.
  - Forms carry a CSRF token.
  - A strict Content-Security-Policy is set, and framing is denied.

## Operating it safely

- Set `APP_ENV=production`. The server then refuses to start without a platform
  key, `SECRETS_KEY` and `UI_COOKIE_KEY`. It also turns off the mock provider
  unless a tenant opts in.
- Back up `SECRETS_KEY` together with your database credentials. Without it,
  stored provider credentials cannot be decrypted.
- Serve it behind TLS and set `PUBLIC_URL` to the `https://` address.
- Never set `WEBHOOK_ALLOW_PRIVATE=true` outside local development.
- Prefer database keys with narrow scopes over `BOOTSTRAP_API_KEYS`.
