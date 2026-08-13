PRAGMA foreign_keys = ON;

-- Create-only. Every statement here is IF NOT EXISTS, so this file is what an
-- empty database gets and nothing more: a column added to a table that already
-- exists on this host will never appear by editing this file. Those go in the
-- ensureColumn loop in Open(), and any index naming such a column goes in the
-- index loop that runs after it.

-- sources: where to look for jobs. Each source carries its OWN cadence and effort
-- tier, so a cheap careers-page fetch runs daily while an open-ended research
-- sweep runs less often. The dispatcher reads next_run_at to decide what is due.
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
    status        TEXT NOT NULL DEFAULT 'active',    -- 'active' | 'proposed' (found by a scout, awaiting review) | 'rejected'
    enabled       INTEGER NOT NULL DEFAULT 1,        -- pause switch, independent of status
    last_run_at   INTEGER NOT NULL DEFAULT 0,
    next_run_at   INTEGER NOT NULL DEFAULT 0,
    notes         TEXT NOT NULL DEFAULT '',
    proposed_by   TEXT NOT NULL DEFAULT '',          -- name of the scout that proposed it, '' if you added it
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);
-- Only an active, enabled source is ever due, so the index leads with both.
CREATE INDEX IF NOT EXISTS idx_sources_due ON sources(status, enabled, next_run_at);

-- listings: what the pollers found. status tracks the human decision; sig is the
-- dedupe key so a re-poll updates the same row instead of duplicating it.
CREATE TABLE IF NOT EXISTS listings (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    sig             TEXT UNIQUE NOT NULL,          -- sha1(company|title|location), the dedupe memory
    title           TEXT NOT NULL DEFAULT '',
    company         TEXT NOT NULL DEFAULT '',
    location        TEXT NOT NULL DEFAULT '',
    remote          INTEGER NOT NULL DEFAULT 0,
    employment_type TEXT NOT NULL DEFAULT '',      -- 'full-time' | 'contract' | 'internship' | ''
    seniority       TEXT NOT NULL DEFAULT '',
    role_family     TEXT NOT NULL DEFAULT '',      -- one of the six, or '' for unsorted
    tags            TEXT NOT NULL DEFAULT '',      -- JSON array
    description     TEXT NOT NULL DEFAULT '',      -- the FULL posting text; everything downstream depends on it
    description_fetched_at INTEGER NOT NULL DEFAULT 0,
    comp_min        INTEGER NOT NULL DEFAULT 0,
    comp_max        INTEGER NOT NULL DEFAULT 0,
    comp_currency   TEXT NOT NULL DEFAULT '',
    comp_raw        TEXT NOT NULL DEFAULT '',      -- exactly as posted: '$180k-$240k + equity'
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
    deleted_at      INTEGER NOT NULL DEFAULT 0     -- soft delete: 0 = live, else when it was swept
);
CREATE INDEX IF NOT EXISTS idx_listings_status  ON listings(status);
CREATE INDEX IF NOT EXISTS idx_listings_company ON listings(company);
CREATE INDEX IF NOT EXISTS idx_listings_posted  ON listings(posted_at);

-- Full-text search over the whole posting, not just its title. "Who wants
-- Kubernetes and Go" is a question only the description body can answer, and a
-- LIKE scan over a growing description column is not that question's answer.
--
-- External-content table: the rows live in `listings` and the index holds only
-- the terms, so there is exactly one copy of every description.
--
-- Requires a driver built with FTS5 compiled in. See Open(): a build without it
-- fails here, at boot, with a message naming the build tag.
CREATE VIRTUAL TABLE IF NOT EXISTS listings_fts USING fts5(
    title, company, description, tags,
    content='listings', content_rowid='id'
);

-- The triggers are what make the index true rather than merely initialized. An
-- external-content FTS5 table is not updated by writes to its content table, so
-- without these a re-poll would leave the index describing the previous version
-- of the posting and a search would answer from history.
CREATE TRIGGER IF NOT EXISTS listings_fts_after_insert AFTER INSERT ON listings BEGIN
    INSERT INTO listings_fts(rowid, title, company, description, tags)
    VALUES (new.id, new.title, new.company, new.description, new.tags);
