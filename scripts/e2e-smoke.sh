#!/usr/bin/env bash
# Boot-and-answer smoke test: builds, starts on a throwaway port + data dir,
# exercises the source/listing/document/application lifecycle, then drives the
# real scripts/job-radar-dispatch.sh against that same throwaway store with stub
# session runners, then tears down. No model call, no network, no touching the
# real ~/.config/job-store.
set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_DIR"

export PATH="$HOME/.local/share/mise/shims:$PATH"
PORT="${SMOKE_PORT:-18311}"
DATA_DIR="$(mktemp -d)"
BASE="http://127.0.0.1:${PORT}"

cleanup() {
  [[ -n "${SRV_PID:-}" ]] && kill "$SRV_PID" 2>/dev/null || true
  rm -rf "$DATA_DIR" "${DISPATCH_DIR:-}"
}
trap cleanup EXIT

pass() { echo "    ok: $1"; }
fail() { echo "FAIL: $1"; exit 1; }

# Count rows out of a list response with python, never `grep -o | wc -l`: with
# zero rows grep exits 1, and under this script's `set -euo pipefail` that aborts
# the smoke with no output — so the one direction an assertion like "the re-upsert
# lost the row" exists to catch would kill the test instead of failing it.
count_key() { # $1 = json, $2 = top-level array key
  printf '%s' "$1" | python3 -c "import json,sys; print(len(json.load(sys.stdin).get('$2') or []))"
}
id_of() { # $1 = json, $2 = array key, $3 = field, $4 = value
  printf '%s' "$1" | python3 -c "import json,sys; print(next(r['id'] for r in json.load(sys.stdin)['$2'] if str(r['$3'])=='''$4'''))"
}

echo "==> build"
# Same tags deploy.sh builds with. Without sqlite_fts5 the binary compiles fine
# and then dies at migrate time on `no such module: fts5`, so a smoke that
# omitted the tag would only ever be testing a binary nobody deploys.
GO_TAGS="${GO_TAGS:-sqlite_fts5}"
go build -tags "$GO_TAGS" -o /tmp/job-store-smoke ./cmd/job-store

echo "==> start on $PORT (data=$DATA_DIR)"
# noteboard is pointed at a port nothing listens on, on purpose: the task checks
# below are about job-store SAYING it cannot reach noteboard rather than showing
# an empty task list, and a smoke that happened to find the real noteboard up
# would never exercise that.
JOB_STORE_ADDR=":${PORT}" JOB_STORE_DATA_DIR="$DATA_DIR" NOTEBOARD_URL="http://127.0.0.1:1" \
  /tmp/job-store-smoke &
SRV_PID=$!
for i in $(seq 1 30); do
  curl -sf "$BASE/health" >/dev/null 2>&1 && break
  sleep 0.2
done

echo "==> health"
curl -sf "$BASE/health" | grep -q '"status":"ok"' && pass health || fail health

echo "==> role-family vocabulary is served in display order"
FAMS=$(curl -sf "$BASE/role-families")
echo "$FAMS" | grep -q '"name":"Engineering".*"name":"AI".*"name":"Data".*"name":"Product".*"name":"Design".*"name":"Leadership"' \
  && pass "role families in display order" || fail "role families: got $FAMS"

echo "==> create a board source (role_families normalize onto the vocabulary)"
SRC=$(curl -sf -X POST "$BASE/sources" -H 'Content-Type: application/json' \
  -d '{"name":"smoke-board","kind":"board","target":"https://boards.greenhouse.io/example","company":"Example AI","location":"Remote (US)","role_families":["backend","MLE"],"seniorities":["senior","staff"],"cadence_hours":24}')
echo "$SRC" | grep -q '"role_families":\["Engineering","AI"\]' \
  && pass "source role families normalized + rank-sorted" || fail "source role families: got $SRC"

echo "==> a new approved source is due immediately (next_run_at defaults to now)"
DUE=$(curl -sf "$BASE/sources?due=1")
echo "$DUE" | grep -q '"smoke-board"' && pass "source due" || fail "source due"

echo "==> an unknown kind is refused"
CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/sources" -H 'Content-Type: application/json' \
  -d '{"name":"bad-kind","kind":"telepathy"}')
[[ "$CODE" == "400" ]] && pass "unknown kind rejected (400)" || fail "unknown kind: HTTP $CODE"

echo "==> upsert listing (a specific label canonicalizes onto a family, kept as a tag)"
CREATED=$(curl -sf -X POST "$BASE/listings" -H 'Content-Type: application/json' \
  -d '{"title":"Staff Inference Engineer","company":"Example AI","location":"Remote (US)","remote":true,"employment_type":"full-time","seniority":"staff","role_family":"MLE","comp_min":220000,"comp_max":290000,"comp_currency":"USD","comp_raw":"$220,000 - $290,000 + equity","url":"https://example.com/jobs/1","apply_url":"https://example.com/jobs/1/apply","source_name":"smoke-board","relevance":88}')
