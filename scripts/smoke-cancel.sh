#!/usr/bin/env bash
# CP17 cancellation smoke test of a running Compose stack, through the nginx
# frontend: (1) cancel an approved campaign before sending — its approval is
# revoked and the frozen rows deleted; (2) send another campaign and cancel
# it mid-send — queued rows are cancelled at once, rows in flight finish
# truthfully, the campaign stays cancelled and every row ends terminal.
# No real message is sent.
#
#   LLM_PROVIDER=demo ANTHROPIC_API_KEY= docker compose up --build -d --wait
#   scripts/smoke-cancel.sh
#
# Requires curl, python3 and docker on the host. Creates two campaigns.
set -euo pipefail

BASE_URL="${BASE_URL:-http://localhost:${COMPOSER_HTTP_PORT:-8000}}"
BRIEF='Send a short and friendly 30% discount reminder to users in Istanbul who added products to their cart in the last 30 days but did not complete a purchase.'
TIMEOUT_S="${SMOKE_CANCEL_TIMEOUT_S:-120}"

fail() { echo "FAIL: $*" >&2; exit 1; }
ok() { echo "ok   $*"; }
json() { python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"; }
post() { curl -sS -o "$tmp" -w '%{http_code}' -X "$1" -H 'Content-Type: application/json' -d "$3" "$BASE_URL$2"; }
psql_q() { docker compose exec -T postgres psql -U "${POSTGRES_USER:-composer}" -d "${POSTGRES_DB:-composer}" -tA -c "$1"; }
tmp="$(mktemp)"; trap 'rm -f "$tmp"' EXIT

ready="$(curl -fsS "$BASE_URL/readyz")" || fail "/readyz not reachable via $BASE_URL"
[ "$(json 'd["llm_provider"]' <<<"$ready")" = demo ] || fail "smoke tests require the demo LLM provider: $ready"

# approved_campaign NAME → sets id, aid, version, snapshot (approved campaign).
approved_campaign() {
	local body content confirm
	body="$(python3 -c 'import json,sys; print(json.dumps({"name": sys.argv[1], "brief": sys.argv[2]}))' "$1 $(date -u +%Y%m%dT%H%M%SZ)" "$BRIEF")"
	[ "$(post POST /api/v1/campaigns "$body")" = 201 ] || fail "create: $(cat "$tmp")"
	id="$(json 'd["id"]' <"$tmp")"
	[ "$(post POST "/api/v1/campaigns/$id/interpret" '{}')" = 200 ] || fail "interpret: $(cat "$tmp")"
	version="$(json 'd["campaign"]["version"]' <"$tmp")"
	content="$(python3 -c 'import json,sys; print(json.dumps({"sms_body": "Hi {{first_name|there}}, your cart is waiting. Enjoy 30% off today.", "email_subject": "{{first_name|Hello}}, your cart is waiting: 30% off", "email_body": "Hi {{first_name|there}},\n\nYour cart is still waiting. Enjoy 30% off your order.\n\nSee you soon!", "expected_version": int(sys.argv[1])}))' "$version")"
	[ "$(post PUT "/api/v1/campaigns/$id/content" "$content")" = 200 ] || fail "save content: $(cat "$tmp")"
	version="$(json 'd["campaign"]["version"]' <"$tmp")"
	[ "$(post POST "/api/v1/campaigns/$id/approvals" "{\"expected_version\":$version}")" = 201 ] || fail "prepare: $(cat "$tmp")"
	aid="$(json 'd["approval"]["approval_id"]' <"$tmp")"
	version="$(json 'd["campaign"]["version"]' <"$tmp")"
	snapshot="$(json 'd["approval"]["snapshot_count"]' <"$tmp")"
	confirm="$(python3 -c 'import json,sys; a=json.load(open(sys.argv[1]))["approval"]; print(json.dumps({"expected_version": int(sys.argv[2]), "segment_version": a["segment_version"], "content_version": a["content_version"], "channels": a["channels"]}))' "$tmp" "$version")"
	[ "$(post POST "/api/v1/campaigns/$id/approvals/$aid/confirm" "$confirm")" = 200 ] || fail "confirm: $(cat "$tmp")"
	version="$(json 'd["campaign"]["version"]' <"$tmp")"
}

# (1) Cancel before sending.
approved_campaign smoke-cancel-before
# (Arithmetic is hoisted: bash 3.2 mis-parses $((…)) nested in "$(… "…")".)
stale=$((version - 1)); next=$((version + 1))
[ "$(post POST "/api/v1/campaigns/$id/cancel" "{\"expected_version\":$stale}")" = 409 ] || fail "stale cancel: $(cat "$tmp")"
[ "$(post POST "/api/v1/campaigns/$id/cancel" "{\"expected_version\":$version}")" = 200 ] || fail "cancel: $(cat "$tmp")"
python3 - "$tmp" "$aid" "$snapshot" <<'PY' || fail "cancel response: $(cat "$tmp")"
import json, sys
d = json.load(open(sys.argv[1]))
assert d["status"] == "cancelled" and d["previous_status"] == "approved", d
assert d["revoked_approval_id"] == sys.argv[2] and d["released_held_rows"] == int(sys.argv[3]), d
assert d["campaign"]["active_approval_id"] is None, d
PY
[ "$(psql_q "SELECT count(*) FROM campaign_deliveries WHERE approval_id = '$aid'")" = 0 ] || fail "held rows left after cancel"
[ "$(psql_q "SELECT status || ':' || revoke_reason FROM campaign_approvals WHERE id = '$aid'")" = "revoked:campaign_cancelled" ] || fail "approval not revoked"
[ "$(post POST "/api/v1/campaigns/$id/cancel" "{\"expected_version\":$next}")" = 409 ] && [ "$(json 'd["error"]["code"]' <"$tmp")" = invalid_state ] || fail "repeat cancel: $(cat "$tmp")"
send_body="{\"approval_id\":\"$aid\",\"expected_version\":$next}"
code="$(post POST "/api/v1/campaigns/$id/send" "$send_body")"
[ "$code" = 409 ] || fail "send after cancel: $code $(cat "$tmp")"
ok "cancel before sending: approval revoked, $snapshot frozen rows deleted; repeat and send refused (409)"

# (2) Cancel mid-send.
approved_campaign smoke-cancel-during
send_body="{\"approval_id\":\"$aid\",\"expected_version\":$version}"
code="$(post POST "/api/v1/campaigns/$id/send" "$send_body")"
[ "$code" = 202 ] || fail "send: $code $(cat "$tmp")"
version="$(json 'd["campaign"]["version"]' <"$tmp")"
# Cancel once delivery is under way (the first rows were sent).
deadline=$(( $(date +%s) + 30 ))
while [ "$(psql_q "SELECT count(*) FROM campaign_deliveries WHERE approval_id = '$aid' AND status = 'sent'")" = 0 ]; do
	[ "$(date +%s)" -lt "$deadline" ] || fail "nothing sent within 30s"
	sleep 0.1
done
[ "$(post POST "/api/v1/campaigns/$id/cancel" "{\"expected_version\":$version}")" = 200 ] || fail "cancel while sending: $(cat "$tmp")"
inflight="$(json 'sum(d["in_flight"].values())' <"$tmp")"
cancelled="$(json 'sum(d["cancelled_deliveries"].values())' <"$tmp")"
[ "$(json 'd["previous_status"]' <"$tmp")" = sending ] || fail "previous status: $(cat "$tmp")"
queued="$(psql_q "SELECT count(*) FROM campaign_deliveries WHERE approval_id = '$aid' AND status IN ('pending','retry_scheduled')")"
[ "$queued" = 0 ] || fail "$queued queued rows after cancel"
ok "cancel while sending: $cancelled queued rows cancelled at once, $inflight in flight (at most one request each)"
deadline=$(( $(date +%s) + TIMEOUT_S ))
while [ "$(psql_q "SELECT count(*) FROM campaign_deliveries WHERE approval_id = '$aid' AND status = 'in_flight'")" != 0 ]; do
	[ "$(date +%s)" -lt "$deadline" ] || fail "rows still in flight after ${TIMEOUT_S}s"
	sleep 1
done
partition="$(psql_q "SELECT status || '=' || count(*) FROM campaign_deliveries WHERE approval_id = '$aid' GROUP BY status ORDER BY 1")"
echo "     partition: $(tr '\n' ' ' <<<"$partition")"
total="$(psql_q "SELECT count(*) FROM campaign_deliveries WHERE approval_id = '$aid'")"
open_rows="$(psql_q "SELECT count(*) FROM campaign_deliveries WHERE approval_id = '$aid' AND status IN ('held','pending','in_flight','retry_scheduled')")"
[ "$total" = "$snapshot" ] && [ "$open_rows" = 0 ] || fail "rows: total $total (snapshot $snapshot), open $open_rows"
[ "$(curl -fsS "$BASE_URL/api/v1/campaigns/$id" | json 'd["status"]')" = cancelled ] || fail "campaign not cancelled"
sent_after="$(psql_q "SELECT count(*) FROM campaign_deliveries WHERE approval_id = '$aid' AND status = 'sent'")"
ok "every row terminal; the campaign stays cancelled ($sent_after sent before or by rows in flight)"
echo "cancel smoke test passed ($BASE_URL)"
