#!/usr/bin/env bash
# job-radar dispatcher: find open ROLES for DUE sources and record them in
# job-store (:8311). Runs as a scheduler shell job. Per-source cadence is
# enforced by the store's next_run_at, so on most ticks most sources are not due.
#
# Cost control: we only launch a Claude session when at least one source is due,
# and that session is hard-capped with --max-budget-usd. Board sources (one cheap
# fetch, ideally of an ATS JSON API) and research sources (a small web sweep) are
# handled in the same session.
#
# The service never branches on `kind` — kind + cadence_hours + model IS the
# effort-tier system, and every dispatch semantic lives in the prompt below. If
# you are changing how a kind behaves, this file is the only place to change it.
set -euo pipefail

export PATH="$HOME/.local/share/mise/shims:$HOME/.local/share/mise/installs/node/25.8.2/bin:$PATH"
STORE="${JOB_STORE_URL:-http://localhost:8311}"
BUDGET="${JOB_RADAR_MAX_BUDGET_USD:-50}"
MODEL="${JOB_RADAR_MODEL:-sonnet}"
# The session runner is a variable so the "did anything actually run" guard at the
# bottom can be tested without paying for a model call. scripts/e2e-smoke.sh
# drives this script with stub runners that reschedule none, one, and all of the
# sources they are handed.
CLAUDE_BIN="${JOB_RADAR_CLAUDE_BIN:-claude}"

if ! due=$(curl -sfS "$STORE/sources?due=1"); then
  echo "job-radar: cannot reach job-store at $STORE — no dispatch this run" >&2
  exit 1
fi

# Count the sources the store actually returned, rather than counting how many
# times the text '"id"' appears in the response. Counting with `grep -o | wc -l`
# is the trap event-radar fell into: an empty due list makes grep exit 1, which
# under `set -o pipefail` and `set -e` kills this script before it prints
# anything. The ordinary quiet tick — nothing due, nothing to do — then reaches
# the scheduler as a bare `exit status 1`, indistinguishable from the store being
# down, and the "no sources due" branch below can never run at all.
if ! read -r count due_ids <<<"$(printf '%s' "$due" | python3 -c '
import json, sys
sources = json.load(sys.stdin).get("sources") or []
print(len(sources), ",".join(str(source["id"]) for source in sources))
')"; then
  echo "job-radar: job-store returned a source list this script cannot read: ${due:0:300}" >&2
  exit 1
fi

# This gate is the cost control: no due source means no session, and no session
# means no spend. Everything below it costs money, so nothing below it may run on
# a quiet tick.
if [[ "$count" == "0" ]]; then
  echo "job-radar: no sources due — skipping (no Claude session launched)"
  exit 0
fi
echo "job-radar: $count source(s) due — launching capped session (\$$BUDGET, $MODEL)"

read -r -d '' INSTRUCTIONS <<'EOF' || true
You are the job-radar dispatcher. Find CURRENTLY OPEN roles for the sources that
are due and record them in job-store at http://localhost:8311. Do NOT apply to
anything, do NOT email anyone, and do NOT ask questions — work autonomously, then
stop.

THE MOST IMPORTANT THING YOU DO IS CAPTURE THE FULL JOB DESCRIPTION. A listing
without the complete posting text is nearly worthless: tailoring a resume needs the
real requirements, a relevance score computed from a job title is a guess, and an
application agent filling in a form has nothing else to work from. "description"
must be the COMPLETE text of the posting — responsibilities, requirements,
qualifications, compensation, benefits, location/visa policy — as a human would
read it on the page. Not a summary. Not the first paragraph. Not the board's
one-line blurb. Not a rewrite in your own words. Read it, keep it whole, store it.

Role families are a CLOSED vocabulary of six: Engineering, AI, Data, Product,
Design, Leadership. GET http://localhost:8311/role-families for what each one
covers. Every listing you post must use exactly one of them, or "" if you truly
cannot tell (that surfaces as "Unsorted", which is honest; guessing is not). The
store rejects anything else with a 400 whose message names the whole vocabulary —
read that message and retry with a canonical value, never guess a new one. Put the
specific label ("MLE", "SRE", "solutions architect") in "tags"; the store also
appends a more-specific raw label to tags for you.

Steps:
1. The sources you are working are listed at the END of this prompt, under "The
   source(s) due right now". The dispatcher read them from the store and only
   started you because that read was non-empty, so do not fetch that list again
   and never finish on the grounds that nothing is due.