echo "$CREATED" | grep -q '"Staff Inference Engineer"' && pass "create listing" || fail "create listing"
echo "$CREATED" | grep -q '"role_family":"AI"' && pass "role family canonicalized" || fail "role family: got $CREATED"
echo "$CREATED" | grep -q '"tags":\["mle"\]' && pass "specific label kept as tag" || fail "tag: got $CREATED"
echo "$CREATED" | grep -q '"comp_raw":"\$220,000 - \$290,000 + equity"' \
  && pass "comp_raw kept exactly as posted" || fail "comp_raw: got $CREATED"
LID=$(printf '%s' "$CREATED" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')

echo "==> a listing posted without the full posting text is flagged, not silently accepted"
# The description is the load-bearing field: everything downstream (tailoring,
# relevance, an application agent filling a form) reads it and nothing else. A
# blank one still stores — a partial listing beats a lost one — but it must come
# back flagged, or a poller that dropped the body never finds out.
echo "$CREATED" | grep -q '"description_thin":true' \
  && pass "a description-less create is flagged thin" || fail "no thin flag on a blank description: $CREATED"
curl -sfS "$BASE/listings?thin_description=1" | grep -q 'Staff Inference Engineer' \
  && pass "the thin-description audit lists it" || fail "thin_description=1 did not list the blank row"

export FULL_DESC="About the role. You will own the inference platform end to end: the serving stack, the autoscaler, and the evaluation harness that gates every model release. Responsibilities: design and operate low-latency GPU serving; own capacity planning; partner with research on model rollout. Requirements: 8+ years building distributed systems in Go, Rust or C++; production experience with GPU inference; comfort owning an on-call rotation. Nice to have: CUDA, Triton, vLLM. Compensation: 220,000 to 290,000 USD plus equity. Location: remote within the United States, with quarterly onsites in San Francisco."
REFRESHED=$(curl -sfS -X POST "$BASE/listings" -H 'Content-Type: application/json' \
  -d "$(python3 -c 'import json,os; print(json.dumps({"title":"Staff Inference Engineer","company":"Example AI","location":"Remote (US)","description":os.environ["FULL_DESC"]}))')")
echo "$REFRESHED" | grep -q '"description_thin":true' \
  && fail "a full description is still flagged thin: $REFRESHED" || pass "a full description clears the thin flag"
printf '%s' "$REFRESHED" | python3 -c 'import json,sys; sys.exit(0 if json.load(sys.stdin)["description_fetched_at"] > 0 else 1)' \
  && pass "the store stamps description_fetched_at itself" || fail "description_fetched_at not stamped: $REFRESHED"
curl -sfS "$BASE/listings?thin_description=1" | grep -q 'Staff Inference Engineer' \
  && fail "a listing with a full description is still in the thin audit" || pass "the thin audit clears when the body lands"
curl -sfS "$BASE/listings?q=autoscaler" | grep -q 'Staff Inference Engineer' \
  && pass "full-text search reaches into the description" || fail "q= did not match a word only in the description"

echo "==> an off-vocabulary role family is rejected, not stored"
CODE=$(curl -s -o /tmp/job-store-smoke-badfam -w '%{http_code}' -X POST "$BASE/listings" \
  -H 'Content-Type: application/json' \
  -d '{"title":"Chief Vibes Officer","company":"Example AI","location":"Remote (US)","role_family":"vibes"}')
[[ "$CODE" == "400" ]] && pass "unknown role family rejected (400)" || fail "unknown role family: got HTTP $CODE"
# The 400 has to name the whole vocabulary or a poller cannot retry without
# guessing — the prompt tells it to read this message and pick from it.
grep -q 'Engineering' /tmp/job-store-smoke-badfam && grep -q 'Leadership' /tmp/job-store-smoke-badfam \
  && pass "rejection names the vocabulary" || fail "rejection body: $(cat /tmp/job-store-smoke-badfam)"

echo "==> re-upsert the same role dedupes (sig = company|title|location)"
curl -sf -X POST "$BASE/listings" -H 'Content-Type: application/json' \
  -d '{"title":"Staff Inference Engineer","company":"Example AI","location":"Remote (US)","comp_raw":"$230,000 - $300,000","description":"refreshed"}' >/dev/null
COUNT=$(count_key "$(curl -sf "$BASE/listings")" listings)
[[ "$COUNT" == "1" ]] && pass "dedupe (1 listing)" || fail "dedupe expected 1 got $COUNT"

echo "==> a re-poll refreshes the posting and never the user's decision"
curl -sf -X PATCH "$BASE/listings/$LID" -H 'Content-Type: application/json' \
  -d '{"status":"interested","notes":"worth a look","relevance":95}' | grep -q '"status":"interested"' \
  && pass "patch listing (the decision channel)" || fail "patch listing"
REPOLL=$(curl -sf -X POST "$BASE/listings" -H 'Content-Type: application/json' \
  -d '{"title":"Staff Inference Engineer","company":"Example AI","location":"Remote (US)","description":"re-polled","relevance":10,"status":"candidate","notes":""}')
echo "$REPOLL" | grep -q '"status":"interested"' && pass "re-poll cannot reset status" || fail "re-poll clobbered status: $REPOLL"
echo "$REPOLL" | grep -q '"notes":"worth a look"' && pass "re-poll cannot clobber notes" || fail "re-poll clobbered notes: $REPOLL"
echo "$REPOLL" | grep -q '"relevance":95' && pass "re-poll cannot clobber relevance" || fail "re-poll clobbered relevance: $REPOLL"
echo "$REPOLL" | grep -q 're-polled' && pass "re-poll does refresh the description" || fail "re-poll did not refresh: $REPOLL"

echo "==> prune sweeps stale candidates and spares everything the user touched"
NOW=$(date +%s)
BEFORE=$(count_key "$(curl -sfS "$BASE/listings")" listings)
# Posted 100 days ago and never triaged; posted 100 days ago but the user said
# they were interested; and one with no posted date at all.
curl -sfS -X POST "$BASE/listings" -H 'Content-Type: application/json' \
  -d "{\"title\":\"Stale Candidate\",\"company\":\"Example AI\",\"location\":\"Remote (US)\",\"posted_at\":$((NOW - 8640000))}" >/dev/null
TOUCHED=$(curl -sfS -X POST "$BASE/listings" -H 'Content-Type: application/json' \
  -d "{\"title\":\"Stale But Wanted\",\"company\":\"Example AI\",\"location\":\"Remote (US)\",\"posted_at\":$((NOW - 8640000))}")
TID=$(printf '%s' "$TOUCHED" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
curl -sfS -X PATCH "$BASE/listings/$TID" -H 'Content-Type: application/json' -d '{"status":"interested"}' >/dev/null
curl -sfS -X POST "$BASE/listings" -H 'Content-Type: application/json' \
  -d '{"title":"Undated Opening","company":"Example AI","location":"Remote (US)"}' >/dev/null

PRUNED=$(curl -sfS -X POST "$BASE/listings/prune" -H 'Content-Type: application/json' -d '{"grace_days":45}' \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["pruned"])')
[[ "$PRUNED" == "1" ]] && pass "prune removed the one stale candidate" || fail "prune removed $PRUNED listing(s), want 1"
LISTED=$(curl -sfS "$BASE/listings")
grep -q 'Stale Candidate' <<<"$LISTED" && fail "a stale candidate is still listed" || pass "stale candidate is hidden"
grep -q 'Stale But Wanted' <<<"$LISTED" && pass "a listing the user touched is spared" || fail "a touched listing was pruned"
grep -q 'Undated Opening' <<<"$LISTED" && pass "an undated listing is never stale" || fail "an undated listing was pruned"

echo "==> a pruned listing is soft-deleted: still there, still deduping, restorable"
ALL=$(curl -sfS "$BASE/listings?include_deleted=1")
grep -q 'Stale Candidate' <<<"$ALL" && pass "the row survives the prune" || fail "prune destroyed the row"
PID=$(id_of "$ALL" listings title "Stale Candidate")
# The row IS the dedupe memory: a re-poll of the same long-dead posting must not
# put it back in front of the user.
curl -sfS -X POST "$BASE/listings" -H 'Content-Type: application/json' \
  -d "{\"title\":\"Stale Candidate\",\"company\":\"Example AI\",\"location\":\"Remote (US)\",\"posted_at\":$((NOW - 8640000))}" >/dev/null
curl -sfS "$BASE/listings" | grep -q 'Stale Candidate' \
  && fail "a re-poll resurrected a pruned listing" || pass "a re-poll cannot resurrect a pruned listing"
curl -sfS -X POST "$BASE/listings/$PID/restore" | grep -q '"deleted_at":0' \
  && pass "restore brings it back" || fail "restore"
# A decision can never land invisibly on a swept row.
curl -sfS -X DELETE "$BASE/listings/$PID" >/dev/null
CODE=$(curl -s -o /dev/null -w '%{http_code}' -X PATCH "$BASE/listings/$PID" \
  -H 'Content-Type: application/json' -d '{"status":"applied"}')
[[ "$CODE" == "404" ]] && pass "patching a soft-deleted listing is a 404" || fail "patch on deleted row: HTTP $CODE"
curl -sfS -X DELETE "$BASE/listings/$PID?hard=true" | grep -q '"status":"purged"' \
  && pass "hard delete purges" || fail "hard delete"

# Leave the store as the later checks expect to find it.
for title in "Stale But Wanted" "Undated Opening"; do
  ID=$(id_of "$(curl -sfS "$BASE/listings")" listings title "$title")
  curl -sfS -X DELETE "$BASE/listings/$ID?hard=true" >/dev/null
done
AFTER=$(count_key "$(curl -sfS "$BASE/listings")" listings)
[[ "$AFTER" == "$BEFORE" ]] && pass "prune checks left the store as they found it" \
  || fail "listing count drifted: $BEFORE -> $AFTER"

echo "==> a scouted source arrives proposed, inert, and never due"
PROPOSED=$(curl -sfS -X POST "$BASE/sources" -H 'Content-Type: application/json' \
  -d '{"name":"scouted-board","kind":"board","target":"https://jobs.ashbyhq.com/example","location":"Remote (US)","role_families":["AI"],"status":"proposed","proposed_by":"smoke scout"}')
echo "$PROPOSED" | grep -q '"status":"proposed"' && pass "proposed status stored" || fail "proposed: $PROPOSED"
echo "$PROPOSED" | grep -q '"enabled":false' && pass "proposal is inert" || fail "proposal should not be enabled: $PROPOSED"
SID=$(printf '%s' "$PROPOSED" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
curl -sfS "$BASE/sources?due=1" | grep -q 'scouted-board' \
  && fail "a proposed source reached the due list" || pass "proposed source is not due"

echo "==> rejecting sticks, even when the scout re-proposes it"
curl -sfS -X PATCH "$BASE/sources/$SID" -H 'Content-Type: application/json' \
  -d '{"status":"rejected"}' | grep -q '"status":"rejected"' && pass "reject" || fail "reject"
curl -sfS -X POST "$BASE/sources" -H 'Content-Type: application/json' \
  -d '{"name":"scouted-board","kind":"board","target":"https://jobs.ashbyhq.com/example2","status":"proposed"}' \
  | grep -q '"status":"rejected"' && pass "re-proposal cannot un-reject" || fail "re-proposal un-rejected a source"

echo "==> approving a proposal enables it and makes it due"
curl -sfS -X POST "$BASE/sources" -H 'Content-Type: application/json' \
  -d '{"name":"good-board","kind":"board","target":"https://jobs.lever.co/example","status":"proposed"}' >/dev/null
GID=$(id_of "$(curl -sfS "$BASE/sources?status=proposed")" sources name "good-board")
[[ -n "$GID" ]] && pass "filter by status=proposed" || fail "filter by status=proposed"
curl -sfS -X PATCH "$BASE/sources/$GID" -H 'Content-Type: application/json' -d '{"status":"active"}' \
  | grep -q '"enabled":true' && pass "approval enables" || fail "approval should enable"
curl -sfS "$BASE/sources?due=1" | grep -q 'good-board' && pass "approved source is due" || fail "approved source should be due"

echo "==> documents render server-side, so agent and browser see the same bytes"
DOC=$(curl -sfS -X POST "$BASE/documents" -H 'Content-Type: application/json' \
  -d '{"name":"smoke-resume","kind":"resume","format":"markdown","body":"# Jane Dev\n\n- Go, SQLite","is_default":true}')
DID=$(printf '%s' "$DOC" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
curl -sfS "$BASE/documents/$DID/render" | grep -qi '<h1' && pass "render returns html" || fail "render did not convert markdown"

echo "==> an application joins on the listing id, and submit is honestly 501"
CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/applications" -H 'Content-Type: application/json' \
  -d '{"listing_id":999999}')
[[ "$CODE" == "400" ]] && pass "an application for a nonexistent listing is a 400" || fail "bogus listing_id: HTTP $CODE"
APP=$(curl -sfS -X POST "$BASE/applications" -H 'Content-Type: application/json' \
  -d "{\"listing_id\":$LID,\"agent_status\":\"draft\",\"resume_document_id\":$DID}")
AID=$(printf '%s' "$APP" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
CODE=$(curl -s -o /tmp/job-store-smoke-submit -w '%{http_code}' -X POST "$BASE/applications/$AID/submit")
[[ "$CODE" == "501" ]] && pass "submit is 501, not a pretend success" || fail "submit: HTTP $CODE"

echo "==> creating the application hands the pipeline over: the listing is now applied"
curl -sfS "$BASE/listings/$LID" | grep -q '"status":"applied"' \
  && pass "the listing moved to applied" || fail "the listing did not move to applied"

echo "==> the field once called status is refused, never ignored"
# A caller still sending `status` would otherwise get a 200 and no change, and go
# on believing it had recorded something.
CODE=$(curl -s -o /tmp/job-store-smoke-renamed -w '%{http_code}' -X PATCH "$BASE/applications/$AID" \
  -H 'Content-Type: application/json' -d '{"status":"submitted"}')
[[ "$CODE" == "400" ]] && pass "a patch with the old status field is a 400" || fail "patch status: HTTP $CODE"
CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/applications" \
  -H 'Content-Type: application/json' -d "{\"listing_id\":$LID,\"status\":\"ready\"}")
[[ "$CODE" == "400" ]] && pass "a create with the old status field is a 400" || fail "create status: HTTP $CODE"
CODE=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/applications?status=draft")
[[ "$CODE" == "400" ]] && pass "a list filter on the old status field is a 400" || fail "list status: HTTP $CODE"
grep -q 'agent_status' /tmp/job-store-smoke-renamed && grep -q 'stage' /tmp/job-store-smoke-renamed \
  && pass "the rejection names both replacements" || fail "rejection body: $(cat /tmp/job-store-smoke-renamed)"

echo "==> a stage change cannot happen without leaving a trace"
curl -sfS -X PATCH "$BASE/applications/$AID" -H 'Content-Type: application/json' \
  -d '{"stage":"submitted","note":"applied through the careers page"}' \
  | grep -q '"stage":"submitted"' && pass "stage change accepted" || fail "stage change"
EVENTS=$(curl -sfS "$BASE/applications/$AID/events")
[[ "$(count_key "$EVENTS" events)" == "2" ]] \
  && pass "the timeline has the create and the stage change" || fail "events: $EVENTS"
grep -q '"stage_from":"drafting","stage_to":"submitted"' <<<"$EVENTS" \
  && pass "the event records where it moved from and to" || fail "event shape: $EVENTS"
curl -sfS -X POST "$BASE/applications/$AID/events" -H 'Content-Type: application/json' \
  -d '{"note":"recruiter called","source":"user"}' | grep -q '"note":"recruiter called"' \
  && pass "a note is recorded without moving the stage" || fail "note event"
CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/applications/$AID/events" \
  -H 'Content-Type: application/json' -d '{"note":"they said yes","stage_to":"offer"}')
[[ "$CODE" == "400" ]] && pass "the note route cannot write a stage change" || fail "note route wrote a stage: HTTP $CODE"

echo "==> only a confirmed email may drive a stage"
EMAIL=$(curl -sfS -X POST "$BASE/applications/$AID/emails" -H 'Content-Type: application/json' \
  -d '{"account_id":"gmail-personal","message_id":"18f2c9a1b","subject":"Your application","from_address":"careers@example.ai","linked_by":"matcher"}')
echo "$EMAIL" | grep -q '"status":"proposed"' \
  && pass "a matcher may only propose" || fail "matcher link: $EMAIL"
EMAIL_ID=$(printf '%s' "$EMAIL" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/application-emails/$EMAIL_ID/stage" \
  -H 'Content-Type: application/json' -d '{"stage":"acknowledged"}')
[[ "$CODE" == "400" ]] && pass "a proposed email cannot move the stage" || fail "proposed email moved a stage: HTTP $CODE"
curl -sfS "$BASE/applications/$AID" | grep -q '"stage":"submitted"' \
  && pass "the stage is where it was" || fail "the stage moved on a proposal"
curl -sfS -X PATCH "$BASE/application-emails/$EMAIL_ID" -H 'Content-Type: application/json' \
  -d '{"status":"linked"}' | grep -q '"status":"linked"' && pass "a human confirms the link" || fail "confirm link"
curl -sfS -X POST "$BASE/application-emails/$EMAIL_ID/stage" -H 'Content-Type: application/json' \
  -d '{"stage":"acknowledged"}' | grep -q '"stage":"acknowledged"' \
  && pass "a confirmed email moves the stage" || fail "confirmed email did not move the stage"
curl -sfS "$BASE/applications/$AID/events" | grep -q '"source":"email"' \
  && pass "the mail-driven move is on the timeline as such" || fail "no email-sourced event"

echo "==> tasks are noteboard ids, and an unreachable noteboard is said out loud"
# The standard set is created IN noteboard, so with noteboard down there are no
# ids to store — and a 502 saying so beats links to todos that never existed.
CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/applications/$AID/tasks/standard")
[[ "$CODE" == "502" ]] && pass "standard tasks refuse rather than invent ids" || fail "standard tasks: HTTP $CODE"
curl -sfS -X POST "$BASE/applications/$AID/tasks" -H 'Content-Type: application/json' \
  -d '{"noteboard_id":"93c345b7-7536-4f37-a76a-25dc25d015a3"}' | grep -q '"noteboard_id"' \
  && pass "a noteboard todo id is linked" || fail "link task"
CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/applications/$AID/tasks/standard")
[[ "$CODE" == "400" ]] && pass "the standard set will not duplicate follow-ups it already has" \
  || fail "standard tasks on an application with links: HTTP $CODE"
EXPANDED=$(curl -sfS "$BASE/applications/$AID?expand=tasks")
printf '%s' "$EXPANDED" | python3 -c 'import json,sys; t=json.load(sys.stdin)["tasks"]; sys.exit(0 if t.get("error") and t.get("items") is None else 1)' \
  && pass "an unreachable noteboard is an explicit error, never an empty list" \
  || fail "expand with noteboard down: $EXPANDED"

echo "==> the listing status stops moving once an application owns the pipeline"
CODE=$(curl -s -o /tmp/job-store-smoke-frozen -w '%{http_code}' -X PATCH "$BASE/listings/$LID" \
  -H 'Content-Type: application/json' -d '{"status":"dismissed"}')
[[ "$CODE" == "400" ]] && pass "a listing with an application refuses a status change" || fail "listing status: HTTP $CODE"
curl -sfS -X PATCH "$BASE/listings/$LID" -H 'Content-Type: application/json' \
  -d '{"notes":"screen scheduled"}' | grep -q 'screen scheduled' \
  && pass "everything else on that listing is still editable" || fail "notes patch refused too"

echo "==> a summary line needs no extra requests"
SUMMARY=$(curl -sfS "$BASE/applications?stage=acknowledged")
printf '%s' "$SUMMARY" | python3 -c '
import json,sys
app = json.load(sys.stdin)["applications"][0]
for field in ("resume_drifted","email_count","task_count","open_task_count","last_activity_at"):
    assert field in app, field
assert app["email_count"] == 1, app["email_count"]
assert app["task_count"] == 1, app["task_count"]
# noteboard is unreachable in this smoke, so the open count must say "cannot tell".
assert app["open_task_count"] is None, app["open_task_count"]
assert app["last_activity_at"] > 0, app["last_activity_at"]
' && pass "each row carries its own counts, and open_task_count is null rather than 0" \
  || fail "summary fields: $SUMMARY"
CODE=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/applications?stage=drafting")
[[ "$CODE" == "200" ]] && pass "stage and agent_status are separate filters" || fail "stage filter: HTTP $CODE"

echo "==> the resume that was actually sent is pinned, and drift is reported"
SUBMITTED=$(curl -sfS -X PATCH "$BASE/applications/$AID" -H 'Content-Type: application/json' \
  -d '{"agent_status":"submitted"}')
printf '%s' "$SUBMITTED" | python3 -c 'import json,sys; sys.exit(0 if json.load(sys.stdin)["resume_body_sha256"] else 1)' \
  && pass "submitting pins the resume body" || fail "nothing pinned: $SUBMITTED"
echo "$SUBMITTED" | grep -q '"resume_drifted":false' \
  && pass "no drift the moment it was pinned" || fail "drift reported immediately: $SUBMITTED"
curl -sfS -X PATCH "$BASE/documents/$DID" -H 'Content-Type: application/json' \
  -d '{"body":"# Jane Dev\n\n- Go, SQLite, and a new bullet"}' >/dev/null
curl -sfS "$BASE/applications/$AID" | grep -q '"resume_drifted":true' \
  && pass "editing the resume shows up as drift" || fail "an edited resume did not read as drifted"

echo "==> tailoring is queued, and it refuses a listing it cannot tailor against"
# A listing with no real description has nothing to tailor against, and guessing
# from a job title produces exactly the generic letter this feature exists to
# avoid — so the refusal is at the queueing edge, before an agent is ever spawned.
THIN=$(curl -sfS -X POST "$BASE/listings" -H 'Content-Type: application/json' \
  -d '{"title":"Mystery Role","company":"Vague Corp","location":"Remote (US)"}')
THIN_ID=$(printf '%s' "$THIN" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/documents/$DID/tailor" \
  -H 'Content-Type: application/json' -d "{\"listing_id\":$THIN_ID}")
[[ "$CODE" == "400" ]] && pass "tailoring an empty-description listing is a 400" || fail "tailor on empty description: HTTP $CODE"
CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/documents/$DID/tailor" \
  -H 'Content-Type: application/json' -d '{"listing_id":999999}')
[[ "$CODE" == "400" ]] && pass "tailoring for a nonexistent listing is a 400" || fail "tailor on bogus listing: HTTP $CODE"

TAILOR_LISTING=$(curl -sfS -X POST "$BASE/listings" -H 'Content-Type: application/json' \
  -d "$(python3 -c 'import json,os; print(json.dumps({"title":"Staff Platform Engineer","company":"Tailor Test Co","location":"Remote (US)","url":"https://example.com/jobs/42","description":os.environ["FULL_DESC"]}))')")
TAILOR_LID=$(printf '%s' "$TAILOR_LISTING" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
CODE=$(curl -s -o /tmp/job-store-smoke-tailor -w '%{http_code}' -X POST "$BASE/documents/$DID/tailor" \
  -H 'Content-Type: application/json' -d "{\"listing_id\":$TAILOR_LID,\"instructions\":\"lead with the platform work\"}")
[[ "$CODE" == "202" ]] && pass "queueing a tailor request is a 202" || fail "tailor queue: HTTP $CODE"
grep -q '"status":"pending"' /tmp/job-store-smoke-tailor && pass "the request lands pending" || fail "queued request: $(cat /tmp/job-store-smoke-tailor)"
TREQ_ID=$(python3 -c 'import json; print(json.load(open("/tmp/job-store-smoke-tailor"))["id"])')

echo "==> mark source ran advances next_run_at"
SMOKE_SRC_ID=$(id_of "$(curl -sfS "$BASE/sources")" sources name "smoke-board")
RAN=$(curl -sf -X POST "$BASE/sources/$SMOKE_SRC_ID/ran")
echo "$RAN" | grep -q '"last_run_at"' && pass "mark ran" || fail "mark ran"
curl -sf "$BASE/sources?due=1" | grep -q '"smoke-board"' && fail "should no longer be due" || pass "rescheduled (not due)"

echo "==> the dispatcher hands the due list to the session and grades on what ran"
# Drives the real scripts/job-radar-dispatch.sh against this throwaway store with
# stub session runners, so all three grading branches — nothing ran, some ran, all
# ran — plus the quiet-tick skip are exercised without a model call.
DISPATCH_DIR="$(mktemp -d)"
STUB_LOG="$DISPATCH_DIR/prompt.txt"

write_stub() { # $1 = ids to mark ran (space separated, may be empty)
  cat >"$DISPATCH_DIR/claude-stub" <<STUB
#!/usr/bin/env bash
# Stub session runner. The dispatcher invokes it as \`-p <prompt> --model … \`, so
# \$2 is the prompt; record it, mark the ids this stub was built to mark, ignore
# the model flags.
printf '%s' "\$2" >"$STUB_LOG"
for id in $1; do curl -sfS -X POST "$BASE/sources/\$id/ran" >/dev/null; done
STUB
  chmod +x "$DISPATCH_DIR/claude-stub"
}

due_now() {
  curl -sfS "$BASE/sources?due=1" | python3 -c \
    'import json,sys; print(",".join(str(s["id"]) for s in json.load(sys.stdin).get("sources") or []))'
}

# A second due source, so the partial case below has something to leave behind.
curl -sfS -X POST "$BASE/sources" -H 'Content-Type: application/json' \
  -d '{"name":"dispatch-src","kind":"research","target":"remote staff backend roles","location":"Remote (US)","role_families":["Engineering"],"cadence_hours":24}' >/dev/null
DUE_IDS_CSV=$(due_now)
[[ "$DUE_IDS_CSV" == *,* ]] && pass "sources due before dispatch ($DUE_IDS_CSV)" || fail "expected two due sources, got '$DUE_IDS_CSV'"
DUE_IDS_SPACED=${DUE_IDS_CSV//,/ }
FIRST_DUE=${DUE_IDS_CSV%%,*}

run_dispatch() { # prints output, returns the script's exit status
  JOB_STORE_URL="$BASE" JOB_RADAR_CLAUDE_BIN="$DISPATCH_DIR/claude-stub" \
    bash "$REPO_DIR/scripts/job-radar-dispatch.sh" 2>&1
}

write_stub ""
OUT=$(run_dispatch) && DISPATCH_CODE=0 || DISPATCH_CODE=$?
[[ "$DISPATCH_CODE" != "0" ]] && pass "a session that rescheduled nothing fails the run" \
  || fail "dispatch reported success after rescheduling nothing: $OUT"
grep -q 'rescheduled NOTHING' <<<"$OUT" && pass "the failure says what went wrong" || fail "failure text: $OUT"

grep -q "\"id\": $FIRST_DUE" "$STUB_LOG" && pass "the due rows reach the prompt" || fail "prompt lacks the due rows: $(head -c 300 "$STUB_LOG")"
grep -q 'this list IS the work' "$STUB_LOG" && pass "the prompt tells the session the list is the work" || fail "prompt header missing"
grep -q 'do not fetch that list again' "$STUB_LOG" && pass "the prompt forbids re-fetching the due list" || fail "prompt still invites a re-fetch"
grep -q 'boards-api.greenhouse.io' "$STUB_LOG" && pass "the prompt spells out the ATS JSON APIs" || fail "prompt lost the ATS API patterns"
grep -q 'Do NOT send "status"' "$STUB_LOG" && pass "the prompt leaves status and notes to the user" || fail "prompt no longer protects status/notes"

write_stub "$FIRST_DUE"
OUT=$(run_dispatch) && DISPATCH_CODE=0 || DISPATCH_CODE=$?
[[ "$DISPATCH_CODE" == "0" ]] && pass "a partial dispatch still succeeds" || fail "partial dispatch failed: $OUT"
grep -q 'partial dispatch' <<<"$OUT" && pass "a partial dispatch names what is still due" || fail "partial not reported: $OUT"

write_stub "$DUE_IDS_SPACED"
OUT=$(run_dispatch) && DISPATCH_CODE=0 || DISPATCH_CODE=$?
[[ "$DISPATCH_CODE" == "0" ]] && pass "a full dispatch succeeds" || fail "full dispatch failed: $OUT"
grep -q 'dispatch complete' <<<"$OUT" && pass "full dispatch reports complete" || fail "no completion line: $OUT"

OUT=$(run_dispatch) && DISPATCH_CODE=0 || DISPATCH_CODE=$?
[[ "$DISPATCH_CODE" == "0" ]] && pass "nothing due is still a clean skip" || fail "quiet tick should exit 0: $OUT"
grep -q 'no sources due' <<<"$OUT" && pass "quiet tick says so without launching a session" || fail "quiet-tick text: $OUT"

echo "==> the tailor dispatcher drains the queue and never leaves a request running"
# Drives the real scripts/job-tailor-dispatch.sh with stub session runners: the
# idle tick (the hot path — this one runs every couple of minutes), a session that
# does the job, and a session that abandons it. The abandoned one is the important
# case: a request stuck in `running` is a spinner that never stops in the UI.
TAILOR_LOG="$DISPATCH_DIR/tailor-prompt.txt"

# The stub reads the request/document/listing ids straight out of the prompt,
# which doubles as an assertion that the dispatcher put them there.
cat >"$DISPATCH_DIR/tailor-stub-ok" <<STUB
#!/usr/bin/env bash
set -euo pipefail
printf '%s' "\$2" >>"$TAILOR_LOG"
req=\$(grep -m1 '^request id: ' <<<"\$2" | awk '{print \$3}')
doc=\$(grep -m1 '^source document id: ' <<<"\$2" | awk '{print \$4}')
lst=\$(grep -m1 '^listing id: ' <<<"\$2" | awk '{print \$3}')
new=\$(curl -sfS -X POST "$BASE/documents" -H 'Content-Type: application/json' \
  -d "{\"name\":\"smoke-resume — Tailor Test Co Staff Platform Engineer\",\"kind\":\"resume\",\"format\":\"markdown\",\"body\":\"# Jane Dev\n\n- Platform, Go, SQLite\",\"derived_from_document_id\":\$doc,\"listing_id\":\$lst}" \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
curl -sfS -X PATCH "$BASE/tailor-requests/\$req" -H 'Content-Type: application/json' \
  -d "{\"status\":\"ready\",\"result_document_id\":\$new,\"finished_at\":\$(date +%s)}" >/dev/null
STUB
cat >"$DISPATCH_DIR/tailor-stub-abandon" <<'STUB'
#!/usr/bin/env bash
# Walks away without writing anything — a crashed or budget-capped session.
exit 0
STUB
chmod +x "$DISPATCH_DIR/tailor-stub-ok" "$DISPATCH_DIR/tailor-stub-abandon"

run_tailor() { # $1 = stub to use
  JOB_STORE_URL="$BASE" JOB_TAILOR_CLAUDE_BIN="$DISPATCH_DIR/$1" \
    bash "$REPO_DIR/scripts/job-tailor-dispatch.sh" 2>&1
}
request_status() { curl -sfS "$BASE/tailor-requests/$1" | python3 -c 'import json,sys; print(json.load(sys.stdin)["status"])'; }

OUT=$(run_tailor tailor-stub-ok) && TAILOR_CODE=0 || TAILOR_CODE=$?
[[ "$TAILOR_CODE" == "0" ]] && pass "a completed tailor request succeeds" || fail "tailor dispatch failed: $OUT"
[[ "$(request_status "$TREQ_ID")" == "ready" ]] && pass "the request reaches ready" || fail "request is $(request_status "$TREQ_ID"), want ready"
grep -q 'The full job description' "$TAILOR_LOG" && pass "the prompt carries the full description" || fail "prompt lacks the description section"
grep -q 'the serving stack, the autoscaler' "$TAILOR_LOG" && pass "the description text itself is inlined" || fail "prompt has the header but not the body"
grep -q 'lead with the platform work' "$TAILOR_LOG" && pass "the user's instructions are inlined" || fail "prompt dropped the user instructions"
grep -q 'NEVER MODIFY THE SOURCE DOCUMENT' "$TAILOR_LOG" && pass "the prompt forbids touching the source document" || fail "prompt lost the never-modify rule"
grep -q 'You may NOT invent' "$TAILOR_LOG" && pass "the prompt states the no-fabrication rule" || fail "prompt lost the truthfulness rule"

# The whole point of writing a new row: the original must come out unchanged.
curl -sfS "$BASE/documents/$DID" | grep -q 'Go, SQLite' && pass "the source document is untouched" || fail "the source document was modified"
DERIVED=$(curl -sfS "$BASE/documents?derived_from=$DID")
[[ "$(count_key "$DERIVED" documents)" == "1" ]] && pass "the tailored copy is traceable to its source" || fail "derived_from lookup: $DERIVED"

OUT=$(run_tailor tailor-stub-ok) && TAILOR_CODE=0 || TAILOR_CODE=$?
[[ "$TAILOR_CODE" == "0" ]] && pass "an empty queue is a clean skip" || fail "idle tick should exit 0: $OUT"
grep -q 'no pending tailor requests' <<<"$OUT" && pass "the idle tick launches no session" || fail "idle text: $OUT"

curl -sfS -X POST "$BASE/documents/$DID/tailor" -H 'Content-Type: application/json' \
  -d "{\"listing_id\":$TAILOR_LID}" >/dev/null
ABANDONED_ID=$(curl -sfS "$BASE/tailor-requests?status=pending" | python3 -c \
  'import json,sys; print(json.load(sys.stdin)["tailor_requests"][0]["id"])')
OUT=$(run_tailor tailor-stub-abandon) && TAILOR_CODE=0 || TAILOR_CODE=$?
[[ "$TAILOR_CODE" != "0" ]] && pass "an abandoned request fails the run" || fail "dispatch reported success after writing nothing: $OUT"
STATE=$(request_status "$ABANDONED_ID")
[[ "$STATE" == "failed" ]] && pass "an abandoned request ends failed, never left running" || fail "abandoned request is $STATE, want failed"
curl -sfS "$BASE/tailor-requests/$ABANDONED_ID" | grep -q '"error":"[^"]' \
  && pass "the failure carries a real error string" || fail "failed request has no error text"

echo "==> ALL SMOKE CHECKS PASSED"
