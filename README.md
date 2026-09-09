# job-store

`:8311`. Canonical registry of **where to look for jobs** (`sources`), the **listings** they
yield, the **documents** applied with (resume / cover-letter variants), and the **applications**
made from them.

Deliberately the same shape as [event-store](../event-store) on `:8308`: a source is a place to
look, a poller comes due on its own cadence, and everything a scout finds arrives as `proposed`
for a human to approve. Nothing self-approves.

`CONTRACT.md` is the authoritative spec. This file is how to build and run it.

## ⚠️ Build with `-tags sqlite_fts5`

Search is an FTS5 virtual table, and `mattn/go-sqlite3` only compiles FTS5 into its bundled
SQLite when the build asks for it. A binary built without the tag **compiles and links fine**
and then dies at boot with `no such module: fts5`.

```bash
make check          # gofmt + vet + test + build, all with the tag
go build -tags sqlite_fts5 -o job-store ./cmd/job-store
go test  -tags sqlite_fts5 ./...
```

`deploy.sh` and the `Makefile` always pass it, `Open()` recognizes the failure and names the
flag in the error, and `deploy.sh` smoke-checks `GET /listings?q=` after starting the service —
because a process that is *running* is not the same as a process that *works*.

## Run

```bash
./deploy.sh                       # build, install unit, restart, smoke-check
JOB_STORE_ADDR=:8311 ./job-store  # or run it directly
```

- `JOB_STORE_ADDR` — listen address, default `:8311`
- `JOB_STORE_DATA_DIR` — default `~/.config/job-store`; holds `job-store.db` (WAL) and `blobs/`

## The shape of it

**sources** — where to look. Three kinds (`board`, `research`, `scout`) and the service *never
branches on kind*: kind plus `cadence_hours` plus `model` is the whole effort-tier system, and
the dispatch semantics live in the poller prompt.

The approval gate lives in the query, not in the caller:

```sql
AND status='active' AND enabled=1 AND next_run_at<=?
```

`ListSources(SourceFilter{OnlyDue: true})` folds that in as one inseparable clause, so a caller
cannot forget it and a scout literally cannot give itself work. `POST /sources` refreshes the
description of where to look; `PATCH /sources/{id}` is the only channel that can change the
verdict.

**listings** — what the pollers found, deduped by `sig = sha1(company|title|location)`. The
pipeline is `candidate → interested → applying → applied → rejected|offer`, with `dismissed`
and `closed` as the two ways out. `interested` is the queue an application agent reads.

A re-poll refreshes what the posting says and never `status`, `notes` or `relevance`. Delete is
soft **because the row is the dedupe memory** — purge only with `?hard=true`.

`description` is the *full* posting text and everything downstream depends on it, so a thin one
is made visible rather than allowed to pass: a listing under 200 characters is still stored
(a partial listing beats a lost one) but comes back with `"description_thin": true`, and
`GET /listings?thin_description=1` lists every one of them for auditing.

**role families** — six canonical values (`Engineering`, `AI`, `Data`, `Product`, `Design`,
`Leadership`) with a ~280-entry synonym table. `NormalizeRoleFamily` resolves a freeform title
by longest-span n-gram, ties broken by display rank, and never guesses a default. Anything more
specific than the family is kept as a tag: `"MLE"` → family `AI`, tags `["mle"]`.

One rule in that table is load-bearing and has a test guarding it: an Engineering entry is a
bare specialism (`platform`, `infrastructure`), never that specialism plus `engineer`. Adding
`"platform engineer"` creates a two-word tie that Engineering wins on rank, and every
*ML Platform Engineer* silently lands in Engineering.

**documents** — a document is two things at once: the original bytes exactly as uploaded, and
the extracted text an agent can read and tailor. Both are kept, because the file an employer
receives should be the one the user made. Originals are content-addressed at
`<data dir>/blobs/<sha256[:2]>/<sha256>`.

`POST /documents/upload` accepts `.pdf`, `.docx`, `.txt`, `.md`, `.html` (10 MB cap). PDFs go
through `pdftotext -layout`; docx is read with `archive/zip` + `encoding/xml`, stdlib only, so
the single-dependency property holds. **A failed extraction fails the upload**, with a message
naming the cause — a resume that silently became blank would be discovered by an agent
submitting an empty application.

**tailor_requests** — queued, not synchronous: `POST /documents/{id}/tailor` answers `202` and a
dispatcher runs the agent. Tailoring never overwrites the original; the dispatcher writes a new
document carrying `derived_from_document_id` and `listing_id`. A request against a listing with
no description is a `400` — guessing from a job title produces exactly the generic letter this
feature exists to avoid.

