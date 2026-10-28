#!/usr/bin/env bash
# Slice 4b order-side drills (ADR-022, ADR-023). Requires: gateway/cart/catalog up,
# order-db up, $TOKEN set from an authenticated login drill. Rates are
# fetched LIVE — expectations are computed from the fetched rate.
set -euo pipefail

GW=http://localhost:9051
OUT=$(mktemp -d)
OUT=$(cygpath -m "$OUT" 2>/dev/null || echo "$OUT")

psql_db() { docker exec -i "$(docker compose ps -q order-db)" \
             psql -U order -d order_db "$@"; }

rows_with_key() {
  psql_db -tA -c "SELECT count(*) FROM orders WHERE idempotency_key='$1';"
}

snapshot_of() {
  psql_db -tA -c \
    "SELECT id,
            coalesce(charge_currency,''),
            coalesce(charge_amount_cents::text,''),
            coalesce(exchange_rate_used,''),
            coalesce(exchange_rate_source,''),
            (exchange_rate_fetched_at IS NOT NULL)
     FROM orders WHERE idempotency_key='$1';"
}

restart_order() {
  # empty string = no override; the env/.env value stays in effect
  local primary="${1:-}" fallback="${2:-}"
  if [ -n "$primary" ] && [ -n "$fallback" ]; then
    RATE_PRIMARY_URL="$primary" RATE_FALLBACK_URL="$fallback" \
      docker compose up -d order >/dev/null 2>&1
  elif [ -n "$primary" ]; then
    RATE_PRIMARY_URL="$primary" docker compose up -d order >/dev/null 2>&1
  else
    docker compose up -d order >/dev/null 2>&1
  fi
  sleep 3
}

fail_check() { echo "FAIL: $1"; exit 1; }

echo "== 1. display board serves independently of checkout =="
curl -s "$GW/rates" -o "$OUT/rates.json"
grep -q '"base"' "$OUT/rates.json" || fail_check "rates board missing base"

echo "== 2. fetch the rate the service will fetch (same source of truth) =="
RATE=$(curl -s "https://api.frankfurter.dev/v2/rate/EGP/USD" \
  | python -c 'import sys,json;print(json.load(sys.stdin)["rate"])')
[ -n "$RATE" ] || fail_check "could not fetch USD rate"
echo "USD rate today: $RATE"

echo "== 3. checkout in USD → snapshot == computed expectation =="
CODE=$(curl -s -o "$OUT/resp.json" -w '%{http_code}' -X POST "$GW/orders" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"idempotency_key":"c4b-conv","currency":"USD"}')
[ "$CODE" = 201 ] || fail_check "converted checkout got $CODE: $(cat "$OUT/resp.json")"

SNAP=$(snapshot_of c4b-conv)
echo "snapshot: $SNAP"
TOTAL=$(python -c 'import json;print(json.load(open("'$OUT'/resp.json"))["order"]["total_cents"])')
STORED_RATE=$(echo "$SNAP" | cut -d'|' -f4)
[ "$STORED_RATE" = "$RATE" ] || fail_check "stored rate $STORED_RATE != fetched $RATE"
[ "$(echo "$SNAP" | cut -d'|' -f2)" = "USD" ]       || fail_check "snapshot currency wrong: $SNAP"
[ "$(echo "$SNAP" | cut -d'|' -f5)" = "frankfurter" ] || fail_check "snapshot source wrong: $SNAP"
[ "$(echo "$SNAP" | cut -d'|' -f6)" = "t" ]         || fail_check "snapshot fetched_at missing: $SNAP"

EXPECTED=$(python - "$TOTAL" "$RATE" <<'EOF'
from decimal import Decimal, ROUND_HALF_UP
import sys
print((Decimal(sys.argv[1]) * Decimal(sys.argv[2])).quantize(
    Decimal("1"), ROUND_HALF_UP))
EOF
)
CHARGED=$(echo "$SNAP" | cut -d'|' -f3)
[ "$CHARGED" = "$EXPECTED" ] \
  || fail_check "charged $CHARGED but $TOTAL × $RATE (half-up) = $EXPECTED"
[ "$(rows_with_key c4b-conv)" = 1 ] || fail_check "more than one order row"
echo "charge math OK: $TOTAL × $RATE → $CHARGED cents"

echo "== 4. replay never re-quotes =="
CODE=$(curl -s -o "$OUT/resp2.json" -w '%{http_code}' -X POST "$GW/orders" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"idempotency_key":"c4b-conv","currency":"USD"}')
[ "$CODE" = 201 ] || fail_check "replay got $CODE"
[ "$(snapshot_of c4b-conv)" = "$SNAP" ] || fail_check "replay changed the snapshot"
[ "$(rows_with_key c4b-conv)" = 1 ] || fail_check "replay created rows"

echo "== 5. currency is part of the request's identity =="
CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$GW/orders" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"idempotency_key":"c4b-conv","currency":"SAR"}')
[ "$CODE" = 409 ] || fail_check "conflict got $CODE, want 409"

