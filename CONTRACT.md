# job-store — API contract

`:8311`. Canonical registry of **where to look for jobs** (`sources`), the **listings** they
yield, the **documents** applied with (resume / cover letter variants), and the
**applications** made from them.

Deliberately the same shape as event-store on `:8308`: a source is a place to look, a poller
comes due on its own cadence, and everything a scout finds arrives as `proposed` for a human
to approve. Nothing self-approves.

Module `github.com/kayushkin/job-store`, root package `jobstore`, binary `cmd/job-store`.
Env: `JOB_STORE_ADDR` (default `:8311`), `JOB_STORE_DATA_DIR` (default `~/.config/job-store`).
SQLite at `<data dir>/job-store.db`, WAL, `?_foreign_keys=on`.

All timestamps are epoch seconds (`INTEGER`, 0 = unset). Arrays are JSON-encoded `TEXT`.

Booleans are `INTEGER` 0/1 **in SQLite** and real JSON booleans **over HTTP** — `remote` and
`is_default` are stored as `1` and sent as `true`. The storage type is not the wire type; a
poller sending `"remote": 1` gets a 400, which is the right answer. Read every table below as
the storage layer.

---

## Role families — the vocabulary

Six canonical values, in display order (which is also the tiebreak rank):

| Value | Covers |
|---|---|
| `Engineering` | backend, frontend, full stack, platform, infrastructure, security, mobile |
| `AI` | ML engineering, research engineering, applied AI, LLM/agents |
| `Data` | data engineering, analytics, data science, BI |
| `Product` | product management, program management, TPM |
| `Design` | product design, UX, UI, research (design), brand |
| `Leadership` | engineering manager, director, VP, head of, CTO |

`GET /role-families` serves them so no UI ever builds a filter from observed values.

`NormalizeRoleFamily(raw) (string, bool)` resolves case-insensitively: exact canonical name
first, then a longest-span n-gram scan over a synonym table keyed by `normalizeLabel`
(`[^a-z0-9]+` → single space, lowercased, trimmed), ties broken by display rank. It never
guesses a default. An empty family is allowed (surfaced as "Unsorted"); a non-empty
unresolvable one is a 400 whose message names the whole vocabulary.

When the raw label was more specific than the canonical name, the normalized raw is appended
to `tags` once (case-insensitive dedupe) — `"MLE"` → family `AI`, tags `["mle"]`.

---

## `sources` — where to look

```sql
CREATE TABLE IF NOT EXISTS sources (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    name          TEXT UNIQUE NOT NULL,
    kind          TEXT NOT NULL DEFAULT 'research',  -- 'board' | 'research' | 'scout'
    target        TEXT NOT NULL DEFAULT '',          -- board: URL. research: the query/spec. scout: what to hunt for.
    company       TEXT NOT NULL DEFAULT '',          -- board sources are usually one company's careers page
    location      TEXT NOT NULL DEFAULT '',          -- geo filter, e.g. 'Remote (US)', 'Bay Area'
    role_families TEXT NOT NULL DEFAULT '',          -- JSON array, normalized + rank-sorted
    seniorities   TEXT NOT NULL DEFAULT '',          -- JSON array, freeform: 'senior','staff','principal'
    keywords      TEXT NOT NULL DEFAULT '',          -- JSON array, must-have terms
    cadence_hours INTEGER NOT NULL DEFAULT 24,       -- jobs move faster than events; 24 is the default, not 72
    model         TEXT NOT NULL DEFAULT '',          -- effort-tier hint for the research kind
    status        TEXT NOT NULL DEFAULT 'active',    -- 'active' | 'proposed' | 'rejected'
    enabled       INTEGER NOT NULL DEFAULT 1,        -- pause switch, independent of status
    last_run_at   INTEGER NOT NULL DEFAULT 0,
    next_run_at   INTEGER NOT NULL DEFAULT 0,
    notes         TEXT NOT NULL DEFAULT '',
    proposed_by   TEXT NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sources_due ON sources(status, enabled, next_run_at);
```

Three kinds, and the service **never branches on kind** — kind plus `cadence_hours` plus
`model` is the whole effort-tier system, and the dispatch semantics live in the poller prompt:

- **board** — fetch `target` and extract the openings listed on it. Cheapest tier, shortest
  cadence. One company, or one aggregator page.
- **research** — a few focused searches for `target` roles in `location`, filtered by
  `role_families` / `seniorities` / `keywords`.
