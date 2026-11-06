# Final acceptance checklist (CP18)

Verified on 2026-11-06 against `e458687` (CP17B code `1d39f91` + session log)
plus the CP18 working-tree changes listed at the end. Host: macOS (Apple
Silicon), Docker Desktop with Compose v5.1.4, Go 1.27.3 toolchain (module
`go 1.25.0`; images build with `golang:1.25-alpine`), Node 22.20. Demo LLM
only; no Anthropic request was made.

## Assignment requirements

| Requirement | Status | Evidence |
|---|---|---|
| Interpret brief with Claude; structured JSON rule; backend validation; never execute LLM SQL | ✅ | `segment` validator/compiler tests; `ai` scripted tests; ADR-003 |
| Only supported fields/operators; consent per channel | ✅ | validator tables; audience partition tests; Compose run: partitions sum to matched users |
| Audience size estimate | ✅ | `GET /audience`; fixture count tests; browser run (178 matched, 205 eligible messages) |
| SMS ≤ 160, subject ≤ 80, email body; `{{first_name}}`; backend validation | ✅ | `content` tests; live probes (`malformed_placeholder`, `unknown_variable`, `invalid_character`, `markup_not_allowed`, `too_long`) |
| Streamed content to the frontend | ✅ | `POST …/content/generations` SSE; browser run |
| Preview and edit before approval | ✅ | editors + sandboxed email preview; content versions |
| Explicit human approval before sending | ✅ | send while pending → `409 invalid_state`; confirm/send version- and binding-guarded |
| Asynchronous sending via a mock provider | ✅ | worker + `mockprovider`; smoke-send; browser run |
| ≤ 100 msg/s per channel, retries, 429, idempotency, per recipient/channel tracking | ✅ | rate integration test; Compose runs: `max_in_any_rolling_1s` ≤ 100, `limit_exceeded` 0, `duplicate_accepted_keys` 0 |
| Live statistics, health and metrics endpoints | ✅ | SSE dashboard; `/healthz`, `/readyz`, `/metrics` |
| Mock LLM so tests run without a key | ✅ | `go test ./...`, `npm test` with no key |

## Automated verification

| Check | Result |
|---|---|
| `go test ./...` (no key, no Docker) | ✅ 422 top-level tests, 992 subtests passed, 0 skipped |
| `go test -race ./...` | ✅ all packages ok |
| `go test -tags=integration -race ./test/integration/...` (testcontainers) | ✅ 129 top-level tests, 142 subtests passed, 0 skipped (147 s) |
| `gofmt -l` | ✅ clean |
| `go vet` (default, `integration`, `live` tags) | ✅ clean |
| staticcheck v0.8.2 (default and `integration` tags) | ✅ clean — run offline from the verified module cache (see note) |
| `go mod verify` | ✅ all modules verified |
| Frontend Vitest | ✅ 18 files, 270 tests passed (260 before CP18 + 10 new focus tests) |
| Frontend typecheck (`vue-tsc` app + tests, `tsc` node) | ✅ clean |
| Frontend production build | ✅ |
| `npm audit --omit=dev` | ✅ 0 vulnerabilities |

**`make lint` note.** The literal target fails at the staticcheck step: the
module proxy download fails with `x509: certificate signed by unknown
authority` (local TLS interception). TLS verification and the checksum database
were **not** changed. The same pinned v0.8.2 was run from the local module
cache with `GOPROXY=file://$(go env GOMODCACHE)/cache/download` and is clean;
gofmt and all `go vet` steps of `make lint` pass as written.

## Clean-room installation

Fresh `git clone` (no `.env`) into a scratch directory, isolated project
`cc-cp18`, port 18000, `LLM_PROVIDER=demo ANTHROPIC_API_KEY=`; the existing
`campaign-composer` stack and its volumes were not touched.