**applications** — the tracking layer is real; *submitting* is still phase 3, and
`POST /applications/{id}/submit` answers `501` naming what is missing rather than pretending.

An application carries **two states that must never collapse into one field**:

- `stage` — where you stand with the **employer**: `drafting → ready → submitted → acknowledged
  → screen → interview → onsite → offer`, plus terminal `rejected`, `withdrawn`, `ghosted`.
  `ghosted` is a real stage, not a missing value.
- `agent_status` — what the **automation** did: `draft`, `ready`, `submitted`, `failed`. This is
  the field once called `status`; an agent failing to fill a form and a company rejecting you
  are not the same event. The old spelling is **refused with a 400 naming both replacements**,
  never ignored — on a create, on a patch, and as `?status=` on the list.

Creating an application moves its listing to `applied` in the same transaction, once — and the
listing never moves again: a later `PATCH /listings/{id}` carrying `status` on a listing that has
an application is a 400 pointing at the application's stage. Everything else on that listing is
still editable; only the pipeline question has an owner now.

Every application row carries its own summary, all derived on read, on the list route as well as
the detail one: `resume_drifted`, `email_count` (confirmed mail only), `task_count`,
`open_task_count` and `last_activity_at`. `last_activity_at` comes from the timeline and the
confirmed mail, **never `updated_at`** — an agent retrying a write is not a company writing back.
`open_task_count` is `null` when noteboard could not be read, and never `0`.

**application_events** — the timeline, append-only. `PATCH /applications/{id}` writing a new
`stage` appends the event **in the same transaction**, so a stage cannot move without leaving a
trace: if the event cannot be written the stage does not move either. `POST
/applications/{id}/events` records a note — something that happened without a stage change — and
deliberately cannot write a `stage_to`, which would let the timeline claim a move that never
happened.

**application_emails** — the join to [mailstack](../mailstack) on `:8195`, which owns the
messages. The key is `(application_id, account_id, message_id)` — mailstack's own per-account
id, always present. `rfc_message_id` is carried for cross-account dedup and is **never** the
key: mailstack's own `backend/backend.go` says it is optional per RFC 5322 §3.6.4 and that no
caller may key on it blindly. A matcher may only **propose** a link and is refused outright if
it asks for `linked`; a human confirms it. **Only a `linked` email may drive a stage change**,
enforced in the one place mail can move a stage — `POST /application-emails/{id}/stage`.

**application_tasks** — noteboard todo ids and nothing else. `GET /applications/{id}?expand=tasks`
reads each todo through noteboard (`NOTEBOARD_URL`, default `http://localhost:8191`) at request
time and passes its record through unchanged. **An unreachable noteboard is an explicit error,
never an empty list** — an application silently showing zero outstanding tasks is worse than one
that admits it cannot tell. `POST /applications/{id}/tasks/standard` creates the standard
follow-ups *in noteboard*, tagged `["jobs","personal"]`, and stores the ids noteboard hands back;
with noteboard down it is a `502` and links nothing.

**which resume was actually sent** — `resume_body_sha256` pins the resume's *content* when
`agent_status` reaches `submitted`, and is never rewritten. `GET /applications/{id}` derives
`resume_drifted` on every read, so improving your resume next month shows up as drift instead of
quietly claiming the employer holds the new version.

## Routes

See the table in `CONTRACT.md`. Errors are `{"error":"…"}`. Every 400 raised by a **store
sentinel** enumerates the valid values, so an agent reading one can retry without guessing —
`GET /listings?role_family=nonsense` answers `invalid role family "nonsense": use one of
Engineering, AI, Data, Product, Design, Leadership`. That is the scope `server.go`'s own comment
above `respondStoreError` states, and it is narrower than "every 400": a malformed body answers
`bad json: invalid character 'b' looking for beginning of object key string`, and a range rule
answers `grace_days must not be negative`. Neither enumerates anything, because neither has a
vocabulary to enumerate.

## Poller

`~/bin/job-radar-dispatch`, installed by `deploy.sh` from `scripts/job-radar-dispatch.sh` and
run by a scheduler shell job. It reads `GET /sources?due=1`, exits 0 without launching a session
when nothing is due, and grades itself on `last_run_at` deltas per source id rather than on exit
status.

## Tests

```bash
make test
```

The suite is mostly safety properties, each one standing in for a way this could quietly go
wrong: a proposed source is never due, approving one makes it due, a re-poll never overwrites a
decision, a prune never touches a listing the user has touched, an application cannot name a
listing that does not exist, an extraction failure never becomes an empty body, `is_default` is
exclusive per kind, and a malformed search is a rejection rather than an empty page of results.

## License

MIT — see [LICENSE](LICENSE).
