#!/usr/bin/env bash
# job-tailor dispatcher: drain the tailor_requests queue in job-store (:8311).
# A user clicks "tailor this resume for this role" in the UI, the service queues a
# row, and this script is what actually runs an agent against it.
#
# It backs a UI action, so it runs on a short cron (*/2 or */5) rather than daily.
# That makes the idle path the hot path: this script is invoked hundreds of times
# a day and must cost nothing on almost all of them. Same discipline as
# job-radar-dispatch — count with python3, gate before spending, grade on what
# actually moved — with one extra obligation the radar does not have: a request it
# marks `running` must never be left that way, because a stuck `running` row is a
# spinner that never stops in the UI.
set -euo pipefail

export PATH="$HOME/.local/share/mise/shims:$HOME/.local/share/mise/installs/node/25.8.2/bin:$PATH"
STORE="${JOB_STORE_URL:-http://localhost:8311}"
MODEL="${JOB_TAILOR_MODEL:-sonnet}"
# Per session, and one session per request. Lower than the radar's cap because
# this runs every couple of minutes and the job is bounded: rewrite one document
# against one description.
BUDGET="${JOB_TAILOR_MAX_BUDGET_USD:-10}"
# The session runner is a variable so the queue handling can be tested without
# paying for a model call — scripts/e2e-smoke.sh drives this script with stubs
# that finish a request, and that abandon one.
CLAUDE_BIN="${JOB_TAILOR_CLAUDE_BIN:-claude}"

now() { date +%s; }

# --- the in-flight ledger -----------------------------------------------------
# Every request this run moves to `running` is recorded here and cleared when it
# reaches a terminal state. The EXIT trap closes out whatever is left, so a crash,
# a `timeout` kill from the scheduler, or an unhandled error can never leave a row
# spinning forever — the same close-out the scheduler does for its own interrupted
# runs.
IN_FLIGHT=()

patch_request() { # $1 = id, rest = JSON object body
  curl -sfS -X PATCH "$STORE/tailor-requests/$1" \
    -H 'Content-Type: application/json' -d "$2" >/dev/null
}

json_object() { # key value key value … → a JSON object, values encoded safely
  python3 -c '
import json, sys
args = sys.argv[1:]
print(json.dumps({args[i]: (int(args[i + 1]) if args[i + 1].isdigit() and args[i] in
      ("started_at", "finished_at", "result_document_id") else args[i + 1])
      for i in range(0, len(args), 2)}))
' "$@"
}

release() { # $1 = id — this request reached a terminal state, stop tracking it
  local remaining=()
  local tracked
  for tracked in ${IN_FLIGHT+"${IN_FLIGHT[@]}"}; do
    [[ "$tracked" == "$1" ]] || remaining+=("$tracked")
  done
  IN_FLIGHT=(${remaining+"${remaining[@]}"})
}

close_out_in_flight() {
  local exit_status=$?
  local id status
  for id in ${IN_FLIGHT+"${IN_FLIGHT[@]}"}; do
    status=$(curl -sfS "$STORE/tailor-requests/$id" 2>/dev/null \
      | python3 -c 'import json,sys; print(json.load(sys.stdin).get("status",""))' 2>/dev/null) || status=""
    [[ "$status" == "running" ]] || continue
    echo "job-tailor: closing out request $id left running by an interrupted dispatch (exit $exit_status)" >&2
    patch_request "$id" "$(json_object status failed \
      error "dispatcher exited before this request finished (exit $exit_status) — no result document was written" \
      finished_at "$(now)")" 2>/dev/null || true
  done
}
trap close_out_in_flight EXIT

# --- the idle gate ------------------------------------------------------------
if ! pending=$(curl -sfS "$STORE/tailor-requests?status=pending"); then
  echo "job-tailor: cannot reach job-store at $STORE — no dispatch this run" >&2
  exit 1
fi

# Count the rows the store actually returned. `grep -o | wc -l` cannot do this:
# grep exits 1 on an empty list, and under `set -euo pipefail` that kills the
# script before it can say "nothing pending" — which, on a queue polled every two
# minutes, would turn the ordinary idle tick into hundreds of indistinguishable
# `exit status 1` rows in the scheduler's run history.
if ! read -r pending_count pending_ids <<<"$(printf '%s' "$pending" | python3 -c '
import json, sys
requests = json.load(sys.stdin).get("tailor_requests") or []
print(len(requests), " ".join(str(request["id"]) for request in requests))
')"; then
  echo "job-tailor: job-store returned a queue this script cannot read: ${pending:0:300}" >&2
  exit 1
fi

# The cost control. This is the branch taken on nearly every invocation, so
# nothing above it may cost money and nothing below it may run without work.
if [[ "$pending_count" == "0" ]]; then
  echo "job-tailor: no pending tailor requests — skipping (no Claude session launched)"
  exit 0
fi
echo "job-tailor: $pending_count pending request(s) — ids $pending_ids"