| Check | Result |
|---|---|
| `docker compose up --build -d --wait` | ✅ all images built; every service healthy in ≈ 3 min 47 s |
| Order | ✅ postgres healthy → migrate (0 → 2) → seed → api healthy → mockprovider healthy → worker; frontend after api |
| Seed | ✅ 5,000 users, `dataset_hash c553fe36…` — identical to the CP3 run (deterministic) |
| Reference data | ✅ mixed-spelling cities normalized (İstanbul matches 1,522 users) |
| Readiness | ✅ `/readyz`: database ok, migration 2, not dirty, `llm_provider: demo`, worker fresh |
| Published ports | ✅ only `127.0.0.1:18000→8080` (frontend); postgres, api, worker and mockprovider unpublished; `/metrics`, `/healthz`, `/admin/*` not reachable through nginx |
| `down` + `up` | ✅ campaigns, deliveries, attempts unchanged; mock ledger messages unchanged (126/137); `migrate` no-op, `seed` "nothing to do" |
| Rebuild (`up --build`) | ✅ data preserved |
| Smoke scripts | ✅ `smoke.sh`, `smoke-send.sh`, `smoke-cancel.sh` passed |

## Browser acceptance (built-in browser, through nginx, demo mode)

| Scenario | Result |
|---|---|
| **A. Full workflow** — Turkish brief → *City is İstanbul · Abandoned cart in the last 30 days* → 178 matched (99 SMS + 106 email eligible) → generate → prepare (205 frozen, digest, countdown) → confirm → Start sending → live SSE → *Completed with failures* (203 sent, 2 permanent SMS rejections by design) → reload persists | ✅ UI = `/stats` = DB (217 attempts, 2×429) = mock ledger per-campaign messages (106 email, 97 SMS); no duplicate keys |
| **B. Cancellation** — 1,194-delivery campaign cancelled mid-send | ✅ 255 queued cancelled at once, 19 in flight reported; 0 attempts started after `cancelled_at`; 6 timeouts that committed at the mock are `outcome_uncertain` (ledger 479 SMS vs 473 sent — the honest uncertain case); state persists on reload |
| **C. Failure and recovery** — mock provider restarted, then worker `SIGKILL`ed mid-send | ✅ completed; ledger = DB sent per channel (605/576), 23 replays, 0 duplicate accepted keys, `max_in_any_rolling_1s` 100/100, `limit_exceeded` 0; retries and 429s recorded |
| **D. Invalid and stale operations** | ✅ send while pending `409 invalid_state`; stale confirm/send/edit `409 version_conflict`; channel/content/segment mismatch `409 approval_mismatch`; duplicate channel / missing fields `422`; repeat confirm `409`; unknown approval `404`; duplicate send `200 already_started`; expiry (5 s TTL) `409 approval_expired`, campaign back to `content_ready`, held rows deleted; consent revoked after approval → `skipped_consent_revoked` (0 sent); phone removed → `skipped_no_contact`; malformed JSON/duplicate keys/unknown fields/oversized body/bad UUID/out-of-range query → `422`/`415`/`413`; SSE dashboard survived an API container recreation mid-send and reached *Final*; stream ends with one `end` event |
| **E. Usability** | ✅ 375 px: no page-level horizontal overflow (tables scroll inside their container), dialog fits; error states for unknown/invalid campaign IDs and unknown routes; no console errors other than the deliberate API restart (502 / cut stream) and the deliberate 404. **Fixed in CP18:** modal dialogs did not trap Tab or restore focus, and two sections were numbered "4" |

Supporting deterministic evidence lives in the integration suite (lock-wait
expiry, snapshot invariant for users added after prepare, crash after accept →
replay, lease reaper, two workers, rate measurement, 429 isolation).

## Security audit summary

No exploitable issue was found; the existing controls hold under black-box probing:

- Strict JSON (single object, UTF-8, ≤ 64 KiB, depth ≤ 4, no duplicate or
  unknown keys, integer typing and overflow checks); nginx caps bodies at 256 KiB.
- Path IDs must be UUIDs; query parameters are allow-listed, single-valued and bounded.
- SQL: registry identifiers + bind parameters only; no LLM- or user-authored SQL.
- Content: closed placeholder grammar, no template engine, control/bidi
  characters rejected, markup rejected, no truncation; email preview in
  `<iframe sandbox="">` with escaped `srcdoc`; no `v-html` anywhere.
- Same-origin only (no CORS headers; `OPTIONS` → 405); CSP, `X-Frame-Options:
  DENY`, `nosniff`, `Referrer-Policy: no-referrer`, `Cache-Control: no-store` on the API.