- **scout** — posts **no listings**. It reads `GET /sources` with no filter (it needs every
  status, `rejected` included, or it re-proposes what was already turned down), verifies a
  candidate page actually lists dated openings, and POSTs new sources with
  `status:"proposed"` and `proposed_by:"<scout name>"`. Then marks itself ran.

### The approval gate is in the query, not the caller

`UpsertSource` on **insert** sets `enabled = (status != "proposed")` and
`next_run_at = now()` when unset, so a brand-new approved source is due immediately and a
proposal is inert by construction. `ListSources` folds the gate into one inseparable clause:

```sql
AND status='active' AND enabled=1 AND next_run_at<=?
```

so a caller cannot forget it. A scout literally cannot give itself work.

### Two write paths, and only one of them decides

- `POST /sources` — idempotent upsert by `name`. The **update** path omits `status`,
  `enabled`, `last_run_at`, `next_run_at`, `proposed_by`, `created_at` from the `UPDATE`
  entirely. A poller re-posting a source can refresh its description and never its verdict.
- `PATCH /sources/{id}` — the decision channel. Setting `status` to `active` from `proposed`
  also sets `enabled=1, next_run_at=now()`, so approving makes it due on the next tick.

`POST /sources/{id}/ran` stamps `last_run_at=now()` and `next_run_at=now()+cadence_hours*3600`
— advancing from *now*, not from the old `next_run_at`, so a late run never stampedes
catch-up runs.

Defaults on upsert: blank `kind` → `research`, blank `status` → `active`,
`cadence_hours <= 0` → 24. An unknown `kind` or `status` is a 400 naming the valid values.

---

## `listings` — what the pollers found

```sql
CREATE TABLE IF NOT EXISTS listings (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    sig             TEXT UNIQUE NOT NULL,          -- sha1(company|title|location)[:8], the dedupe memory
    title           TEXT NOT NULL DEFAULT '',
    company         TEXT NOT NULL DEFAULT '',
    location        TEXT NOT NULL DEFAULT '',
    remote          INTEGER NOT NULL DEFAULT 0,
    employment_type TEXT NOT NULL DEFAULT '',      -- 'full-time' | 'contract' | 'internship' | ''
    seniority       TEXT NOT NULL DEFAULT '',
    role_family     TEXT NOT NULL DEFAULT '',      -- one of the six, or '' for unsorted
    tags            TEXT NOT NULL DEFAULT '',      -- JSON array
    description     TEXT NOT NULL DEFAULT '',      -- the FULL posting text — see below, this is load-bearing
    description_fetched_at INTEGER NOT NULL DEFAULT 0,
    comp_min        INTEGER NOT NULL DEFAULT 0,
    comp_max        INTEGER NOT NULL DEFAULT 0,
    comp_currency   TEXT NOT NULL DEFAULT '',
    comp_raw        TEXT NOT NULL DEFAULT '',      -- exactly as posted: '$180k–$240k + equity'
    url             TEXT NOT NULL DEFAULT '',      -- the posting
    apply_url       TEXT NOT NULL DEFAULT '',      -- where an application is actually submitted
    source_id       INTEGER NOT NULL DEFAULT 0,
    source_name     TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL DEFAULT 'candidate',
    relevance       INTEGER NOT NULL DEFAULT 0,    -- 0-100, the poller's own score
    notes           TEXT NOT NULL DEFAULT '',      -- the user's notes, never a poller's
    posted_at       INTEGER NOT NULL DEFAULT 0,
    closes_at       INTEGER NOT NULL DEFAULT 0,
    discovered_at   INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    deleted_at      INTEGER NOT NULL DEFAULT 0     -- soft delete: 0 = live
);
CREATE INDEX IF NOT EXISTS idx_listings_status  ON listings(status);
CREATE INDEX IF NOT EXISTS idx_listings_company ON listings(company);
CREATE INDEX IF NOT EXISTS idx_listings_posted  ON listings(posted_at);
```

### Status is the pipeline

`candidate` → the user picks → `interested` → an agent works it → `applying` → `applied` →
`rejected` (the company said no) or `offer`. `dismissed` is the user saying no; `closed` is
the posting going away. **`interested` is the queue an application agent reads.**

`POST /listings` upserts by `sig` and its update path **never touches `status`, `notes` or
`relevance`** — a re-poll refreshes the description and comp, never the user's decision.
`PATCH /listings/{id}` is the decision channel, and it guards with
`WHERE id=? AND deleted_at=0`, returning 404 when `RowsAffected()==0`, so a decision can never
be recorded invisibly on a swept row.

