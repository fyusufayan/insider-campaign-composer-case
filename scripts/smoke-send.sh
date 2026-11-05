#!/usr/bin/env bash
# CP16 delivery smoke test of a running Compose stack, through the nginx
# frontend: create → interpret (demo) → save content → prepare → confirm →
# send, then wait for the worker to deliver through the mock provider and
# verify the result in PostgreSQL and in the mock provider's counters.
# No real message is sent (the mock provider records messages in memory).
#
#   LLM_PROVIDER=demo ANTHROPIC_API_KEY= docker compose up --build -d --wait
#   scripts/smoke-send.sh
#
# Requires curl, python3 and docker on the host. Creates one campaign. Safe
# to repeat: every check is scoped to the campaign it creates (the mock
# provider's durable ledger keeps earlier campaigns).
set -euo pipefail

BASE_URL="${BASE_URL:-http://localhost:${COMPOSER_HTTP_PORT:-8000}}"
BRIEF='Send a short and friendly 30% discount reminder to users in Istanbul who added products to their cart in the last 30 days but did not complete a purchase.'
TIMEOUT_S="${SMOKE_SEND_TIMEOUT_S:-180}"

fail() { echo "FAIL: $*" >&2; exit 1; }
ok() { echo "ok   $*"; }
json() { python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"; }
post() { curl -sS -o "$tmp" -w '%{http_code}' -X "$1" -H 'Content-Type: application/json' -d "$3" "$BASE_URL$2"; }
psql_q() { docker compose exec -T postgres psql -U "${POSTGRES_USER:-composer}" -d "${POSTGRES_DB:-composer}" -tA -c "$1"; }
tmp="$(mktemp)"; trap 'rm -f "$tmp"' EXIT

ready="$(curl -fsS "$BASE_URL/readyz")" || fail "/readyz not reachable via $BASE_URL"
[ "$(json 'd["llm_provider"]' <<<"$ready")" = demo ] || fail "smoke tests require the demo LLM provider: $ready"
ok "stack ready (demo LLM)"

body="$(python3 -c 'import json,sys; print(json.dumps({"name": sys.argv[1], "brief": sys.argv[2]}))' "smoke-send $(date -u +%Y%m%dT%H%M%SZ)" "$BRIEF")"
[ "$(post POST /api/v1/campaigns "$body")" = 201 ] || fail "create: $(cat "$tmp")"
id="$(json 'd["id"]' <"$tmp")"
[ "$(post POST "/api/v1/campaigns/$id/interpret" '{}')" = 200 ] || fail "interpret: $(cat "$tmp")"
version="$(json 'd["campaign"]["version"]' <"$tmp")"
channels="$(json '",".join(d["campaign"]["channels"])' <"$tmp")"
ok "campaign $id interpreted (channels: $channels)"

content="$(python3 - "$version" <<'PY'
import json, sys
print(json.dumps({"sms_body": "Hi {{first_name|there}}, your cart is waiting. Enjoy 30% off today.",
                  "email_subject": "{{first_name|Hello}}, your cart is waiting: 30% off",
                  "email_body": "Hi {{first_name|there}},\n\nYour cart is still waiting. Enjoy 30% off your order.\n\nSee you soon!",
                  "expected_version": int(sys.argv[1])}))
PY
)"
[ "$(post PUT "/api/v1/campaigns/$id/content" "$content")" = 200 ] || fail "save content: $(cat "$tmp")"
version="$(json 'd["campaign"]["version"]' <"$tmp")"

[ "$(post POST "/api/v1/campaigns/$id/approvals" "{\"expected_version\":$version}")" = 201 ] || fail "prepare: $(cat "$tmp")"
aid="$(json 'd["approval"]["approval_id"]' <"$tmp")"
version="$(json 'd["campaign"]["version"]' <"$tmp")"
snapshot="$(json 'd["approval"]["snapshot_count"]' <"$tmp")"
confirm="$(python3 -c 'import json,sys; a=json.load(open(sys.argv[1]))["approval"]; print(json.dumps({"expected_version": int(sys.argv[2]), "segment_version": a["segment_version"], "content_version": a["content_version"], "channels": a["channels"]}))' "$tmp" "$version")"
[ "$(post POST "/api/v1/campaigns/$id/approvals/$aid/confirm" "$confirm")" = 200 ] || fail "confirm: $(cat "$tmp")"
version="$(json 'd["campaign"]["version"]' <"$tmp")"
held="$(psql_q "SELECT count(*) FROM campaign_deliveries WHERE approval_id = '$aid' AND status = 'held'")"
[ "$held" = "$snapshot" ] || fail "held rows $held != snapshot $snapshot after confirm (confirm must not send)"
ok "approval $aid confirmed: $snapshot frozen rows, nothing released"

code="$(post POST "/api/v1/campaigns/$id/send" "{\"approval_id\":\"$aid\",\"expected_version\":$version}")"
[ "$code" = 202 ] || fail "send: $code $(cat "$tmp")"
python3 - "$tmp" "$snapshot" <<'PY' || fail "send response"
import json, sys
d = json.load(open(sys.argv[1]))
assert d["status"] == "sending" and not d["already_started"], d
assert sum(d["released"].values()) == int(sys.argv[2]), d
text = json.dumps(d)
for s in ("@example", "+90", "user_id", "destination", "idempotency_key", "vars"):
    assert s not in text, s
PY
code="$(post POST "/api/v1/campaigns/$id/send" "{\"approval_id\":\"$aid\",\"expected_version\":$version}")"
[ "$code" = 200 ] && [ "$(json 'd["already_started"]' <"$tmp")" = True ] || fail "repeat send: $code $(cat "$tmp")"
ok "send accepted (202), repeat is idempotent (200); response carries counts only"

deadline=$(( $(date +%s) + TIMEOUT_S ))
while :; do
	status="$(curl -fsS "$BASE_URL/api/v1/campaigns/$id" | json 'd["status"]')"
	case "$status" in completed|completed_with_failures) break ;; esac
	[ "$(date +%s)" -lt "$deadline" ] || fail "campaign still $status after ${TIMEOUT_S}s"
	sleep 1