- SSE streams capped at 64 per API process; `ReadHeaderTimeout` on every server.
- Logs: query strings not logged, contacts masked; `smoke-send` and an
  integration privacy scan check logs/responses for contacts and message text.
- Secrets: `.env` git-ignored and never committed (history scanned for the
  local key prefix: 0 hits); only placeholder test keys in tests.
- Containers: non-root, read-only, capabilities dropped, `no-new-privileges`;
  only the frontend is published, on loopback.

## CP18 changes

- `frontend/src/composables/useModalFocus.ts` (new) + `ApprovalDialog.vue`,
  `ConfirmDialog.vue`: Tab/Shift+Tab stay inside modal dialogs; focus returns
  to the opener on close; Escape still closes only when nothing is pending; a
  dialog that opened by itself (no opener) or is unmounted by navigation leaves
  focus on the document, never on a detached node. Tests:
  `frontend/tests/modalFocus.spec.ts` (ConfirmDialog) and *dialog keyboard
  focus* in `frontend/tests/approval.spec.ts` (ApprovalDialog in the full app);
  the trap and focus-return tests fail without the fix (jsdom does not move
  focus on Tab by itself, so the fallback tests guard against errors rather
  than prove the trap).
- `ContentPanel.vue`, `ApprovalPanel.vue`, `DeliveryPanel.vue`: section
  numbers 5/6/7 (were 4/5/6, duplicating "4"), plus matching code comments
  and the "step 7" hint. Before interpretation the page shows steps 1–3
  (placeholders); afterwards 1–7.
- `README.md` restructured for reviewers; stale seed count corrected.
- Documentation consistency: ARCHITECTURE §8/§14/§18, ADR-013 and ADR-015
  status notes (incl. the distinction between live *integration* verification and
  comparative *model-quality* evaluation), ADR-016 trade-off, ROADMAP submission
  status, `CLAUDE.md` stack line (stdlib metrics, Playwright not built).
- `.gitignore`: mock ledger journals and database dumps (`*.journal`, `*.dump`, `*.pgdump`, `*.sql.gz`, `dumps/`).
- This checklist.

## Remaining items

**Blockers:** none found for running, reviewing or demonstrating the submission.

**Not performed, by decision:** the roadmap's Day 2 12-brief comparative model
evaluation (A tier in this repository's own plan, not in the assignment). It
needs paid API calls; `docs/model-evaluation.md` does not exist and no results
are claimed. Live Anthropic *integration* was verified once in CP10 (one
interpretation call); that is not a model-quality comparison (ADR-015 status).

**Accepted trade-offs:** see README *Known limitations* and ARCHITECTURE §18 —
at-least-once requests with ledger-conditional at-most-one acceptance; no
recall of in-flight requests on cancel; provider-arrival rate assumes ≤ 30 ms
jitter; single dispatching worker; stdlib metrics exposition; staticcheck
download blocked by local TLS interception; B-tier items not built; base images
pinned by tag.

## Recommended 3–5 minute live demo

1. `LLM_PROVIDER=demo ANTHROPIC_API_KEY= docker compose up --build -d --wait`; open http://localhost:8000 (Demo AI badge).
2. Example brief → Create → **Interpret**: rule chips, assumptions, per-channel partition that sums to matched users (30 s).
3. **Generate content**: streamed phases, validated version; type `{{email}}` in the SMS to show a blocking server error, undo (45 s).
4. **Prepare approval**: frozen counts, digest, 15-min countdown, rendered content; acknowledge → **Confirm** (45 s).
5. **Start sending**: live dashboard to *Completed with failures*; point out "sent = accepted by the mock", attempts vs deliveries, 429s/retries; reload (60 s).
6. Optional: a 90-day abandoned-cart campaign (≈ 1,200 deliveries) → **Cancel sending** mid-flight → cancelled vs in-flight vs uncertain (45 s).
7. Close with `docker compose exec mockprovider wget -qO- http://127.0.0.1:8090/admin/stats` (`max_in_any_rolling_1s ≤ 100`, `duplicate_accepted_keys: 0`).