Soft delete keeps the row **because the row is the dedupe memory** — purge only via
`?hard=true`. `POST /listings/prune` sweeps stale candidates: `closes_at` passed, or still
`candidate` and `posted_at` older than the grace window (`{"grace_days": n}`, default 45). A
listing the user has touched (any status but `candidate`) is never pruned. An undated listing
is never stale.

List ordering: `ORDER BY (posted_at=0), posted_at DESC, id DESC` — undated rows sort last.

### The full description is required, not a nice-to-have

`description` holds the **complete text of the posting** — responsibilities, requirements,
comp, the lot — fetched from the posting page itself. A board's summary blurb is not the
description. Everything downstream depends on this: tailoring a resume needs the real
requirements, relevance scoring is meaningless without them, and an application agent filling a
form has nothing else to work from.

So the store makes a thin description visible rather than letting it pass:

- `POST /listings` sets `description_fetched_at` whenever it writes a non-empty description.
- A create whose `description` is blank or under **200 characters** still succeeds — a partial
  listing beats a lost one — but the response carries `"description_thin": true` so the poller
  knows to go back for it, and `GET /listings?thin_description=1` lists every such row for
  auditing. It is never silently fine.
- `POST /documents/{id}/tailor` refuses outright on a listing with an empty description.

### Search

An FTS5 virtual table `listings_fts` over `title`, `company`, `description` and `tags`, kept in
sync by `AFTER INSERT`/`UPDATE`/`DELETE` triggers on `listings`, backs `GET /listings?q=`.
Full-text over the whole description is the point — "who wants Kubernetes and Go" is a question
only the full text can answer. Results stay in the standard list order; `q` filters, it does
not reorder. A malformed FTS query is a 400 naming the syntax problem, never a silent zero
rows.

---

## `documents` — resume and cover-letter variants

The preview surface. A document is **two things at once**: the original bytes exactly as
uploaded, and the extracted text an agent can read and tailor. Both are kept. A PDF resume
submitted to an employer should be the file the user made, not a markdown round-trip of it, so
the upload is never lossy — extraction is added alongside the original, never instead of it.

```sql
CREATE TABLE IF NOT EXISTS documents (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    name            TEXT UNIQUE NOT NULL,
    kind            TEXT NOT NULL DEFAULT 'resume',   -- 'resume' | 'cover_letter'
    format          TEXT NOT NULL DEFAULT 'markdown', -- 'markdown'|'html'|'text'|'pdf'|'docx'
    body            TEXT NOT NULL DEFAULT '',         -- editable/extracted text
    source_filename TEXT NOT NULL DEFAULT '',         -- as uploaded
    content_type    TEXT NOT NULL DEFAULT '',
    blob_sha256     TEXT NOT NULL DEFAULT '',         -- content-addressed original bytes, '' if typed in
    byte_size       INTEGER NOT NULL DEFAULT 0,
    extracted_at    INTEGER NOT NULL DEFAULT 0,       -- when body was derived from the blob
    is_default      INTEGER NOT NULL DEFAULT 0,       -- at most one per kind; setting it clears the others
    derived_from_document_id INTEGER NOT NULL DEFAULT 0, -- set on a tailored copy
    listing_id      INTEGER NOT NULL DEFAULT 0,       -- the role a tailored copy was written for
    notes           TEXT NOT NULL DEFAULT '',
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_documents_kind    ON documents(kind, is_default);
CREATE INDEX IF NOT EXISTS idx_documents_derived ON documents(derived_from_document_id);
```

Original bytes live at `<data dir>/blobs/<sha256[:2]>/<sha256>` — content-addressed, so
re-uploading the same file costs nothing and no filename collision can clobber a blob.

### Upload and extraction

`POST /documents/upload` takes `multipart/form-data`: `file`, plus `name` and `kind` fields.
Accepts `.pdf`, `.docx`, `.txt`, `.md`, `.html`. Extraction by type:

- **pdf** — shell out to `pdftotext -layout -enc UTF-8 <in> -`. Poppler is installed on this
  host and `-layout` is what preserves the column structure a resume depends on.
- **docx** — a docx is a zip; read `word/document.xml` with `archive/zip` + `encoding/xml` and
  concatenate the `w:t` runs, breaking paragraphs on `w:p`. Stdlib only, no new dependency.
