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

## Two statuses, and which one owns what

A listing and an application both have a state, and letting them overlap would give one
question two answers. The split is by *whose* state it is:

- **`listings.status`** — your interest in the **role**, before you commit:
  `candidate → interested → dismissed`. **Creating an application moves it to `applied`, once,
  in the same transaction as the insert — and it never moves again.** A later
  `PATCH /listings/{id}` carrying `status` on a listing that has an application is a **400**
  naming the application and its stage; every other field on that listing is still editable.
  It never tracks the employer.
- **`applications.stage`** — where you stand **with the employer**, and the source of truth
  from the moment an application exists:
  `drafting → ready → submitted → acknowledged → screen → interview → onsite → offer`, plus
  the terminal `rejected`, `withdrawn` and `ghosted`.
- **`applications.agent_status`** — what the **automation** has done: `draft`, `ready`,
  `submitted`, `failed`. This is the field previously called `status`; it was renamed because
  "status" on a record that now tracks an employer relationship no longer described it. An
  agent failing to fill a form and a company rejecting you are not the same event and must
  never collapse into one field.

`ghosted` is a real stage, not a missing value. Silence is the most common outcome in a job
search, and a pipeline that can only say `submitted` forever cannot tell you what to chase.

### `application_events` — the timeline

Every stage change is appended, never overwritten. "When did I apply, when did they reply, how
long have they been silent" is the question a job search actually asks, and a single mutable
`stage` column cannot answer it.

```sql
CREATE TABLE IF NOT EXISTS application_events (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    application_id INTEGER NOT NULL,
    stage_from     TEXT NOT NULL DEFAULT '',
    stage_to       TEXT NOT NULL DEFAULT '',
    note           TEXT NOT NULL DEFAULT '',
    source         TEXT NOT NULL DEFAULT 'user',  -- 'user' | 'agent' | 'email'
    occurred_at    INTEGER NOT NULL,
    created_at     INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_application_events ON application_events(application_id, occurred_at);
```

`PATCH /applications/{id}` writing a new `stage` appends the event itself, in the same
transaction. A caller cannot change a stage without leaving a trace.

---

## `application_emails` — what they actually sent you

Joins an application to messages in **mailstack** (`:8195`), which owns them. job-store stores
identifiers and just enough to render a row without a round-trip; it never becomes a second
copy of your mail.

```sql
CREATE TABLE IF NOT EXISTS application_emails (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    application_id INTEGER NOT NULL,
    account_id     TEXT NOT NULL,               -- mailstack account
    message_id     TEXT NOT NULL,               -- mailstack's own per-account message id
    rfc_message_id TEXT NOT NULL DEFAULT '',    -- RFC 5322, brackets stripped. May be empty.
    thread_id      TEXT NOT NULL DEFAULT '',
    direction      TEXT NOT NULL DEFAULT 'inbound', -- 'inbound' | 'outbound'
    subject        TEXT NOT NULL DEFAULT '',
    from_address   TEXT NOT NULL DEFAULT '',
    occurred_at    INTEGER NOT NULL DEFAULT 0,
    linked_by      TEXT NOT NULL DEFAULT 'user',    -- 'user' | 'agent' | 'matcher'
    status         TEXT NOT NULL DEFAULT 'linked',  -- 'proposed' | 'linked' | 'rejected'
    created_at     INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_application_emails_msg
    ON application_emails(application_id, account_id, message_id);
```

**The join key is `(account_id, message_id)`** — mailstack's own id, always present. The RFC
`Message-ID` is carried because it is the only identifier stable across accounts and folders,
but mailstack's own source says it is optional per RFC 5322 §3.6.4 and that **no caller may key
on it blindly**, so it is a dedup aid here and never the primary key.

A matcher may **propose** a link (`status:"proposed"`, `linked_by:"matcher"`); a human confirms
it to `linked`. Same discipline as a scouted source: guessing that an email from a company
domain belongs to a given application is exactly the guess that quietly files a rejection under
the wrong job. Only a `linked` email may drive a stage change, and
`POST /application-emails/{id}/stage` is the one route that can do it — a `proposed` link there
is a 400, so the rule lives in one place instead of being asked of every caller that reads mail.

Linking one by hand: `POST /applications/{id}/emails` with

```json
{"account_id":"gmail-personal","message_id":"18f2c9a1b","direction":"inbound",
 "subject":"…","from_address":"…","rfc_message_id":"…","thread_id":"…","occurred_at":0}
```