2. For EACH due source (note its "id", "kind", "target", "company", "location",
   "role_families", "seniorities", "keywords", "notes"). A source's "notes" carry
   extraction rules for that specific board — read them and follow them, they
   override the general guidance below:

   - kind "board": fetch "target" and extract EVERY currently-open role on it,
     then filter (see step 3). If the board is hosted by a known ATS, use its
     JSON API instead of scraping the rendered HTML. Two reasons, and the second
     is the bigger one: the HTML is usually a client-side-rendered shell whose
     openings are not in the bytes you get back, so scraping it silently reports
     an empty board for a company that is hiring — AND the JSON APIs hand you the
     FULL description of every role inline, in the same single call, which is
     exactly the text the listing is required to carry:
       * Greenhouse — board URL contains `boards.greenhouse.io/<token>` or
         `job-boards.greenhouse.io/<token>`:
           curl -s "https://boards-api.greenhouse.io/v1/boards/<token>/jobs?content=true"
         → {"jobs":[{id,title,absolute_url,location:{name},updated_at,content,…}]}
         `content=true` is not optional here — without it there is no body at all.
         `content` comes back HTML-ESCAPED (`&lt;p&gt;`, `&amp;`): unescape it
         (html.unescape / a strip-tags pass) before storing, or the description is
         a wall of entities nobody can read and no resume can be tailored against.
       * Lever — `jobs.lever.co/<token>`:
           curl -s "https://api.lever.co/v0/postings/<token>?mode=json"
         → a JSON ARRAY of {text, hostedUrl, applyUrl, categories:{location,team,
         commitment}, createdAt (epoch MILLIseconds — divide by 1000),
         descriptionPlain, lists:[{text,content}], additionalPlain}.
         The full posting is `descriptionPlain` PLUS every entry of `lists`
         (that is where "Requirements", "What you'll do" and the rest live) plus
         `additionalPlain`. Concatenate them all — taking only descriptionPlain,
         or only the first list, drops the requirements section, which is the part
         that actually matters downstream.
       * Ashby — `jobs.ashbyhq.com/<token>`:
           curl -s "https://api.ashbyhq.com/posting-api/job-board/<token>"
         (append `?includeCompensation=true` for pay bands)
         → {"jobs":[{title, location, employmentType, jobUrl, applyUrl,
         publishedAt, descriptionPlain, descriptionHtml, compensation…}]}
         `descriptionPlain` is the full body; prefer it over descriptionHtml.
       * Workable — `apply.workable.com/<token>`:
           curl -s "https://apply.workable.com/api/v1/widget/accounts/<token>?details=true"
         → {"jobs":[{title, url, application_url, location, created_at,
         description, requirements, benefits}]} — concatenate description +
         requirements + benefits; each is a separate HTML field.
     A 404 from one of these means the token is wrong or the company left that
     ATS — say so in your summary rather than falling back to guessing roles.
     Only if the target is NOT one of these should you WebFetch the page, and then
     you must open each individual posting to get its body: a board index page
     carries titles and a teaser line, never the full text.

   - kind "research": run a few focused WebSearch queries for roles matching the
     "target" description in "location", across its "role_families",
     "seniorities" and "keywords". Then FOLLOW THROUGH to the actual posting and
     read it: "url" and "apply_url" must point at a real job posting, never at a
     search-results page, an aggregator's redirect stub, or a company careers
     index, and "description" must be the text of that posting — never a search
     result's snippet, and never the two lines a search engine chose to show you.
     A listing whose url is a search page is worse than no listing — it cannot be
     applied to and it poisons the dedupe memory under a sig that will never be
     revisited. If you cannot reach the real posting, drop the row and say so.

   - kind "scout": this one looks for new SOURCES, not roles. Post NO listings for
     it. See step 5.

3. Filter every candidate role against the source before posting it:
   - "role_families" — skip roles outside them (an empty list means no filter).
   - "seniorities" — freeform ("senior", "staff", "principal"); skip a role whose
     level is clearly below the lowest one asked for. New-grad and intern rows are
     noise on a senior source.
   - "location" — a role must be in that geography or genuinely remote-eligible
     for it. "Remote" on a posting that then says "must sit in EMEA" is not
     remote for a US source; read the posting, not the badge.
   - "keywords" — must-have terms; if present, require them.
   - Post only roles that are OPEN NOW. Skip anything the page marks closed,
     filled, paused, or "evergreen pipeline" with no real opening behind it.