END;
CREATE TRIGGER IF NOT EXISTS listings_fts_after_delete AFTER DELETE ON listings BEGIN
    INSERT INTO listings_fts(listings_fts, rowid, title, company, description, tags)
    VALUES ('delete', old.id, old.title, old.company, old.description, old.tags);
END;
CREATE TRIGGER IF NOT EXISTS listings_fts_after_update AFTER UPDATE ON listings BEGIN
    INSERT INTO listings_fts(listings_fts, rowid, title, company, description, tags)
    VALUES ('delete', old.id, old.title, old.company, old.description, old.tags);
    INSERT INTO listings_fts(rowid, title, company, description, tags)
    VALUES (new.id, new.title, new.company, new.description, new.tags);
END;

-- documents: resume and cover-letter variants. A document is two things at once
-- — the original bytes exactly as uploaded (content-addressed under
-- <data dir>/blobs/) and the extracted text an agent can read and tailor. Both
-- are kept, because the file an employer receives should be the one the user
-- made, not a markdown round-trip of it.
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

-- tailor_requests: a queued ask to customize one document for one role. Queued,
-- not synchronous — the service stays dumb and a dispatcher script runs the
-- agent, the same split as the pollers. Tailoring never overwrites the original:
-- the dispatcher writes a NEW document carrying derived_from_document_id.
CREATE TABLE IF NOT EXISTS tailor_requests (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    document_id        INTEGER NOT NULL,                -- the document to start from
    listing_id         INTEGER NOT NULL,                -- the role to tailor for
    instructions       TEXT NOT NULL DEFAULT '',        -- optional steer from the user
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

-- applications: one application against one listing, carrying TWO states that
-- must never collapse into one field. stage is where you stand with the employer
-- and is the pipeline once an application exists; agent_status is what the
-- automation has done. An agent failing to fill a form and a company rejecting
-- you are not the same event.
--
-- agent_status is the column once called `status`. On a database created before
-- the rename it is renamed in place by the ensureRenamedColumn call in Open(),
-- never dropped and recreated, so no row loses what it held.
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

-- application_events: the timeline, append-only. Every stage change is appended
-- in the SAME transaction as the change, so a caller cannot move an application
-- without leaving a trace. "When did I apply, when did they reply, how long have
-- they been silent" is the question a job search actually asks, and one mutable
-- stage column cannot answer it.
--
-- stage_from and stage_to are both empty on an event that records something which
-- happened without moving the stage.
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

-- application_emails: the join to mailstack (:8195), which OWNS the messages.
-- Identifiers plus just enough to render a row without a round-trip, never a
-- second copy of your mail.
--
-- The key is (application_id, account_id, message_id) — mailstack's own
-- per-account id, always present. rfc_message_id is carried because it is the
-- only identifier stable across accounts and folders, but mailstack's own source
-- says it is OPTIONAL per RFC 5322 §3.6.4 and that no caller may key on it
-- blindly, so it is a dedup aid here and never the primary key.
CREATE TABLE IF NOT EXISTS application_emails (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    application_id INTEGER NOT NULL,
    account_id     TEXT NOT NULL,                   -- mailstack account
    message_id     TEXT NOT NULL,                   -- mailstack's own per-account message id
    rfc_message_id TEXT NOT NULL DEFAULT '',        -- RFC 5322, brackets stripped. May be empty.
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

-- application_tasks: noteboard todo ids and nothing else. noteboard owns the
-- content; a title or status copied here would be a second truth that drifts the
-- first time one of them is edited.
CREATE TABLE IF NOT EXISTS application_tasks (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    application_id  INTEGER NOT NULL,
    noteboard_id    TEXT NOT NULL,            -- noteboard item uuid, the only reference kept
    created_at      INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_application_tasks
    ON application_tasks(application_id, noteboard_id);