read -r -d '' INSTRUCTIONS <<'EOF' || true
You are the job-tailor dispatcher. You are given ONE resume or cover letter and
ONE job posting, both inlined below in full. Rewrite the document for that
specific role and save the rewrite as a NEW document in job-store at
http://localhost:8311. Work autonomously, do not ask questions, then stop.

WRITE A NEW DOCUMENT. NEVER MODIFY THE SOURCE DOCUMENT.
Create it with:
  POST http://localhost:8311/documents
  {
    "name":   "<source document name> — <company> <job title>",
    "kind":   "<the source document's kind: resume or cover_letter>",
    "format": "markdown",
    "body":   "<the full tailored document>",
    "derived_from_document_id": <the source document's id>,
    "listing_id": <the listing's id>,
    "notes": "<one line: what you emphasised and why>"
  }
You must NOT send PATCH to /documents/<source id>, and you must NOT set
"is_default". Saying it twice because it is the property most likely to be
violated and the most damaging when it is: THE SOURCE DOCUMENT IS THE USER'S
ORIGINAL AND MUST COME OUT OF THIS UNCHANGED. Every tailored copy is a new row
pointing back at it through derived_from_document_id; that is what makes the
variants traceable and the original safe.

TRUTHFULNESS IS A HARD RULE, NOT A PREFERENCE.
You may: reorder sections and bullets so the most relevant work comes first;
re-emphasise projects the posting cares about; adopt the posting's own vocabulary
where it genuinely describes the same thing ("distributed systems" → "large-scale
distributed systems" only if the source says so); drop or compress material that
is irrelevant to this role; tighten wording.
You may NOT invent, and this list is exhaustive of what is forbidden: employers,
job titles, dates, durations, degrees, certifications, clearances, technologies
the person has not used, team sizes, revenue or performance numbers, or any metric
not present in the source document. Do not upgrade "contributed to" into "led".
Do not turn a side project into a job. Do not stretch an end date to close a gap.
If the posting asks for something the source does not show, leave it out — a gap
the reader can see is survivable, a fabrication found in an interview is not.

KEEP THE SHAPE.
Same overall length (within ~10%), same section structure, same document kind and
voice. A tailored resume that grew a new job, a new section, or a page is not a
customization — it is a different document, and a false one. If you find yourself
adding rather than reordering, stop and reconsider.

Use the FULL job description inlined below — it is the whole posting, not a
summary — and follow any "Instructions from the user" verbatim; they override the
general guidance here.

When the new document is created, read its "id" from the response and finish with:
  PATCH http://localhost:8311/tailor-requests/<request id>
  {"status":"ready","result_document_id":<new document id>,"finished_at":<epoch seconds>}
Do not mark it ready without that id — the dispatcher checks, and a "ready" row
with no result document is a request that has to be re-run by hand.

Finish with two or three plain-text lines: what you moved to the front, what you
cut, and anything the posting asked for that the source genuinely does not have.
EOF

session_status=0
failures=0
for request_id in $pending_ids; do
  request=$(curl -sfS "$STORE/tailor-requests/$request_id") || {
    echo "job-tailor: request $request_id vanished from the store before it could be started" >&2
    failures=$((failures + 1))
    continue
  }
  if ! read -r document_id listing_id <<<"$(printf '%s' "$request" | python3 -c '
import json, sys
request = json.load(sys.stdin)
print(request["document_id"], request["listing_id"])
')"; then
    echo "job-tailor: request $request_id is unreadable: ${request:0:200}" >&2
    failures=$((failures + 1))
    continue
  fi
  instructions=$(printf '%s' "$request" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("instructions") or "(none — use your judgement)")')

  # Claim it before doing any work, so the UI stops showing it as queued and a
  # second dispatcher tick cannot pick up the same row.
  patch_request "$request_id" "$(json_object status running started_at "$(now)")"
  IN_FLIGHT+=("$request_id")

  fail_request() { # $1 = error string
    echo "job-tailor: request $request_id failed — $1" >&2
    patch_request "$request_id" "$(json_object status failed error "$1" finished_at "$(now)")"
    release "$request_id"
    failures=$((failures + 1))
  }

  if ! document=$(curl -sfS "$STORE/documents/$document_id"); then
    fail_request "source document $document_id could not be read from the store"
    continue
  fi
  if ! listing=$(curl -sfS "$STORE/listings/$listing_id"); then
    fail_request "listing $listing_id could not be read from the store"
    continue
  fi

  # The full job description is the whole input to this job. A listing whose
  # description never got fetched cannot be tailored against — guessing from a job
  # title produces exactly the generic letter this feature exists to avoid — so
  # fail loudly and name the fix rather than burning a session on a title.
  if ! read -r description_length <<<"$(printf '%s' "$listing" | python3 -c \
    'import json,sys; print(len(json.load(sys.stdin).get("description") or ""))')"; then
    fail_request "listing $listing_id is unreadable"
    continue
  fi
  if [[ "$description_length" -lt 200 ]]; then
    fail_request "listing $listing_id has a thin description ($description_length chars) — re-poll its source for the full posting text before tailoring against it"
    continue
  fi

  # Inline both documents in the prompt. Same reason the radar inlines its due
  # list: a session told to go and fetch its own inputs can misread an empty or
  # unrelated response and proceed anyway, and here that would mean rewriting a
  # resume against nothing.
  context=$(printf '%s' "$request" | JOB_TAILOR_DOCUMENT="$document" JOB_TAILOR_LISTING="$listing" \
    JOB_TAILOR_USER_INSTRUCTIONS="$instructions" python3 -c '
import json, os, sys

request = json.load(sys.stdin)
document = json.loads(os.environ["JOB_TAILOR_DOCUMENT"])
listing = json.loads(os.environ["JOB_TAILOR_LISTING"])

print("## The request — this IS the work")
print()
print("request id: %d" % request["id"])
print("source document id: %d   (NEVER modify this document)" % document["id"])
print("listing id: %d" % listing["id"])
print()
print("### Instructions from the user")
print(os.environ["JOB_TAILOR_USER_INSTRUCTIONS"])
print()
print("### The source document — name %r, kind %r, format %r" % (
    document.get("name"), document.get("kind"), document.get("format")))
print()
print(document.get("body") or "")
print()
print("### The role — %s at %s (%s)" % (
    listing.get("title"), listing.get("company"), listing.get("location")))
print("url: %s" % (listing.get("url") or ""))
print("compensation as posted: %s" % (listing.get("comp_raw") or "(not posted)"))
print()
print("### The full job description")
print()
print(listing.get("description") or "")
')

  echo "job-tailor: request $request_id — tailoring document $document_id for listing $listing_id (\$$BUDGET, $MODEL)"
  request_status=0
  "$CLAUDE_BIN" -p "$INSTRUCTIONS

$context" \
    --model "$MODEL" \
    --max-budget-usd "$BUDGET" \
    --dangerously-skip-permissions || request_status=$?

  # Grade this request on the row, not on the session's exit code: a session can
  # exit 0 having written nothing, and it can exit non-zero after the document
  # landed.
  if ! after=$(curl -sfS "$STORE/tailor-requests/$request_id"); then
    fail_request "job-store became unreachable after the session (exit $request_status) — cannot tell whether a document was written"
    continue
  fi
  if ! read -r after_status result_document_id <<<"$(printf '%s' "$after" | python3 -c '
import json, sys
request = json.load(sys.stdin)
print(request.get("status"), request.get("result_document_id") or 0)
')"; then
    fail_request "cannot read request $request_id back after the session"
    continue
  fi

  if [[ "$after_status" == "ready" && "$result_document_id" != "0" ]]; then
    release "$request_id"
    echo "job-tailor: request $request_id ready — document $result_document_id"
    [[ "$request_status" == "0" ]] || session_status=$request_status
    continue
  fi
  if [[ "$after_status" == "failed" ]]; then
    release "$request_id"
    failures=$((failures + 1))
    echo "job-tailor: request $request_id was marked failed by the session" >&2
    continue
  fi
  if [[ "$after_status" == "ready" ]]; then
    fail_request "the session marked this ready with no result_document_id — there is no tailored document to show"
    continue
  fi
  fail_request "the session exited $request_status without finishing the request (still $after_status, no result document)"
done

# --- grade the run ------------------------------------------------------------
# The queue is the scoreboard. Anything still pending was never picked up, and
# anything still running was abandoned mid-flight (the EXIT trap will close those
# out as failed on the way down, but the run itself is a failure either way).
if ! remaining=$(curl -sfS "$STORE/tailor-requests"); then
  echo "job-tailor: job-store is unreachable now — cannot grade this dispatch" >&2
  exit 1
fi
if ! read -r stuck_count stuck_ids <<<"$(printf '%s' "$remaining" | JOB_TAILOR_BATCH="$pending_ids" python3 -c '
import json, os, sys
batch = {int(i) for i in os.environ["JOB_TAILOR_BATCH"].split()}
stuck = sorted(str(r["id"]) for r in (json.load(sys.stdin).get("tailor_requests") or [])
               if r["id"] in batch and r.get("status") in ("pending", "running"))
print(len(stuck), ",".join(stuck))
')"; then
  echo "job-tailor: cannot read the queue back — treating this dispatch as failed" >&2
  exit 1
fi

if [[ "$stuck_count" != "0" ]]; then
  echo "job-tailor: $stuck_count of $pending_count request(s) never finished (ids $stuck_ids) — they are being closed out as failed. Refusing to report this run as a success." >&2
  exit 1
fi

if [[ "$failures" != "0" ]]; then
  echo "job-tailor: $failures of $pending_count request(s) failed — see the error on each row" >&2
  exit 1
fi

if [[ "$session_status" != "0" ]]; then
  echo "job-tailor: every request finished, but a session exited $session_status" >&2
  exit "$session_status"
fi

echo "job-tailor: $pending_count request(s) tailored"