- **txt / md / html** — the bytes are the body.

**Extraction failing is a 4xx/5xx that says so, never an empty body written silently.** If
`pdftotext` is absent, exits non-zero, or yields nothing but whitespace, the upload fails with
a message naming the cause. A resume that silently became blank is worse than a refused upload
— it would be discovered by an agent submitting an empty application.

`GET /documents/{id}/file` serves the original bytes with the stored `content_type` and
`Content-Disposition: inline`, so a PDF previews natively in an iframe. 404 when
`blob_sha256` is empty (a typed-in document has no original).

`GET /documents/{id}/render` returns `text/html` — the body converted for preview and wrapped
in a minimal print-friendly stylesheet. Conversion is server-side so the preview an agent sees
and the preview the browser shows are the same bytes. For `pdf`/`docx` it renders the
*extracted* text with a banner pointing at `/file` for the true original.

### Choosing which one to use

`is_default` marks the working copy per kind, and setting it clears the flag on every other
document of that kind in the same transaction — a "default" that could be ambiguous is not a
default. An application overrides it per-listing through its own
`resume_document_id` / `cover_letter_document_id`.

---

## `tailor_requests` — customize a document for one role

Queued, not synchronous: the service stays dumb and a dispatcher script runs the agent, the
same split as the pollers. The UI shows `pending → running → ready`.

```sql
CREATE TABLE IF NOT EXISTS tailor_requests (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    document_id        INTEGER NOT NULL,   -- the document to start from
    listing_id         INTEGER NOT NULL,   -- the role to tailor for
    instructions       TEXT NOT NULL DEFAULT '',  -- optional steer from the user
    status             TEXT NOT NULL DEFAULT 'pending', -- 'pending'|'running'|'ready'|'failed'
    result_document_id INTEGER NOT NULL DEFAULT 0,
    agent_session_id   TEXT NOT NULL DEFAULT '',
    error              TEXT NOT NULL DEFAULT '',
    created_at         INTEGER NOT NULL,
    started_at         INTEGER NOT NULL DEFAULT 0,
    finished_at        INTEGER NOT NULL DEFAULT 0,
    updated_at         INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_tailor_pending ON tailor_requests(status, created_at);
```

**Tailoring never overwrites the original.** It writes a *new* document with
`derived_from_document_id` and `listing_id` set, so the source stays pristine and every variant
is traceable to the role it was written for. Both `document_id` and `listing_id` must resolve
to live rows or the request is a 400 — join on ids, never names.

