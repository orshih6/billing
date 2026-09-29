# Integrating with billing

Base URL: wherever you run it, e.g. `https://billing.example.com` (local: `http://localhost:8080`).
Go services should use the typed client in `github.com/orshih6/billing/client`.

## Conventions

- **Auth**: every request sends `X-API-Key: bk_…`. `Authorization` is not read.
  `401` codes are `unauthorized`, `key_expired` and `key_revoked`. A missing scope
  is `403 insufficient_scope`.
- **Tenant**: comes from the key. Platform keys add `X-Tenant-Id: <uuid>`.
- **Routes are unversioned**: `/customers`, not `/v1/customers`.
- **Money**: integer minor units plus a 3-letter `currency`. `4900000` MNT is
  49,000.00 MNT.
- **Errors**: `{"error":{"code":"invoice_not_open","message":"…"}}`. Switch on
  `code`; `message` is for humans.
- **Retries**: send `Idempotency-Key: <unique>` on any POST. A repeat with the
  same body returns the stored response (header `Idempotent-Replayed: true`). The
  same key with a different body is `422 idempotency_key_reused`.
- **Objects**: every object has an `"object"` field naming its type
  (`"invoice"`, `"subscription"`, …). Webhook payloads use exactly the same
  shapes.
- **Lists**: `?limit=` (max 100) and `?starting_after=<last id>`. The response is
  `{"object":"list","data":[…],"has_more":bool}`, newest first. Every list also
  accepts `created_after` / `created_before` (a date or RFC 3339 time).
  Customers, invoices and subscriptions accept `metadata[key]=value`.
- **Search**: `GET /search?q=` looks across customers (name, email, phone,
  external id), invoices (number, reference), payments (provider reference) and
  subscriptions (plan, customer). Pasting any id finds that object and its
  children. Each group appears only if the key can read it.
- **Unknown JSON fields are rejected** (`400 invalid_json`), so typos surface.

## The usual SaaS integration

```bash
H='-H "X-API-Key: $KEY" -H "Content-Type: application/json"'

# once: a plan (or create it in the UI)
POST /plans {"code":"pro","name":"Pro","prices":[{"amount":4900000,"interval":"month","trial_days":0}]}

# on sign-up / first checkout: a customer keyed by YOUR user id
POST /customers {"external_id":"user-42","name":"Bold","email":"b@example.com"}
#   409 external_id_taken → GET /customers/by-external/user-42

# checkout
POST /subscriptions {"customer_id":"…","plan_code":"pro"}
#   → status "incomplete", latest_invoice.id
POST /invoices/{latest_invoice.id}/payments {"provider":"mock","return_url":"https://app/after-pay"}
#   → pay_url: redirect the user there; they return to return_url?payment_id=…&status=succeeded

# gate features (cache briefly, or react to webhooks)
GET /customers/by-external/user-42/entitlements
#   → {"entitlements":[{"plan_code":"pro","status":"active","current_period_end":"…"}]}
```

The customer has access while a subscription is `trialing`, `active` or
`past_due`. The entitlements endpoint only returns those.

Manual payments: start with `{"provider":"manual"}` to get transfer
instructions that include the invoice `reference`. An operator later records the
money, either from the UI or with `POST /payments/manual`:
`{"invoice_id","amount","reference":"<bank statement id>"}`.

## Endpoints