done
ok "campaign finished: $status"

partition="$(psql_q "SELECT channel || ':' || status || '=' || count(*) FROM campaign_deliveries WHERE approval_id = '$aid' GROUP BY channel, status ORDER BY 1")"
echo "     partition: $(tr '\n' ' ' <<<"$partition")"
total="$(psql_q "SELECT count(*) FROM campaign_deliveries WHERE approval_id = '$aid'")"
open_rows="$(psql_q "SELECT count(*) FROM campaign_deliveries WHERE approval_id = '$aid' AND status IN ('held','pending','in_flight','retry_scheduled')")"
[ "$total" = "$snapshot" ] && [ "$open_rows" = 0 ] || fail "rows: total $total (snapshot $snapshot), open $open_rows"
bad_sent="$(psql_q "SELECT count(*) FROM campaign_deliveries WHERE approval_id = '$aid' AND status = 'sent' AND (destination IS NULL OR provider_message_id IS NULL)")"
[ "$bad_sent" = 0 ] || fail "sent rows without destination/message id: $bad_sent"
noattempt="$(psql_q "SELECT count(*) FROM campaign_deliveries d WHERE approval_id = '$aid' AND status IN ('sent','failed') AND last_error IS DISTINCT FROM 'missing_value' AND NOT EXISTS (SELECT 1 FROM delivery_attempts a WHERE a.delivery_id = d.id)")"
[ "$noattempt" = 0 ] || fail "$noattempt provider outcomes without an attempt row"
attempts="$(psql_q "SELECT outcome || '=' || count(*) FROM delivery_attempts a JOIN campaign_deliveries d ON d.id = a.delivery_id WHERE d.approval_id = '$aid' GROUP BY outcome ORDER BY 1")"
echo "     attempts: $(tr '\n' ' ' <<<"$attempts")"
ok "every row terminal; partition sums to the snapshot ($snapshot)"

# The mock's ledger is durable and keeps earlier campaigns: check THIS
# campaign by key prefix. Every sent row has exactly one ledger entry; an
# extra entry can only belong to a row that ended unsent after an ambiguous
# attempt (outcome_uncertain).
sent="$(psql_q "SELECT count(*) FROM campaign_deliveries WHERE approval_id = '$aid' AND status = 'sent'")"
uncertain="$(psql_q "SELECT count(*) FROM campaign_deliveries WHERE approval_id = '$aid' AND status <> 'sent' AND outcome_uncertain")"
stats="$(docker compose exec -T mockprovider wget -qO- "http://127.0.0.1:8090/admin/stats?key_prefix=$id:")" || fail "mock provider stats"
python3 - "$stats" "$sent" "$uncertain" <<'PY' || fail "mock provider stats: $stats"
import json, sys
d = json.loads(sys.argv[1]); sent = int(sys.argv[2]); uncertain = int(sys.argv[3])
assert d["durable"] and d["duplicate_accepted_keys"] == 0, d
ledger = sum(d["prefix_messages"].values())
assert sent <= ledger <= sent + uncertain, (ledger, sent, uncertain)
print(f"     ledger entries for this campaign: {ledger} (sent rows {sent}, uncertain unsent {uncertain})")
for ch, s in d["channels"].items():
    print(f"     mockprovider {ch} (this process): arrivals={s['arrivals']} accepted={s['accepted']} replayed={s['replayed']} "
          f"5xx={s['injected_5xx']} 429={s['injected_429']} timeouts={s['injected_timeouts']} storage_failures={s['storage_failures']}")
PY
ok "mock provider: durable ledger matches this campaign's sent rows; no duplicate accepted keys"

if docker compose logs --no-color worker mockprovider api 2>/dev/null | grep -Eq '[A-Za-z0-9._%+-]+@example\.|\+90[0-9]{10}|your cart is waiting'; then
	fail "logs contain a contact or message text"
fi
ok "no contacts or message text in api/worker/mockprovider logs"
echo "send smoke test passed ($BASE_URL)"