`POST /documents/{id}/tailor` with `{listing_id, instructions}` → **202** and the request row.
Also: `GET /tailor-requests?status&listing_id`, `GET /tailor-requests/{id}`,
`PATCH /tailor-requests/{id}` (the dispatcher's write channel), `DELETE /tailor-requests/{id}`.

A tailor request whose listing has an empty `description` is a **400**. There is nothing to
tailor against without the full job description, and guessing from a job title produces exactly
the generic letter this feature exists to avoid.

---

## `applications` — stubbed, phase 3

The table and CRUD are real so the pipeline has somewhere to write. **No agent runs one yet**;
`POST /applications/{id}/submit` returns `501 not implemented` with a body naming what is
missing, rather than pretending.

```sql
CREATE TABLE IF NOT EXISTS applications (
    id                       INTEGER PRIMARY KEY AUTOINCREMENT,
    listing_id               INTEGER NOT NULL,           -- job-store's own listing id, never the title
    status                   TEXT NOT NULL DEFAULT 'draft', -- 'draft'|'ready'|'submitted'|'failed'
    resume_document_id       INTEGER NOT NULL DEFAULT 0, -- documents.id
    cover_letter_document_id INTEGER NOT NULL DEFAULT 0, -- documents.id, the template
    cover_letter_body        TEXT NOT NULL DEFAULT '',   -- the rendered, listing-specific letter
    answers                  TEXT NOT NULL DEFAULT '',   -- JSON array of {question, answer} screening pairs
    agent_session_id         TEXT NOT NULL DEFAULT '',
    submitted_at             INTEGER NOT NULL DEFAULT 0,
    error                    TEXT NOT NULL DEFAULT '',
    created_at               INTEGER NOT NULL,
    updated_at               INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_applications_listing ON applications(listing_id);
```

`listing_id` must resolve to a live listing or the write is a 400 — join on ids, never names.

---

## Routes

| Method | Path | Notes |
|---|---|---|
| GET | `/health` | `{"status":"ok"}` |
| GET | `/role-families` | `{"role_families":[{name,description}]}` in display order |
| GET | `/listings` | `?status &company &role_family &source_id &remote &seniority &posted_from &posted_to &q &limit &include_deleted` → `{"listings":[…]}` |
| POST | `/listings` | upsert by sig; accepts `posted`/`closes` ISO strings alongside the epoch fields; 201 create / 200 update |
| POST | `/listings/prune` | body optional `{"grace_days":n}` → `{"pruned":n,"grace_days":n}` |
| GET | `/listings/{id}` | `?include_deleted=1` |
| PATCH | `/listings/{id}` | `{status,relevance,notes,role_family,comp_min,comp_max,apply_url}` |
| DELETE | `/listings/{id}` | soft → `{"status":"deleted"}`; `?hard=true` → `{"status":"purged"}` |
| POST | `/listings/{id}/restore` | |
| GET | `/sources` | `?kind &status &due=1` → `{"sources":[…]}` |
| POST | `/sources` | upsert by name |
| GET | `/sources/{id}` | |
| PATCH | `/sources/{id}` | the decision channel |
| POST | `/sources/{id}/ran` | stamp + advance cadence |
| DELETE | `/sources/{id}` | hard |
| GET | `/documents` | `?kind &derived_from &listing_id` → `{"documents":[…]}` |
| POST | `/documents` | upsert by name (typed-in body, no blob) |
| POST | `/documents/upload` | `multipart/form-data`: `file`, `name`, `kind`. pdf/docx/txt/md/html |
| GET | `/documents/{id}` | |
| GET | `/documents/{id}/file` | original bytes, stored content-type, inline. 404 if never uploaded |
| GET | `/documents/{id}/render` | `text/html` preview |
| POST | `/documents/{id}/tailor` | `{listing_id,instructions}` → **202** + the queued request |
| PATCH | `/documents/{id}` | `{name,body,format,is_default,notes}` |
| DELETE | `/documents/{id}` | hard |
| GET | `/tailor-requests` | `?status &listing_id` → `{"tailor_requests":[…]}` |
| GET | `/tailor-requests/{id}` | |
| PATCH | `/tailor-requests/{id}` | the dispatcher's write channel |
| DELETE | `/tailor-requests/{id}` | hard |
| GET | `/applications` | `?status &listing_id` → `{"applications":[…]}` |
| POST | `/applications` | upsert by `listing_id` |
| GET | `/applications/{id}` | |
| PATCH | `/applications/{id}` | |
| POST | `/applications/{id}/submit` | **501**, phase 3 |
| DELETE | `/applications/{id}` | hard |

Errors are `{"error":"…"}`. One `respondStoreError` maps sentinel errors: `ErrNotFound` → 404,
`ErrInvalidSource` / `ErrInvalidListing` / `ErrInvalidRoleFamily` / `ErrInvalidDocument` /
`ErrInvalidApplication` → 400, anything else → 500. Error strings enumerate the valid values so
an agent reading a 400 can retry without guessing.

---

## Pollers

`~/bin/job-radar-dispatch`, installed by `deploy.sh` from `scripts/job-radar-dispatch.sh`, run
by a scheduler shell job. It mirrors `event-radar-dispatch` including the failure modes that
one learned the hard way:

1. `GET /sources?due=1`. Store unreachable → stderr and `exit 1`.
2. Count the rows with `python3`, never `grep -o | wc -l` — grep exits 1 on an empty list and
   under `set -euo pipefail` that kills the script before it can say "nothing due".
3. **Zero due → print and `exit 0` without launching a session.** This gate is the cost control.
4. Build the prompt with the due rows inlined, one JSON object per line, under a header saying
   this list *is* the work — a session told to re-fetch its own worklist will one day misread
   an empty result and report success having done nothing.
5. `claude -p "$PROMPT" --model "$MODEL" --max-budget-usd "$BUDGET"`.
6. **Grade on `last_run_at` deltas per source id**, before vs after — not on exit status and
   not on list membership, since a short cadence can come due again immediately. Zero sources
   remarked → refuse to report success, `exit 1`.

Env knobs, all overridable so the script is testable: `JOB_STORE_URL`, `JOB_RADAR_MODEL`,
`JOB_RADAR_MAX_BUDGET_USD`, `JOB_RADAR_CLAUDE_BIN`.