`account_id` and `message_id` are the only required fields. `direction` is accepted and
defaults to `inbound`; `linked_by` defaults to `user`, which means the link lands `linked` — a
person linking a message is not guessing. The decoder is **not** strict: an unknown field is
ignored rather than rejected, so a caller sending extra keys gets a 201, not a 400. Re-posting
the same `(account_id, message_id)` updates the descriptive fields and returns **200**;
`status` and `linked_by` are omitted from that update, so a re-run cannot undo a verdict.

---

## `application_tasks` — the things you still have to do

Tasks are **noteboard todos**. This table stores their ids and nothing else — no title, no
body, no status copy. noteboard owns the content; duplicating it here would create a second
truth that drifts the first time one is edited.

```sql
CREATE TABLE IF NOT EXISTS application_tasks (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    application_id  INTEGER NOT NULL,
    noteboard_id    TEXT NOT NULL,            -- noteboard item uuid, the only reference kept
    created_at      INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_application_tasks
    ON application_tasks(application_id, noteboard_id);
```

`GET /applications/{id}?expand=tasks` reads each todo through noteboard at request time. If
noteboard is unreachable the field is an explicit error, never an empty list — an application
that silently shows zero outstanding tasks is worse than one that admits it cannot tell.

The expansion is a **200 whose `tasks` field carries the error**, not a non-2xx: the application
itself is still worth serving. The shape, exactly:

```jsonc
"tasks": {
  "items": [                              // null — not [] — when NOTHING could be read
    { "id": 7,                            // the application_tasks row
      "noteboard_id": "93c345b7-…",
      "created_at": 1786…,
      "item": { "id": "93c345b7-…", "type": "todo", "title": "…", "status": "open", … },
      "error": ""                         // present instead of `item` when THIS todo failed
    }
  ],
  "error": ""                             // set whenever any todo could not be read
}
```

`item` is noteboard's own record passed through **unchanged** — read `item.title` and
`item.status`, not `title`/`status` on the wrapper. This layer is transparent on purpose: a
flattened copy of a noteboard todo here would be a second, narrower schema to drift.

`tasks` is absent entirely without `?expand=tasks`, which is not the same as an empty list.
Without the parameter, `task_count` and `open_task_count` above are the summary to read.

---

## `applications` — submission is still phase 3

The table, the CRUD and the whole tracking layer are real. **No agent submits one yet**;
`POST /applications/{id}/submit` returns `501 not implemented` with a body naming what is
missing, rather than pretending.

```sql
CREATE TABLE IF NOT EXISTS applications (
    id                       INTEGER PRIMARY KEY AUTOINCREMENT,
    listing_id               INTEGER NOT NULL,                 -- job-store's own listing id, never the title
    stage                    TEXT NOT NULL DEFAULT 'drafting', -- where you stand with the EMPLOYER
    agent_status             TEXT NOT NULL DEFAULT 'draft',    -- 'draft'|'ready'|'submitted'|'failed'
    resume_document_id       INTEGER NOT NULL DEFAULT 0,       -- documents.id
    resume_body_sha256       TEXT NOT NULL DEFAULT '',         -- what was ACTUALLY sent, pinned at submit
    cover_letter_document_id INTEGER NOT NULL DEFAULT 0,       -- documents.id, the template
    cover_letter_body        TEXT NOT NULL DEFAULT '',         -- the rendered, listing-specific letter
    answers                  TEXT NOT NULL DEFAULT '',         -- JSON array of {question, answer} screening pairs
    agent_session_id         TEXT NOT NULL DEFAULT '',
    submitted_at             INTEGER NOT NULL DEFAULT 0,
    error                    TEXT NOT NULL DEFAULT '',
    created_at               INTEGER NOT NULL,
    updated_at               INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_applications_listing ON applications(listing_id);
```

`listing_id` must resolve to a live listing or the write is a 400 — join on ids, never names.
Creating an application moves that listing to `applied` **in the same transaction**.

`agent_status` is the column once called `status`. A database written before the rename is
migrated in place (`ALTER TABLE … RENAME COLUMN`), keeping every value, and `stage` is
backfilled only where the old value implies one: `submitted → submitted`, `ready → ready`,
everything else the `drafting` default. The old spelling is **refused, not ignored** — a
`status` field on a create or patch, or `?status=` on the list, is a 400 naming both
replacements, because a silently dropped field is a caller believing it recorded something.

Only `PATCH /applications/{id}` writes `stage`. `POST /applications` refreshes what the
automation holds and never the stage, so "no stage change without an event" holds by
construction rather than by every caller remembering.