echo "== 6. door-check precedes any money =="
CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$GW/orders" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"idempotency_key":"c4b-jpy","currency":"JPY"}')
[ "$CODE" = 422 ] || fail_check "JPY got $CODE, want 422"
[ "$(rows_with_key c4b-jpy)" = 0 ] || fail_check "JPY leaked a row"

echo "== 7. fallback takeover (ADR-023): primary dead → the second source answers =="
restart_order "http://127.0.0.1:9" ""    # primary dead; fallback stays REAL
FB_RATE=$(curl -s "https://open.er-api.com/v6/latest/EGP" \
  | python -c 'import sys,json;print(json.load(sys.stdin)["rates"]["USD"])')
[ -n "$FB_RATE" ] || fail_check "could not fetch er-api rate"
CODE=$(curl -s -o "$OUT/fb.json" -w '%{http_code}' -X POST "$GW/orders" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"idempotency_key":"c4b-fb","currency":"USD"}')
[ "$CODE" = 201 ] || fail_check "takeover checkout got $CODE"
FB_SNAP=$(snapshot_of c4b-fb)
FB_TOTAL=$(python -c 'import json;print(json.load(open("'$OUT'/fb.json"))["order"]["total_cents"])')
echo "fallback snapshot: $FB_SNAP"
[ "$(echo "$FB_SNAP" | cut -d'|' -f2)" = "USD" ]  || fail_check "snapshot currency wrong: $FB_SNAP"
[ "$(echo "$FB_SNAP" | cut -d'|' -f5)" = "er-api" ] || fail_check "provenance lost — the row must name the answering source: $FB_SNAP"
[ "$(echo "$FB_SNAP" | cut -d'|' -f6)" = "t" ]    || fail_check "snapshot fetched_at missing: $FB_SNAP"
FB_EXPECTED=$(python - "$FB_TOTAL" "$FB_RATE" <<'EOF'
from decimal import Decimal, ROUND_HALF_UP
import sys
print((Decimal(sys.argv[1]) * Decimal(sys.argv[2])).quantize(
    Decimal("1"), ROUND_HALF_UP))
EOF
)
FB_CHARGED=$(echo "$FB_SNAP" | cut -d'|' -f3)
[ "$FB_CHARGED" = "$FB_EXPECTED" ] \
  || fail_check "takeover charge $FB_CHARGED, but $FB_TOTAL × $FB_RATE (half-up) = $FB_EXPECTED"
echo "takeover OK: er-api answered, the math stayed exact"

echo "== 8. full outage: EVERY source dead → refuse; base path untouched =="
restart_order "http://127.0.0.1:9" "http://127.0.0.1:9"   # both URLs dead
CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$GW/orders" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"idempotency_key":"c4b-down","currency":"USD"}')
[ "$CODE" = 503 ] || fail_check "outage checkout got $CODE, want 503"
[ "$(rows_with_key c4b-down)" = 0 ] || fail_check "refused checkout leaked rows"
CODE=$(curl -s -o "$OUT/resp3.json" -w '%{http_code}' -X POST "$GW/orders" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"idempotency_key":"c4b-base","currency":"EGP"}')
[ "$CODE" = 201 ] || fail_check "base checkout got $CODE during outage"
[ "$(psql_db -tA -c "SELECT (charge_currency IS NULL AND charge_amount_cents \
    IS NULL) FROM orders WHERE idempotency_key='c4b-base';")" = "t" ] \
  || fail_check "base checkout wrote a snapshot"
echo "outage story OK: USD refused, EGP fine"

echo "== 9. snapshot round-trips through GET as integers =="
restart_order                            # back to the real providers
OID=$(psql_db -tA -c "SELECT id FROM orders WHERE idempotency_key='c4b-conv';")
CODE=$(curl -s -o "$OUT/order.json" -w '%{http_code}' "$GW/orders/$OID" \
  -H "Authorization: Bearer $TOKEN")
[ "$CODE" = 200 ] || fail_check "order GET got $CODE"
grep -q '"charge_currency":"USD"' "$OUT/order.json" || fail_check "no charge_currency"
grep -q "\"charge_amount_cents\":$CHARGED" "$OUT/order.json" \
  || fail_check "charge not the stored integer"
if grep -Eq '"charge_amount_cents":[0-9]+\.[0-9]' "$OUT/order.json"; then
  fail_check "float text on the wire — never allowed"
fi

echo "== 10. cleanup drill keys (leave the dev DB inspection-clean) =="
psql_db -q -c "DELETE FROM order_items WHERE order_id IN
  (SELECT id FROM orders WHERE idempotency_key LIKE 'c4b-%');"
psql_db -q -c "DELETE FROM orders WHERE idempotency_key LIKE 'c4b-%';"

restart_order   # leave the stack on the real providers
echo "ALL SLICE-4B ORDER DRILLS PASSED"
