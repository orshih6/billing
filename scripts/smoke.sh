#!/usr/bin/env bash
# End-to-end walkthrough against a running server, using only curl + jq.
#
#   BASE=http://localhost:8080 PLATFORM_KEY=<platform admin key> ./scripts/smoke.sh
#
# It creates a throwaway tenant, so it is safe to run against any environment
# where the mock provider is allowed. With DATABASE_URL set it also time-travels
# the engine past the first period to exercise a renewal.
set -euo pipefail

BASE=${BASE:-http://localhost:8080}
: "${PLATFORM_KEY:?set PLATFORM_KEY to a platform admin key}"
command -v jq >/dev/null || { echo "jq is required"; exit 1; }

say() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
call() { # call METHOD PATH KEY [JSON]
  local method=$1 path=$2 key=$3 body=${4:-}
  local out status
  out=$(curl -sS -w '\n%{http_code}' -X "$method" "$BASE$path" -H "X-API-Key: $key" \
        -H 'Content-Type: application/json' ${body:+-d "$body"})
  status=${out##*$'\n'}; out=${out%$'\n'*}
  if [[ $status -ge 400 ]]; then echo "FAIL $method $path → $status: $out" >&2; exit 1; fi
  echo "$out"
}

slug="smoke-$(date +%s)"
say "create tenant $slug"
T=$(call POST /tenants "$PLATFORM_KEY" "{\"slug\":\"$slug\",\"name\":\"Smoke $slug\",\"create_admin_key\":true}")
KEY=$(jq -r .api_key.token <<<"$T")
echo "tenant $(jq -r .tenant.id <<<"$T")"

say "plan + customer"
call POST /plans "$KEY" '{"code":"pro","name":"Pro","prices":[{"amount":4900000,"interval":"month"}]}' | jq -c '{code, price: .prices[0].amount}'
C=$(call POST /customers "$KEY" '{"name":"Smoke User","external_id":"smoke-user-1","email":"smoke@example.com"}')
CID=$(jq -r .id <<<"$C")

say "subscribe (incomplete until paid)"
S=$(call POST /subscriptions "$KEY" "{\"customer_id\":\"$CID\",\"plan_code\":\"pro\"}")
SID=$(jq -r .id <<<"$S"); INV=$(jq -r .latest_invoice.id <<<"$S")
jq -c '{status, entitled, invoice: .latest_invoice.number, due: .latest_invoice.amount_due}' <<<"$S"

say "pay with the mock provider"
P=$(call POST "/invoices/$INV/payments" "$KEY" '{"provider":"mock","return_url":"https://example.com/return"}')
echo "checkout page: $(jq -r .pay_url <<<"$P")"
call POST "/payments/$(jq -r .id <<<"$P")/mock/succeed" "$KEY" | jq -c '{status, amount}'
call GET "/subscriptions/$SID" "$KEY" | jq -c '{status, entitled, current_period_end}'
call GET "/customers/by-external/smoke-user-1/entitlements" "$KEY" | jq -c '.entitlements'

say "one-off invoice, overpaid manually → credit"
I=$(call POST /invoices "$KEY" "{\"customer_id\":\"$CID\",\"finalize\":true,\"lines\":[{\"description\":\"Setup\",\"unit_amount\":1000000}]}")
call POST /payments/manual "$KEY" "{\"invoice_id\":\"$(jq -r .id <<<"$I")\",\"amount\":1500000,\"reference\":\"SMOKE-$slug\"}" | jq -c '{status, amount}'
call GET "/customers/$CID/balance" "$KEY" | jq -c .

if [[ -n "${DATABASE_URL:-}" ]]; then
  say "time-travel the engine to 3 days before renewal"
  END=$(call GET "/subscriptions/$SID" "$KEY" | jq -r .current_period_end)
  AT=$(python3 -c "import datetime,sys;e=datetime.datetime.fromisoformat(sys.argv[1].replace('Z','+00:00'));print((e-datetime.timedelta(days=3)).strftime('%Y-%m-%dT%H:%M:%SZ'))" "$END")
  go run ./cmd/billing worker --now "$AT"
  call GET "/invoices?subscription_id=$SID&status=open" "$KEY" | jq -c '[.data[] | {number, kind, amount_due, credit_applied}]'
fi

say "books"
call GET /reports/ledger-reconciliation "$KEY" | jq -c .
call GET /reports/stats "$KEY" | jq -c .
echo; echo "UI: $BASE/ui  (log in with: $KEY)"