### Derived fields — every application row carries its own summary

All four are computed on read, never stored, and they come back on **both** `GET /applications`
and `GET /applications/{id}` so a board never has to fetch `/events`, `/emails` and `/tasks` per
row just to render a line:

| Field | What it is |
|---|---|
| `resume_drifted` | the resume document's body no longer hashes to `resume_body_sha256` |
| `email_count` | how many **`linked`** emails are attached. A `proposed` link is a matcher's guess and does not count |
| `task_count` | how many noteboard todos are linked. Known locally, always exact |
| `open_task_count` | how many of those noteboard still reports as `open`, from **one** noteboard read for the whole page (`?type=todo&status=open&include_held=true`; a held todo is parked work, not finished work, so it counts). `null` means **could not tell** — noteboard unreachable, or a write response where nobody asked. It never collapses to `0` |
| `last_activity_at` | the newest `occurred_at` across `application_events` and **`linked`** `application_emails`. Deliberately **not** `updated_at`: an agent retrying a write is not a company writing back, and this is what a "quiet for N days" column is measured from |

A noteboard that is down does not fail the list — the applications are served with
`open_task_count: null` and the reason is logged.

### Which resume was actually sent

`resume_document_id` names the document; `resume_body_sha256` pins its **content at the moment
of submission**. Documents are editable, so the id alone answers "which file" and not "which
version" — edit your resume next month and the id would quietly claim you sent the new one.
The hash is written when `agent_status` reaches `submitted` and is never rewritten after.

`GET /applications/{id}` therefore reports `resume_drifted: true` when the document's current
body no longer hashes to the pinned value. That is not an error — it is the normal result of
improving your resume — but you should know the copy an employer holds is not the copy on your
screen before you walk into an interview.

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
| GET | `/applications` | `?stage &agent_status &listing_id` → `{"applications":[…]}`, each row carrying `resume_drifted`, `email_count`, `task_count`, `open_task_count`, `last_activity_at`. `?status` is a **400** — it would be ambiguous between the two |
| POST | `/applications` | upsert by `listing_id`; creating one moves the listing to `applied`. Never writes `stage` |
| GET | `/applications/{id}` | `?expand=tasks` reads the linked todos through noteboard at request time |
| PATCH | `/applications/{id}` | `{stage,agent_status,resume_document_id,…,note,event_source,event_occurred_at}`. A `stage` change appends its event in the same transaction |
| POST | `/applications/{id}/submit` | **501**, phase 3 |
| DELETE | `/applications/{id}` | hard; takes the timeline, email links and task links with it |
| GET | `/applications/{id}/events` | `{"events":[…]}`, oldest first |
| POST | `/applications/{id}/events` | `{note,source,occurred_at}` → **201**. Records a note; a `stage_to` here is a 400 |
| GET | `/applications/{id}/emails` | `{"emails":[…]}`, proposals included |
| POST | `/applications/{id}/emails` | upsert by `(account_id,message_id)`; `linked_by:"matcher"` ⇒ `proposed`. Re-posting never touches `status` or `linked_by` |
| PATCH | `/application-emails/{id}` | `{status}` — the confirm/reject channel |
| DELETE | `/application-emails/{id}` | hard; the message itself is mailstack's |
| POST | `/application-emails/{id}/stage` | `{stage,note}` — the ONLY path by which mail moves a stage. A `proposed` link is a **400** |
| GET | `/applications/{id}/tasks` | `{"tasks":[…]}` — ids only |
| POST | `/applications/{id}/tasks` | `{noteboard_id}` |
| POST | `/applications/{id}/tasks/standard` | creates the standard follow-ups **in noteboard**, links the ids it returns → **201**. noteboard down ⇒ **502**; an application that already has links ⇒ **400** |
| DELETE | `/application-tasks/{id}` | hard; the todo stays in noteboard |

Errors are `{"error":"…"}`. One `respondStoreError` maps sentinel errors: `ErrNotFound` → 404,
`ErrInvalidSource` / `ErrInvalidListing` / `ErrInvalidRoleFamily` / `ErrInvalidDocument` /
`ErrInvalidApplication` / `ErrInvalidApplicationEvent` / `ErrInvalidApplicationEmail` /
`ErrInvalidApplicationTask` → 400, `ErrNoteboard` → **502** (a dependency is down, which is
neither the caller's mistake nor a bug in here), anything else → 500. Error strings enumerate
the valid values so an agent reading a 400 can retry without guessing.

`NOTEBOARD_URL` overrides where the task routes read and write, default `http://localhost:8191`.

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