| Method & path | Scope | Notes |
|---|---|---|
| `GET /me` | any | key, scopes, tenant |
| `GET /scopes`, `GET /event-types` | any | |
| `GET/POST /tenants`, `GET/PATCH /tenants/{id}` | platform admin | `create_admin_key: true` returns a key once |
| `GET/POST /tenants/{id}/api-keys` | platform admin | |
| `POST /engine/run` | platform admin | run due renewals/lapses now |
| `GET /tenant`, `PATCH /tenant` | any / admin | settings, effective settings |
| `GET /search?q=` | any read scope | grouped results; see above |
| `GET /payment-providers` | payments:read | providers, the credentials they need, and the tenant's connection state (secrets masked) |
| `PUT /payment-providers/{name}` | admin | `{"mode":"test"/"live","enabled":true,"config":{…}}`; blank secrets keep the stored value |
| `DELETE /payment-providers/{name}` | admin | disconnect |
| `GET/POST /api-keys`, `POST /api-keys/{id}/revoke` | keys:read / keys:write | can't grant scopes you lack |
| `GET/POST /customers` | customers:read / write | `?q=` name, email, phone, external id |
| `GET/PATCH /customers/{id}` | customers:read / write | `tax_exempt`, `tax_rate_bps` (−1 clears) |
| `GET /customers/by-external/{externalId}` | customers:read | |
| `GET /customers/{id}/entitlements`, `…/by-external/{externalId}/entitlements` | customers:read | |
| `GET /customers/{id}/balance` | customers:read | owed + credit per currency |
| `POST /customers/{id}/credits` | customers:write | `{amount,currency,reason}` |
| `GET/POST /plans`, `GET/PATCH /plans/{id}`, `GET /plans/by-code/{code}` | plans:read / write | `kind`: `base` or `addon` |
| `POST /plans/{id}/prices` | plans:write | prices are immutable versions |
| `GET /prices/{id}`, `POST /prices/{id}/activate` / `/deactivate` | plans:* | |
| `GET/POST /subscriptions`, `GET /subscriptions/{id}` | subscriptions:* | `price_id` or `plan_code`, `quantity` (seats), `add_ons:[{price_id,quantity}]`, `promo_code`, `trial_days` |
| `POST /subscriptions/{id}/quantity` | subscriptions:write | `{"quantity":5}`: more now (prorated), fewer at renewal |
| `POST /subscriptions/{id}/add-ons` | subscriptions:write | `{"price_id","quantity"}`, quantity 0 removes it at renewal |
| `POST/DELETE /subscriptions/{id}/discount` | subscriptions:write | `{"promo_code":"SPRING"}` |
| `GET/POST /coupons`, `GET/PATCH /coupons/{id}` | coupons:read / write | percent or amount off; once/repeating/forever; plan_ids, max_redemptions, redeem_by |
| `GET /coupons/check?code=&currency=` | coupons:read | validate a promo code for checkout without redeeming |
| `POST /subscriptions/{id}/cancel` | subscriptions:write | `{"at_period_end":true}` (default) |
| `POST /subscriptions/{id}/resume` | subscriptions:write | |
| `POST /subscriptions/{id}/change-price` | subscriptions:write | `{"price_id","when":"now"/"period_end"/""}` |
| `GET/POST /invoices`, `GET /invoices/{id}` | invoices:* | one-off: `{customer_id,lines:[…],finalize,promo_code}`; totals: `subtotal`, `discount_total`, `tax_total`, `total` |
| `POST /invoices/{id}/lines`, `/finalize`, `/void`, `/mark-uncollectible` | invoices:write | |
| `GET/POST /invoices/{id}/payments` | payments:read / write | `{"provider":"mock"/"manual","return_url"}` |
| `GET /payments`, `GET /payments/{id}` | payments:read | `?q=` provider reference or note, `status`, `provider` |
| `POST /payments/manual` | payments:manual | record money received |
| `POST /payments/{id}/confirm` | payments:manual | settle a pending manual payment |
| `POST /payments/{id}/mock/{succeed/fail}` | payments:write | |
| `POST /payments/{id}/cancel`, `/refund` | payments:write | cancel also closes the gateway intent if the provider supports it |
| `POST /payments/{id}/retry-provider-cancel` | payments:write | re-queue a gateway cancel that ended `failed` |
| `GET /ledger/accounts[/{id}[/entries]]`, `GET /ledger/transactions` | ledger:read | |
| `GET /reports/ledger-reconciliation` | ledger:read | must be `ok: true` |
| `GET /reports/stats` | invoices:read | MRR, outstanding, collected |
| `GET/POST /webhook-endpoints`, `PATCH/DELETE /webhook-endpoints/{id}` | webhooks:* | secret shown once |
| `GET /webhook-endpoints/{id}/deliveries`, `POST /webhook-deliveries/{id}/retry` | webhooks:* | |
| `GET /events?type=` | webhooks:read | |
| `POST /webhooks/{provider}/{accountID}` | none | inbound gateway callbacks; the URL is shown per connected account |
| `GET /healthz`, `GET /readyz` | none | |

## Webhooks

Register an endpoint (`POST /webhook-endpoints {"url","event_types":[…]}`) and
store the returned `secret`. Each delivery is:

```http
POST <your url>
X-Billing-Event: invoice.paid
X-Billing-Event-Id: <uuid>
X-Billing-Signature: t=1790000000,v1=<hex hmac-sha256(secret, "t.body")>

{"id":"…","type":"invoice.paid","tenant_id":"…","created_at":"…","data":{…the invoice…}}
```

To verify a delivery, compute the HMAC over the **raw** body and reject stale
timestamps. In Go, `client.VerifyWebhook(body, header, secret, 5*time.Minute)`
does both. Delivery is at least once, so deduplicate on the event `id`.

Event types: `customer.created|updated|credit_granted`,
`subscription.created|activated|renewed|updated|past_due|canceled|expired`,
`invoice.created|finalized|paid|partially_paid|voided|uncollectible`,
`payment.created|succeeded|failed|canceled|refunded`.