4. POST each surviving role to http://localhost:8311/listings as JSON:
     {
       "title":           "Staff Software Engineer, Inference",
       "company":         "Example AI",
       "location":        "San Francisco, CA / Remote (US)",
       "remote":          true,
       "employment_type": "full-time",          // 'full-time'|'contract'|'internship'|''
       "seniority":       "staff",
       "role_family":     "AI",                 // one of the six, or ""
       "tags":            ["inference","cuda"], // specific labels, lowercase
       "description":     "THE COMPLETE POSTING TEXT — every section: what the
                           team does, responsibilities, requirements, nice-to-
                           haves, the comp paragraph, benefits, and the location /
                           visa / clearance policy. Plain text or light markdown,
                           whitespace tidied, nothing dropped. This will typically
                           be several thousand characters. Do NOT summarize it and
                           do NOT paraphrase it: a downstream agent tailoring a
                           resume needs the employer's own words and its exact
                           vocabulary.",
       "comp_min":        220000,               // parsed integers, 0 if not posted
       "comp_max":        290000,
       "comp_currency":   "USD",
       "comp_raw":        "$220,000 - $290,000 + equity",
       "url":             "https://…/jobs/123",     // the posting itself
       "apply_url":       "https://…/jobs/123/apply", // where the application is submitted
       "posted":          "2026-08-01",         // ISO string; or posted_at epoch seconds
       "closes":          "",                   // ISO string; or closes_at epoch seconds
       "source_id":       12,                   // THIS source's id, from the list below
       "source_name":     "example-ai-board",   // THIS source's name
       "relevance":       78                    // 0-100, your own honest score
     }
   - "comp_raw" keeps the compensation EXACTLY as the posting words it, including
     the currency symbol, the range dash, and any "+ equity" or "+ bonus" tail.
     "comp_min"/"comp_max" are your parsed integers of the same string. If the
     posting shows no pay, leave all four empty/0 — do not fill them from
     levels.fyi, from a sibling role, or from your own estimate. An invented band
     is indistinguishable from a posted one once it is a row.
   - "relevance" is 0-100 and it is yours to set honestly: 90+ means this is
     squarely what the source was created to find, 50 means plausible-but-a-reach,
     below 30 means do not post it at all. Do not inflate it to look productive —
     the whole point of the number is to let a human read the top of the list
     first.
   - Do NOT send "status" and do NOT send "notes". Both belong to the user:
     "status" is their pipeline decision (candidate → interested → applying →
     applied → …) and "notes" is their notebook. The upsert path deliberately
     never overwrites status, notes or relevance on an existing row, so re-posting
     a role you already found refreshes its description and comp and leaves their
     decisions alone.
   - Do NOT send "description_fetched_at" — the store stamps it itself whenever it
     writes a non-empty description.
   - The store dedupes by sha1(company|title|location), so re-posting is safe and
     idempotent — run it as often as the cadence says without fear of duplicates.
     That also means company/title/location must be spelled consistently: use the
     company's own name for "company" (not the ATS token, not a subsidiary).
5. CHECK EVERY WRITE FOR A THIN DESCRIPTION. The POST response carries
   "description_thin": true whenever the description it stored is blank or under
   200 characters. The write still succeeded — a partial listing beats a lost one —
   but that flag means the body did not come through, and it is a bug to fix in
   THIS run, not a leftover for the next one:
   - Read the flag on each response. If it is true, go back for the real posting:
     open "url" directly, or re-read the ATS field you skipped (Greenhouse without
     `content=true`, Lever without its `lists`, a board index instead of the
     posting), and re-POST the same company/title/location with the full body. The
     upsert dedupes, so fixing it costs one call and creates no duplicate row.
   - Before you finish a source, audit it:
       curl -s "http://localhost:8311/listings?thin_description=1"
     Anything of yours still listed there is a listing you have not finished.
   - If a posting genuinely has almost no text (a two-line "email us your CV"
     page), leave it and say so explicitly in your summary — an honest short
     posting and a failed fetch look identical in the data, and only you can tell
     them apart.
6. A due source of kind "scout" is different: its job is to grow the radar by
   finding new PLACES TO LOOK, so the board list is not limited to the companies
   that were thought of when it was set up. For a scout:
   - First GET http://localhost:8311/sources (NO filter — you need EVERY status,
     including rejected ones) and read the whole list. Anything already there,
     under any status, is not new. Never re-propose a source whose status is
     "rejected": that is a decision already made, and the store will refuse to
     un-reject it anyway.
   - Search for companies, ATS boards, curated hiring lists and niche job boards
     that match this scout's "target" description in its "location", across its
     "role_families".
   - VERIFY before proposing. Actually fetch the candidate board — through its ATS
     JSON API where there is one (step 2) — and confirm it returns dated, open
     postings that match the target. Pages that look like sources but are not: a
     client-side-rendered careers page that returns only nav chrome, a board whose
     ATS token 404s, a "we're always hiring" page with no postings, a LinkedIn or
     Indeed search URL (never propose one — they are search results, not a board),
     an aggregator that only reposts the boards you already watch.
   - POST each new one to http://localhost:8311/sources as JSON with:
       name (short and specific, e.g. "modal-board"),
       kind ("board" ONLY if you verified a real board that lists openings,
         otherwise "research"),
       target (the board URL for board — prefer the canonical
         jobs.ashbyhq.com/<token> / boards.greenhouse.io/<token> /
         jobs.lever.co/<token> form so the poller can derive the API token —
         or a search spec for research),
       company, location, role_families (from the six), seniorities, keywords,
       cadence_hours: match it to how fast that page actually changes — a big
         company's board that turns over daily is 24, a 30-person startup that
         posts monthly is 168 or more. Do not put everything on 24: every
         approved source is a recurring cost forever.
       status: "proposed", proposed_by: "<this scout's name>",
       notes: what you verified, which API you verified it through, how many
         openings it had on the day you looked, any parse trap, and why it is
         worth watching.
   - Everything you post is INERT until a human approves it: a proposed source is
     never fetched and never runs. So do not enable anything, do not post listings
     for it, and do not act as though it is live. You cannot give yourself work.
   - Be selective. Three or four proposals worth reading beat twenty that need
     triaging.
