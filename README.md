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

**applications** — phase 3. The table and CRUD are real so the pipeline has somewhere to write;
`POST /applications/{id}/submit` answers `501` naming what is missing rather than pretending.

## Routes

See the table in `CONTRACT.md`. Errors are `{"error":"…"}` and every 400 enumerates the valid
values, so an agent reading one can retry without guessing.

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
