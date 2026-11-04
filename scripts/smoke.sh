#!/usr/bin/env bash
# HTTP-level smoke test of a running Compose stack (Slice 1a), through the
# nginx frontend exactly as a browser uses it. Creates one campaign named
# "smoke <timestamp>"; it changes no other data.
#
#   docker compose up --build -d --wait && scripts/smoke.sh
#
# Requires curl and python3 on the host. BASE_URL defaults to http://localhost:8000.
set -euo pipefail

BASE_URL="${BASE_URL:-http://localhost:${COMPOSER_HTTP_PORT:-8000}}"
BRIEF='Send a short and friendly 30% discount reminder to users in Istanbul who added products to their cart in the last 30 days but did not complete a purchase.'

fail() { echo "FAIL: $*" >&2; exit 1; }
ok() { echo "ok   $*"; }

# json <python expression over `d`> < body
json() { python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"; }

# 1. readiness through nginx, demo provider without a key
ready="$(curl -fsS "$BASE_URL/readyz")" || fail "/readyz not reachable via $BASE_URL"
[ "$(json 'd["status"]' <<<"$ready")" = ready ] || fail "/readyz: $ready"
[ "$(json 'd["migration_dirty"]' <<<"$ready")" = False ] || fail "schema dirty: $ready"
ok "/readyz ready, migration_version=$(json 'd["migration_version"]' <<<"$ready"), llm_provider=$(json 'd["llm_provider"]' <<<"$ready")"

# 2. SPA shell and deep-link fallback
for path in / /campaigns/new /campaigns/00000000-0000-0000-0000-000000000000; do
	ct="$(curl -fsS -o /dev/null -w '%{content_type}' "$BASE_URL$path")" || fail "GET $path"
	[[ "$ct" == text/html* ]] || fail "GET $path: content type $ct"
done
ok "SPA served for / and client-side routes"

# 3. API errors pass through with the JSON envelope and status
code="$(curl -sS -o /dev/null -w '%{http_code}' "$BASE_URL/api/v1/campaigns/00000000-0000-0000-0000-000000000000")"
[ "$code" = 404 ] || fail "unknown campaign: status $code, want 404"
ok "API 404 passes through"

# 4. create → interpret (demo) → audience
body="$(python3 -c 'import json,sys; print(json.dumps({"name": sys.argv[1], "brief": sys.argv[2]}))' "smoke $(date -u +%Y%m%dT%H%M%SZ)" "$BRIEF")"
created="$(curl -fsS -X POST -H 'Content-Type: application/json' -d "$body" "$BASE_URL/api/v1/campaigns")" || fail "create campaign"
id="$(json 'd["id"]' <<<"$created")"
ok "campaign created: $id"

interp="$(curl -fsS -X POST -H 'Content-Type: application/json' -d '{}' "$BASE_URL/api/v1/campaigns/$id/interpret")" || fail "interpret"
matched="$(json 'd["audience"]["matched_users"]' <<<"$interp")"
[ "$matched" -gt 0 ] || fail "interpretation matched no users: $interp"
python3 - "$interp" <<'PY' || fail "audience partition identities"
import json, sys
d = json.loads(sys.argv[1])
a = d["audience"]
for ch, s in a["by_channel"].items():
    assert s["eligible"] + s["no_consent"] + s["no_contact"] == a["matched_users"], (ch, s)
r = a["reach_users"]
assert r["both"] + r["sms_only"] + r["email_only"] + r["unreachable"] == a["matched_users"], r
PY
ok "interpreted (demo): matched_users=$matched; per-channel partitions sum to matched_users"

# 5. no channels → zero messages, matched users unchanged
version="$(json 'd["campaign"]["version"]' <<<"$interp")"
curl -fsS -o /dev/null -X PUT -H 'Content-Type: application/json' \
	-d "{\"channels\":[],\"expected_version\":$version}" "$BASE_URL/api/v1/campaigns/$id/channels" || fail "PUT channels"
aud="$(curl -fsS "$BASE_URL/api/v1/campaigns/$id/audience")" || fail "GET audience"
[ "$(json 'd["audience"]["messages"]' <<<"$aud")" = 0 ] || fail "messages != 0 with no channels: $aud"
[ "$(json 'd["audience"]["matched_users"]' <<<"$aud")" = "$matched" ] || fail "matched users changed: $aud"
ok "no channels: 0 messages, matched_users still $matched"

# 6. persisted state is readable again
[ "$(curl -fsS "$BASE_URL/api/v1/campaigns/$id" | json 'd["channels"]')" = "[]" ] || fail "channels not persisted"
ok "campaign state persisted"

echo "smoke test passed ($BASE_URL)"