7. When you finish a source — every kind, scouts included — POST
   http://localhost:8311/sources/<id>/ran to reschedule it by its cadence. The
   dispatcher grades this run by which sources got that mark, so a source you
   worked but never marked reads as a source that never ran.
8. Be economical, but not at the description's expense: one API call per board
   source is usually enough precisely BECAUSE the ATS APIs return the full bodies
   inline. Prefer quality over volume — a board with 400 openings should yield the
   handful that match the source with their complete text, not forty near-misses
   with none. Fewer, complete listings beat more, thin ones every time.
9. Finish with a short plain-text summary: per source, how many listings you added
   and the best one or two by relevance; how many came back description_thin and
   what you did about each; and for a scout, which sources you proposed and which
   candidates you rejected and why.
EOF

# Hand the session the list the gate already measured instead of telling it to
# fetch the same list a second time. The re-fetch is what lost event-radar a full
# day on 2026-08-03: the session ran the GET, was handed sources 3, 4 and 5, and
# answered "GET /sources?due=1 returned an empty list" — its previous command, an
# unrelated lookup, had answered `[]` twice and it reported that instead. Three
# sources went unfetched and the run was graded success. A list already in the
# prompt cannot be confused with another command's output, and one fewer round
# trip is one fewer chance for the session to disagree with the gate that
# launched it.
due_rows=$(printf '%s' "$due" | python3 -c '
import json, sys
for source in json.load(sys.stdin).get("sources") or []:
    print(json.dumps(source, sort_keys=True))
')

PROMPT="$INSTRUCTIONS

## The $count source(s) due right now — this list IS the work

The dispatcher read these from GET $STORE/sources?due=1 immediately before starting
you, and that same read is why you were started at all. Work every row below. The
list is never empty, so \"nothing was due\" is never the right answer: if you think
it is empty, you are reading some other command's output.

$due_rows"

session_status=0
"$CLAUDE_BIN" -p "$PROMPT" \
  --model "$MODEL" \
  --max-budget-usd "$BUDGET" \
  --dangerously-skip-permissions || session_status=$?

# A dispatch that rescheduled nothing did nothing, and reporting that as `success`
# is how a lost day comes to look exactly like a quiet one in the run history. A
# source that ran has a fresh last_run_at (and normally drops off the due list),
# so compare the MARKS PER ID rather than list membership: job cadences are short
# (24h is the default, some boards are shorter), so a source can legitimately come
# due again between the two reads, and membership alone would read that as a
# source that never ran.
if ! after=$(curl -sfS "$STORE/sources?due=1"); then
  echo "job-radar: session finished (exit $session_status) but job-store is unreachable now — cannot tell whether any source ran" >&2
  exit 1
fi

if ! read -r ran_count still_ids <<<"$(printf '%s' "$after" | JOB_RADAR_DUE_BEFORE="$due" python3 -c '
import json, os, sys

def marks(payload):
    return {source["id"]: source.get("last_run_at") for source in payload.get("sources") or []}

before = marks(json.loads(os.environ["JOB_RADAR_DUE_BEFORE"]))
after = marks(json.load(sys.stdin))
ran = [i for i in before if i not in after or after[i] != before[i]]
still = [str(i) for i in sorted(before) if i in after and after[i] == before[i]]
print(len(ran), ",".join(still))
')"; then
  echo "job-radar: cannot compare the due list before and after the session — treating this dispatch as failed" >&2
  exit 1
fi

if [[ "$ran_count" == "0" ]]; then
  echo "job-radar: the session rescheduled NOTHING — all $count source(s) it was handed are still due (ids $due_ids), session exit $session_status. Refusing to report this run as a success." >&2
  exit 1
fi

if [[ -n "$still_ids" ]]; then
  echo "job-radar: partial dispatch — $ran_count source(s) ran, still due: $still_ids"
fi

if [[ "$session_status" != "0" ]]; then
  echo "job-radar: session exited $session_status" >&2
  exit "$session_status"
fi

echo "job-radar: dispatch complete"
