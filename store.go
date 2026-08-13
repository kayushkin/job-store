package jobstore

import (
	"crypto/sha1"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// ErrNotFound is returned when a lookup finds no row.
var ErrNotFound = errors.New("not found")

// ErrInvalidSource marks a source the caller described wrongly (bad kind, bad
// status, non-positive cadence), so the HTTP layer answers 400 rather than 500.
var ErrInvalidSource = errors.New("invalid source")

// ErrInvalidListing marks a listing the caller described wrongly (missing title,
// unknown status), so the HTTP layer answers 400 rather than 500.
var ErrInvalidListing = errors.New("invalid listing")

// ErrInvalidDocument marks a document the caller described wrongly (missing name,
// unknown kind or format), so the HTTP layer answers 400 rather than 500.
var ErrInvalidDocument = errors.New("invalid document")

// ErrInvalidApplication marks an application the caller described wrongly — most
// often a listing_id that resolves to nothing — so the HTTP layer answers 400.
var ErrInvalidApplication = errors.New("invalid application")

// ErrInvalidTailorRequest marks a tailor request the caller described wrongly —
// an unknown document or listing, or a listing with no description to tailor
// against — so the HTTP layer answers 400.
var ErrInvalidTailorRequest = errors.New("invalid tailor request")

// Store wraps the SQLite database.
type Store struct {
	db      *sql.DB
	dataDir string
}

// DefaultDataDir is where the DB lives when JOB_STORE_DATA_DIR is unset.
func DefaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".config", "job-store")
}

// Open opens (and migrates) the store.
func Open(dataDir string) (*Store, error) {
	if dataDir == "" {
		dataDir = DefaultDataDir()
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", dataDir, err)
	}
	dbPath := filepath.Join(dataDir, "job-store.db")
	db, err := sql.Open("sqlite3", dbPath+"?_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable WAL: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(dataDir, blobDirName), 0o755); err != nil {
		db.Close()
		return nil, fmt.Errorf("mkdir blobs: %w", err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		// The schema creates an FTS5 virtual table, and mattn/go-sqlite3 only
		// compiles FTS5 in when the build asks for it. Say so here rather than
		// letting an operator read "no such module: fts5" and go looking for a
		// missing SQLite install: the fix is a build flag, not a package.
		if strings.Contains(err.Error(), "no such module: fts5") {
			return nil, fmt.Errorf(
				"migrate: %w — this binary was built without FTS5; rebuild with: go build -tags sqlite_fts5 ./cmd/job-store", err)
		}
		return nil, fmt.Errorf("migrate: %w", err)
	}
	s := &Store{db: db, dataDir: dataDir}
	// schema.sql only creates tables that do not exist yet, so a column added to a
	// table already present on a running host has to be added here. Every entry is
	// idempotent: ensureColumn is a no-op when the column is already there, which
	// is the case for a database created from the current schema.sql.
	for _, col := range []struct{ table, column, ddl string }{
		{"listings", "deleted_at", `deleted_at INTEGER NOT NULL DEFAULT 0`},
		{"listings", "apply_url", `apply_url TEXT NOT NULL DEFAULT ''`},
		{"listings", "description_fetched_at", `description_fetched_at INTEGER NOT NULL DEFAULT 0`},
		{"sources", "company", `company TEXT NOT NULL DEFAULT ''`},
		{"sources", "proposed_by", `proposed_by TEXT NOT NULL DEFAULT ''`},
		{"documents", "source_filename", `source_filename TEXT NOT NULL DEFAULT ''`},
		{"documents", "content_type", `content_type TEXT NOT NULL DEFAULT ''`},
		{"documents", "blob_sha256", `blob_sha256 TEXT NOT NULL DEFAULT ''`},
		{"documents", "byte_size", `byte_size INTEGER NOT NULL DEFAULT 0`},
		{"documents", "extracted_at", `extracted_at INTEGER NOT NULL DEFAULT 0`},
		{"documents", "derived_from_document_id", `derived_from_document_id INTEGER NOT NULL DEFAULT 0`},
		{"documents", "listing_id", `listing_id INTEGER NOT NULL DEFAULT 0`},
	} {
		if err := s.ensureColumn(col.table, col.column, col.ddl); err != nil {
			db.Close()
			return nil, fmt.Errorf("add %s.%s: %w", col.table, col.column, err)
		}
	}
	// Indexes naming a column added above have to be created after that column
	// exists, so they live here rather than in schema.sql: schema.sql runs first,
	// and on an existing database an index over a not-yet-added column fails to
	// create and the service refuses to boot.
	for _, ddl := range []string{
		`CREATE INDEX IF NOT EXISTS idx_listings_live ON listings(deleted_at, posted_at)`,
		`CREATE INDEX IF NOT EXISTS idx_documents_derived ON documents(derived_from_document_id)`,
	} {
		if _, err := db.Exec(ddl); err != nil {
			db.Close()
			return nil, fmt.Errorf("create index: %w", err)
		}
	}
	return s, nil
}

// ensureColumn adds a column when the table does not already have it. SQLite has
// no "ADD COLUMN IF NOT EXISTS", so the presence check is a table_info scan.
func (s *Store) ensureColumn(table, column, ddl string) error {
	rows, err := s.db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = s.db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + ddl)
	if err == nil {
		log.Printf("[job-store] schema: added %s.%s", table, column)
	}
	return err
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// DataDir reports the data directory.
func (s *Store) DataDir() string { return s.dataDir }

func now() int64 { return time.Now().Unix() }

func marshalTags(t []string) string {
	if len(t) == 0 {
		return "[]"
	}
	b, err := json.Marshal(t)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// unmarshalTags decodes a JSON array column, always returning a non-nil slice so
// the JSON that goes back out is [] rather than null — a UI mapping over the
// field should never have to check for null first.
func unmarshalTags(s string) []string {
	out := []string{}
	if s == "" {
		return out
	}
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return []string{}
	}
	if out == nil {
		out = []string{}
	}
	return out
}

func marshalAnswers(a []ApplicationAnswer) string {
	if len(a) == 0 {
		return "[]"
	}
	b, err := json.Marshal(a)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func unmarshalAnswers(s string) []ApplicationAnswer {
	out := []ApplicationAnswer{}
	if s == "" {
		return out
	}
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return []ApplicationAnswer{}
	}
	if out == nil {
		out = []ApplicationAnswer{}
	}
	return out
}

// Sig builds a stable dedupe signature from company, title and location, so a
// re-poll of the same source updates the same row instead of creating a duplicate
// — and so a listing the user soft-deleted is recognized rather than re-proposed.
//
// The three fields are normalized the same way a label is, so "Acme, Inc." and
// "Acme Inc" agree and a posting that gains a trailing "(Remote)" in its location
// does not become a second row.
func Sig(company, title, location string) string {
	norm := func(v string) string { return normalizeLabel(v) }
	raw := norm(company) + "|" + norm(title) + "|" + norm(location)
	sum := sha1.Sum([]byte(raw))
	return fmt.Sprintf("%x", sum[:8])
}

type scanner interface {
	Scan(dest ...any) error
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---- Listings ----

const secondsPerDay int64 = 24 * 60 * 60

// DefaultPruneGraceDays is how long a listing nobody has acted on stays in the
// candidate list after it was posted. Six weeks: long enough that a posting you
// have not read yet is still there, short enough that the list is not a museum.
const DefaultPruneGraceDays = 45

// ThinDescriptionMinChars is the length below which a description is too short
// to be the real posting. A board's one-line summary blurb clears 40 characters
// and tells a tailoring agent nothing; the responsibilities-and-requirements
// text every downstream consumer needs never comes in under this.
//
// A listing under it is still accepted — a partial listing beats a lost one —
// but it is flagged, never silently treated as complete.
const ThinDescriptionMinChars = 200

// IsThinDescription reports whether a description is too short to tailor a
// resume or score relevance against.
func IsThinDescription(description string) bool {
	return len(strings.TrimSpace(description)) < ThinDescriptionMinChars
}

// ListingFilter narrows a ListListings query. Zero values mean "no filter".
type ListingFilter struct {
	Status     string
	Company    string
	RoleFamily string
	SourceID   int64
	Seniority  string
	// Remote is a tri-state: nil means "do not filter on it at all", which is not
	// the same as false ("only on-site"). A plain bool would make ?remote=0
	// indistinguishable from an absent parameter.
	Remote     *bool
	PostedFrom int64
	PostedTo   int64
	// Query is a full-text search over title, company, description and tags,
	// answered by the listings_fts index. It filters within the standard list
	// order; it does not reorder by relevance.
	Query string
	// ThinDescription lists only the rows whose description is too short to be
	// the real posting, so a poller's misses can be audited rather than sitting
	// in the list looking like everything else.
	ThinDescription bool
	Limit           int
	// IncludeDeleted lists soft-deleted listings alongside live ones. Off by
	// default: a caller asking for listings wants the ones still worth acting on.
	IncludeDeleted bool
}

// UpsertListing inserts a new listing or, when one with the same sig exists,
// refreshes its descriptive fields while preserving the human decision. Status,
// notes and relevance are never clobbered by a re-poll: a poller refreshes what
// the posting says, never what you decided about it. Returns the row and whether
// it was newly created.
func (s *Store) UpsertListing(l *Listing) (*Listing, bool, error) {
	if strings.TrimSpace(l.Title) == "" {
		return nil, false, fmt.Errorf("%w: title required", ErrInvalidListing)
	}
	if l.Sig == "" {
		l.Sig = Sig(l.Company, l.Title, l.Location)
	}
	if l.Status == "" {
		l.Status = ListingStatusCandidate
	}
	if !ValidListingStatus(l.Status) {
		return nil, false, fmt.Errorf("%w: unknown status %q: use one of %s",
			ErrInvalidListing, l.Status, strings.Join(ListingStatuses, ", "))
	}
	// Collapse whatever label the discovering poller chose onto the canonical
	// vocabulary before it reaches the table, so every read path can trust it.
	if err := NormalizeListingRoleFamily(l); err != nil {
		return nil, false, err
	}
	ts := now()
	// description_fetched_at records when the posting body was last actually
	// fetched, so it is stamped only when a non-empty description is written.
	descriptionFetchedAt := int64(0)
	if strings.TrimSpace(l.Description) != "" {
		descriptionFetchedAt = ts
	}

	var existingID int64
	err := s.db.QueryRow(`SELECT id FROM listings WHERE sig = ?`, l.Sig).Scan(&existingID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		res, err := s.db.Exec(`
			INSERT INTO listings
			  (sig, title, company, location, remote, employment_type, seniority, role_family,
			   tags, description, description_fetched_at, comp_min, comp_max, comp_currency,
			   comp_raw, url, apply_url, source_id, source_name, status, relevance, notes,
			   posted_at, closes_at, discovered_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			l.Sig, l.Title, l.Company, l.Location, boolToInt(l.Remote), l.EmploymentType,
			l.Seniority, l.RoleFamily, marshalTags(l.Tags), l.Description, descriptionFetchedAt,
			l.CompMin, l.CompMax, l.CompCurrency, l.CompRaw, l.URL, l.ApplyURL, l.SourceID,
			l.SourceName, l.Status, l.Relevance, l.Notes, l.PostedAt, l.ClosesAt, ts, ts)
		if err != nil {
			return nil, false, err
		}
		id, _ := res.LastInsertId()
		got, err := s.GetListing(id)
		return got, true, err
	case err != nil:
		return nil, false, err
	default:
		// Refresh what the posting says; leave status, notes and relevance alone.
		// Those three are the decision, and a re-poll is not a decision.
		//
		// The description is refreshed only when the re-poll actually carries one.
		// A board listing re-scraped without its body would otherwise blank a
		// description someone already paid a page fetch for, and the row would go
		// from complete to thin with nothing recording that it had ever been full.
		// COALESCE-style guards belong in SQL here rather than a read-then-write,
		// so two concurrent pollers cannot interleave into a blanked description.
		_, err := s.db.Exec(`
			UPDATE listings SET
			  title=?, company=?, location=?, remote=?, employment_type=?, seniority=?,
			  role_family=?, tags=?,
			  description=CASE WHEN ?='' THEN description ELSE ? END,
			  description_fetched_at=CASE WHEN ?='' THEN description_fetched_at ELSE ? END,
			  comp_min=?, comp_max=?, comp_currency=?,
			  comp_raw=?, url=?, apply_url=?, source_id=?, source_name=?, posted_at=?,
			  closes_at=?, updated_at=?
			WHERE id=?`,
			l.Title, l.Company, l.Location, boolToInt(l.Remote), l.EmploymentType, l.Seniority,
			l.RoleFamily, marshalTags(l.Tags),
			strings.TrimSpace(l.Description), l.Description,
			strings.TrimSpace(l.Description), descriptionFetchedAt,
			l.CompMin, l.CompMax,
			l.CompCurrency, l.CompRaw, l.URL, l.ApplyURL, l.SourceID, l.SourceName,
			l.PostedAt, l.ClosesAt, ts, existingID)
		if err != nil {
			return nil, false, err
		}
		// Read back including deleted rows: refreshing a swept listing's details is
		// a real success, and reporting it as "not found" would make a re-poll look
		// like it had failed.
		got, err := s.GetListingIncludingDeleted(existingID)
		return got, false, err
	}
}

// GetListing fetches one live listing by id. A soft-deleted listing reads as not
// found, the same as it drops out of ListListings; use GetListingIncludingDeleted
// to see one anyway.
func (s *Store) GetListing(id int64) (*Listing, error) {
	row := s.db.QueryRow(listingCols+` FROM listings WHERE id=? AND deleted_at=0`, id)
	return scanListing(row)
}

// GetListingIncludingDeleted fetches one listing by id whether or not it has been
// soft-deleted, for inspecting a swept listing before restoring it.
func (s *Store) GetListingIncludingDeleted(id int64) (*Listing, error) {
	row := s.db.QueryRow(listingCols+` FROM listings WHERE id=?`, id)
	return scanListing(row)
}

// ListListings returns listings matching the filter, newest-posted first.
// Soft-deleted listings are left out unless the filter asks for them.
func (s *Store) ListListings(f ListingFilter) ([]*Listing, error) {
	q := listingCols + ` FROM listings WHERE 1=1`
	var args []any
	if !f.IncludeDeleted {
		q += ` AND deleted_at=0`
	}
	if f.Status != "" {
		q += ` AND status=?`
		args = append(args, f.Status)
	}
	if f.Company != "" {
		q += ` AND company=?`
		args = append(args, f.Company)
	}
	if f.RoleFamily != "" {
		q += ` AND role_family=?`
		args = append(args, f.RoleFamily)
	}
	if f.SourceID != 0 {
		q += ` AND source_id=?`
		args = append(args, f.SourceID)
	}
	if f.Seniority != "" {
		q += ` AND seniority=?`
		args = append(args, f.Seniority)
	}
	if f.Remote != nil {
		q += ` AND remote=?`
		args = append(args, boolToInt(*f.Remote))
	}
	if f.PostedFrom != 0 {
		q += ` AND posted_at>=?`
		args = append(args, f.PostedFrom)
	}
	if f.PostedTo != 0 {
		q += ` AND posted_at<=?`
		args = append(args, f.PostedTo)
	}
	if f.ThinDescription {
		q += ` AND length(trim(description)) < ?`
		args = append(args, ThinDescriptionMinChars)
	}
	if f.Query != "" {
		// Check the MATCH expression on its own first. SQLite reports a malformed
		// FTS query as an error, and running it inline would surface that as a 500
		// — or, worse, be mistaken for "no listings matched". A bad query is the
		// caller's mistake and has to read as one.
		if err := s.validateSearchQuery(f.Query); err != nil {
			return nil, err
		}
		q += ` AND id IN (SELECT rowid FROM listings_fts WHERE listings_fts MATCH ?)`
		args = append(args, f.Query)
	}
	// An undated listing sorts last rather than first: posted_at=0 means "we do
	// not know when this went up", which is weaker information than any real date.
	q += ` ORDER BY (posted_at=0), posted_at DESC, id DESC`
	if f.Limit > 0 {
		q += fmt.Sprintf(` LIMIT %d`, f.Limit)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Listing
	for rows.Next() {
		l, err := scanListing(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// validateSearchQuery runs the caller's MATCH expression against the index in
// isolation, so a syntax error comes back as ErrInvalidListing (400) naming the
// problem instead of a 500 or, worse, an empty result the caller reads as "no
// jobs mention Kubernetes".
func (s *Store) validateSearchQuery(query string) error {
	var probe int
	err := s.db.QueryRow(`SELECT 1 FROM listings_fts WHERE listings_fts MATCH ? LIMIT 1`, query).Scan(&probe)
	if err == nil || errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return fmt.Errorf("%w: bad search query %q: %s", ErrInvalidListing, query, err.Error())
}

// ListingPatch carries a human decision about a listing. Nil pointers are skipped.
//
// It is deliberately not the whole record: POST /listings rewrites what the
// posting says and is safe to re-run, while this is the channel for the things
// only a person should change.
type ListingPatch struct {
	Status     *string `json:"status"`
	Relevance  *int    `json:"relevance"`
	Notes      *string `json:"notes"`
	RoleFamily *string `json:"role_family"`
	CompMin    *int64  `json:"comp_min"`
	CompMax    *int64  `json:"comp_max"`
	ApplyURL   *string `json:"apply_url"`
}

// PatchListing applies a partial update and returns the fresh row.
func (s *Store) PatchListing(id int64, p ListingPatch) (*Listing, error) {
	var sets []string
	var args []any
	if p.Status != nil {
		if !ValidListingStatus(*p.Status) {
			return nil, fmt.Errorf("%w: unknown status %q: use one of %s",
				ErrInvalidListing, *p.Status, strings.Join(ListingStatuses, ", "))
		}
		sets = append(sets, "status=?")
		args = append(args, *p.Status)
	}
	if p.RoleFamily != nil {
		// A human re-sorting by hand goes through the same vocabulary as the
		// pollers; an off-vocabulary value is rejected rather than stored. Empty is
		// allowed, and means "back to Unsorted".
		value := *p.RoleFamily
		if value != "" {
			canonical, ok := NormalizeRoleFamily(value)
			if !ok {
				return nil, ErrUnknownRoleFamily(value)
			}
			value = canonical
		}
		sets = append(sets, "role_family=?")
		args = append(args, value)
	}
	if p.Relevance != nil {
		sets = append(sets, "relevance=?")
		args = append(args, *p.Relevance)
	}
	if p.Notes != nil {
		sets = append(sets, "notes=?")
		args = append(args, *p.Notes)
	}
	if p.CompMin != nil {
		sets = append(sets, "comp_min=?")
		args = append(args, *p.CompMin)
	}
	if p.CompMax != nil {
		sets = append(sets, "comp_max=?")
		args = append(args, *p.CompMax)
	}
	if p.ApplyURL != nil {
		sets = append(sets, "apply_url=?")
		args = append(args, *p.ApplyURL)
	}
	if len(sets) == 0 {
		return s.GetListing(id)
	}
	sets = append(sets, "updated_at=?")
	args = append(args, now(), id)
	// A soft-deleted listing is not patchable: it is gone as far as every other
	// read path is concerned, and silently accepting a decision on it would leave
	// that decision invisible. Restore it first.
	res, err := s.db.Exec(`UPDATE listings SET `+strings.Join(sets, ", ")+` WHERE id=? AND deleted_at=0`, args...)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	return s.GetListing(id)
}

// SoftDeleteListing marks a listing deleted without destroying it. The row stays,
// so it drops out of every read path but still holds its sig — the row IS the
// dedupe memory, and a later re-poll of the same posting dedupes against it
// rather than proposing it all over again. Reversible with RestoreListing.
func (s *Store) SoftDeleteListing(id int64) error {
	ts := now()
	res, err := s.db.Exec(`UPDATE listings SET deleted_at=?, updated_at=? WHERE id=? AND deleted_at=0`, ts, ts, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Either no such row, or one already deleted. Both mean there is nothing
		// live at this id, which is what the caller asked to remove.
		if _, err := s.GetListingIncludingDeleted(id); err != nil {
			return err
		}
	}
	return nil
}

// RestoreListing brings a soft-deleted listing back exactly as it was. Restoring
// a live listing changes nothing and is not an error, so a repeated call is safe.
func (s *Store) RestoreListing(id int64) (*Listing, error) {
	if _, err := s.db.Exec(`UPDATE listings SET deleted_at=0, updated_at=? WHERE id=? AND deleted_at!=0`, now(), id); err != nil {
		return nil, err
	}
	return s.GetListing(id)
}

// PurgeListing destroys a listing row outright. This is the irreversible one: it
// also throws away the sig, so a poller that rediscovers the posting will file it
// again as new. Prefer SoftDeleteListing.
func (s *Store) PurgeListing(id int64) error {
	res, err := s.db.Exec(`DELETE FROM listings WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// PruneStaleListings soft-deletes every live listing nobody has acted on that has
// either closed or gone stale, and reports how many it swept. Pass 0 for the
// default grace of DefaultPruneGraceDays.
//
// Two rules bound it, and both matter:
//   - Only a listing still in `candidate` is ever swept. Any other status means a
//     person touched this row — marked it interested, applied to it, was rejected
//     from it — and a sweep that took those away would be deleting the record of
//     the work, not tidying an inbox.
//   - An undated listing is never stale. posted_at=0 and closes_at=0 mean nobody
//     knows when it went up or comes down; there is no instant to measure against,
//     so it waits for a real date rather than being swept on a guess.
func (s *Store) PruneStaleListings(graceDays int) (int64, error) {
	if graceDays < 0 {
		return 0, fmt.Errorf("%w: grace_days must not be negative, got %d", ErrInvalidListing, graceDays)
	}
	if graceDays == 0 {
		graceDays = DefaultPruneGraceDays
	}
	ts := now()
	staleBefore := ts - int64(graceDays)*secondsPerDay
	res, err := s.db.Exec(`
		UPDATE listings SET deleted_at=?, updated_at=?
		WHERE deleted_at=0 AND status=?
		  AND ((closes_at > 0 AND closes_at < ?) OR (posted_at > 0 AND posted_at < ?))`,
		ts, ts, ListingStatusCandidate, ts, staleBefore)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if n > 0 {
		log.Printf("[job-store] pruned %d stale candidate listing(s) (grace %d days)", n, graceDays)
	}
	return n, nil
}

const listingCols = `SELECT id, sig, title, company, location, remote, employment_type, seniority,
	role_family, tags, description, description_fetched_at, comp_min, comp_max, comp_currency,
	comp_raw, url, apply_url, source_id, source_name, status, relevance, notes, posted_at,
	closes_at, discovered_at, updated_at, deleted_at`

func scanListing(sc scanner) (*Listing, error) {
	var l Listing
	var remote int
	var tags string
	err := sc.Scan(&l.ID, &l.Sig, &l.Title, &l.Company, &l.Location, &remote, &l.EmploymentType,
		&l.Seniority, &l.RoleFamily, &tags, &l.Description, &l.DescriptionFetchedAt, &l.CompMin,
		&l.CompMax, &l.CompCurrency, &l.CompRaw, &l.URL, &l.ApplyURL, &l.SourceID, &l.SourceName,
		&l.Status, &l.Relevance, &l.Notes, &l.PostedAt, &l.ClosesAt, &l.DiscoveredAt,
		&l.UpdatedAt, &l.DeletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	l.Remote = remote != 0
	l.Tags = unmarshalTags(tags)
	// Derived on every read rather than stored, so it can never drift from the
	// description it describes.
	l.DescriptionThin = IsThinDescription(l.Description)
	return &l, nil
}

// ---- Sources ----

// DefaultCadenceHours is how often a source runs when it does not say. A day:
// jobs move faster than events, and a posting found three days late is often a
// posting already closed.
const DefaultCadenceHours = 24

// UpsertSource inserts or updates a source by name. On a new source next_run_at
// is set to now so the dispatcher picks it up on the next tick — unless it
// arrives proposed, in which case it is inert by construction.
func (s *Store) UpsertSource(src *Source) (*Source, bool, error) {
	if strings.TrimSpace(src.Name) == "" {
		return nil, false, fmt.Errorf("%w: name required", ErrInvalidSource)
	}
	if src.Kind == "" {
		src.Kind = KindResearch
	}
	if !ValidSourceKind(src.Kind) {
		return nil, false, fmt.Errorf("%w: unknown kind %q: use one of %s",
			ErrInvalidSource, src.Kind, strings.Join(SourceKinds, ", "))
	}
	if src.Status == "" {
		src.Status = SourceStatusActive
	}
	if !ValidSourceStatus(src.Status) {
		return nil, false, fmt.Errorf("%w: unknown status %q: use one of %s",
			ErrInvalidSource, src.Status, strings.Join(SourceStatuses, ", "))
	}
	if src.CadenceHours <= 0 {
		src.CadenceHours = DefaultCadenceHours
	}
	// A source's interests use the same vocabulary as the listings it produces, so
	// the two can be compared directly.
	if err := NormalizeSourceRoleFamilies(src); err != nil {
		return nil, false, err
	}
	ts := now()

	var existingID int64
	err := s.db.QueryRow(`SELECT id FROM sources WHERE name=?`, src.Name).Scan(&existingID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		next := src.NextRunAt
		if next == 0 {
			next = ts
		}
		// A brand new source is enabled unless it arrives proposed: a proposal is
		// inert by construction, so approving it is the only thing that can make it
		// run. A scout literally cannot give itself work.
		enabled := src.Status != SourceStatusProposed
		res, err := s.db.Exec(`
			INSERT INTO sources
			  (name, kind, target, company, location, role_families, seniorities, keywords,
			   cadence_hours, model, status, enabled, last_run_at, next_run_at, notes,
			   proposed_by, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			src.Name, src.Kind, src.Target, src.Company, src.Location,
			marshalTags(src.RoleFamilies), marshalTags(src.Seniorities), marshalTags(src.Keywords),
			src.CadenceHours, src.Model, src.Status, boolToInt(enabled), src.LastRunAt, next,
			src.Notes, src.ProposedBy, ts, ts)
		if err != nil {
			return nil, false, err
		}
		id, _ := res.LastInsertId()
		got, err := s.GetSource(id)
		return got, true, err
	case err != nil:
		return nil, false, err
	default:
		// Refresh the description of where to look, but never the decision about
		// whether to look there. status, enabled, last_run_at, next_run_at,
		// proposed_by and created_at are all absent from this UPDATE on purpose: a
		// scout re-proposing a source you rejected, or a re-run of anything you
		// paused, must not switch it back on.
		_, err := s.db.Exec(`
			UPDATE sources SET
			  kind=?, target=?, company=?, location=?, role_families=?, seniorities=?,
			  keywords=?, cadence_hours=?, model=?, notes=?, updated_at=?
			WHERE id=?`,
			src.Kind, src.Target, src.Company, src.Location, marshalTags(src.RoleFamilies),
			marshalTags(src.Seniorities), marshalTags(src.Keywords), src.CadenceHours,
			src.Model, src.Notes, ts, existingID)
		if err != nil {
			return nil, false, err
		}
		got, err := s.GetSource(existingID)
		return got, false, err
	}
}

// SourcePatch carries a human decision about a source. Nil pointers are skipped.
//
// It is deliberately not the whole record: POST /sources rewrites the description
// of where to look and is safe to re-run, while this is the channel for the things
// only a person should change — whether a proposed source is accepted, whether a
// working one is paused, and how hard it runs.
type SourcePatch struct {
	Status       *string `json:"status"`
	Enabled      *bool   `json:"enabled"`
	CadenceHours *int    `json:"cadence_hours"`
	NextRunAt    *int64  `json:"next_run_at"`
	Notes        *string `json:"notes"`
}

// PatchSource applies a partial update and returns the fresh row. Approving a
// proposed source also makes it due now, so an accepted source is picked up on the
// next dispatch instead of waiting out a cadence it never ran.
func (s *Store) PatchSource(id int64, p SourcePatch) (*Source, error) {
	existing, err := s.GetSource(id)
	if err != nil {
		return nil, err
	}
	var sets []string
	var args []any
	if p.Status != nil {
		if !ValidSourceStatus(*p.Status) {
			return nil, fmt.Errorf("%w: unknown status %q: use one of %s",
				ErrInvalidSource, *p.Status, strings.Join(SourceStatuses, ", "))
		}
		sets = append(sets, "status=?")
		args = append(args, *p.Status)
		if *p.Status == SourceStatusActive && existing.Status == SourceStatusProposed {
			sets = append(sets, "enabled=1", "next_run_at=?")
			args = append(args, now())
		}
	}
	if p.Enabled != nil {
		sets = append(sets, "enabled=?")
		args = append(args, boolToInt(*p.Enabled))
	}
	if p.CadenceHours != nil {
		if *p.CadenceHours <= 0 {
			return nil, fmt.Errorf("%w: cadence_hours must be positive", ErrInvalidSource)
		}
		sets = append(sets, "cadence_hours=?")
		args = append(args, *p.CadenceHours)
	}
	if p.NextRunAt != nil {
		sets = append(sets, "next_run_at=?")
		args = append(args, *p.NextRunAt)
	}
	if p.Notes != nil {
		sets = append(sets, "notes=?")
		args = append(args, *p.Notes)
	}
	if len(sets) == 0 {
		return existing, nil
	}
	sets = append(sets, "updated_at=?")
	args = append(args, now(), id)
	res, err := s.db.Exec(`UPDATE sources SET `+strings.Join(sets, ", ")+` WHERE id=?`, args...)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	return s.GetSource(id)
}

// GetSource fetches one source by id.
func (s *Store) GetSource(id int64) (*Source, error) {
	row := s.db.QueryRow(sourceCols+` FROM sources WHERE id=?`, id)
	return scanSource(row)
}

// SourceFilter narrows ListSources.
type SourceFilter struct {
	Kind    string
	Status  string
	OnlyDue bool
	// DueBefore is the instant OnlyDue measures against, defaulting to now. It is
	// deliberately not settable over HTTP: it exists so a test can ask "what would
	// be due in six hours" without sleeping, not so a caller can widen the gate.
	DueBefore int64
}

// ListSources returns sources matching the filter.
func (s *Store) ListSources(f SourceFilter) ([]*Source, error) {
	q := sourceCols + ` FROM sources WHERE 1=1`
	var args []any
	if f.Kind != "" {
		q += ` AND kind=?`
		args = append(args, f.Kind)
	}
	if f.Status != "" {
		q += ` AND status=?`
		args = append(args, f.Status)
	}
	if f.OnlyDue {
		cutoff := f.DueBefore
		if cutoff == 0 {
			cutoff = now()
		}
		// Active and enabled are part of being due, not separate checks a caller
		// might forget. A proposed source must never reach the dispatcher: that is
		// what stops an agent from adding work — and cost — for itself.
		q += ` AND status=? AND enabled=1 AND next_run_at<=?`
		args = append(args, SourceStatusActive, cutoff)
	}
	q += ` ORDER BY next_run_at ASC, id ASC`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Source
	for rows.Next() {
		src, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, src)
	}
	return out, rows.Err()
}

// MarkSourceRan stamps last_run_at=now and advances next_run_at by the cadence
// FROM NOW, not from the old next_run_at — so a source that ran late does not
// then stampede a run of catch-up ticks it has no work for.
func (s *Store) MarkSourceRan(id int64) (*Source, error) {
	src, err := s.GetSource(id)
	if err != nil {
		return nil, err
	}
	ts := now()
	next := ts + int64(src.CadenceHours)*3600
	_, err = s.db.Exec(`UPDATE sources SET last_run_at=?, next_run_at=?, updated_at=? WHERE id=?`,
		ts, next, ts, id)
	if err != nil {
		return nil, err
	}
	return s.GetSource(id)
}

// DeleteSource removes a source.
func (s *Store) DeleteSource(id int64) error {
	res, err := s.db.Exec(`DELETE FROM sources WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

const sourceCols = `SELECT id, name, kind, target, company, location, role_families, seniorities,
	keywords, cadence_hours, model, status, enabled, last_run_at, next_run_at, notes, proposed_by,
	created_at, updated_at`

func scanSource(sc scanner) (*Source, error) {
	var src Source
	var enabled int
	var families, seniorities, keywords string
	err := sc.Scan(&src.ID, &src.Name, &src.Kind, &src.Target, &src.Company, &src.Location,
		&families, &seniorities, &keywords, &src.CadenceHours, &src.Model, &src.Status, &enabled,
		&src.LastRunAt, &src.NextRunAt, &src.Notes, &src.ProposedBy, &src.CreatedAt, &src.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	src.Enabled = enabled != 0
	src.RoleFamilies = unmarshalTags(families)
	src.Seniorities = unmarshalTags(seniorities)
	src.Keywords = unmarshalTags(keywords)
	return &src, nil
}

// ---- Documents ----

// UpsertDocument inserts or updates a document by name, and returns the row plus
// whether it was newly created. Setting is_default clears the flag on every other
// document of the same kind, in one transaction, so "the default resume" is never
// briefly two rows or none.
func (s *Store) UpsertDocument(d *Document) (*Document, bool, error) {
	if strings.TrimSpace(d.Name) == "" {
		return nil, false, fmt.Errorf("%w: name required", ErrInvalidDocument)
	}
	if d.Kind == "" {
		d.Kind = DocumentKindResume
	}
	if !ValidDocumentKind(d.Kind) {
		return nil, false, fmt.Errorf("%w: unknown kind %q: use one of %s",
			ErrInvalidDocument, d.Kind, strings.Join(DocumentKinds, ", "))
	}
	if d.Format == "" {
		d.Format = DocumentFormatMarkdown
	}
	if !ValidDocumentFormat(d.Format) {
		return nil, false, fmt.Errorf("%w: unknown format %q: use one of %s",
			ErrInvalidDocument, d.Format, strings.Join(DocumentFormats, ", "))
	}
	ts := now()

	tx, err := s.db.Begin()
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()

	var id int64
	created := false
	err = tx.QueryRow(`SELECT id FROM documents WHERE name=?`, d.Name).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		res, err := tx.Exec(`
			INSERT INTO documents
			  (name, kind, format, body, source_filename, content_type, blob_sha256, byte_size,
			   extracted_at, is_default, derived_from_document_id, listing_id, notes,
			   created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			d.Name, d.Kind, d.Format, d.Body, d.SourceFilename, d.ContentType, d.BlobSHA256,
			d.ByteSize, d.ExtractedAt, boolToInt(d.IsDefault), d.DerivedFromDocumentID,
			d.ListingID, d.Notes, ts, ts)
		if err != nil {
			return nil, false, err
		}
		id, _ = res.LastInsertId()
		created = true
	case err != nil:
		return nil, false, err
	default:
		if _, err := tx.Exec(`
			UPDATE documents SET kind=?, format=?, body=?, source_filename=?, content_type=?,
			  blob_sha256=?, byte_size=?, extracted_at=?, is_default=?,
			  derived_from_document_id=?, listing_id=?, notes=?, updated_at=?
			WHERE id=?`,
			d.Kind, d.Format, d.Body, d.SourceFilename, d.ContentType, d.BlobSHA256, d.ByteSize,
			d.ExtractedAt, boolToInt(d.IsDefault), d.DerivedFromDocumentID, d.ListingID,
			d.Notes, ts, id); err != nil {
			return nil, false, err
		}
	}
	if d.IsDefault {
		if err := clearOtherDefaults(tx, d.Kind, id, ts); err != nil {
			return nil, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	got, err := s.GetDocument(id)
	return got, created, err
}

// clearOtherDefaults drops the is_default flag from every document of this kind
// except the one being made default. "At most one per kind" is enforced here
// rather than by a partial unique index, so setting a new default is one write
// the caller cannot half-perform.
func clearOtherDefaults(tx *sql.Tx, kind string, keepID int64, ts int64) error {
	_, err := tx.Exec(`UPDATE documents SET is_default=0, updated_at=? WHERE kind=? AND id!=? AND is_default=1`,
		ts, kind, keepID)
	return err
}

// GetDocument fetches one document by id.
func (s *Store) GetDocument(id int64) (*Document, error) {
	row := s.db.QueryRow(documentCols+` FROM documents WHERE id=?`, id)
	return scanDocument(row)
}

// DocumentFilter narrows ListDocuments. Zero values mean "no filter".
type DocumentFilter struct {
	Kind string
	// DerivedFrom lists the tailored copies written from one document, so the
	// variants of a resume can be found from the resume rather than by guessing
	// at their names.
	DerivedFrom int64
	// ListingID lists the documents written for one role.
	ListingID int64
}

// ListDocuments returns documents matching the filter. The default variant of
// each kind sorts first, then by name, so a picker's first entry is the one that
// would be used anyway.
func (s *Store) ListDocuments(f DocumentFilter) ([]*Document, error) {
	q := documentCols + ` FROM documents WHERE 1=1`
	var args []any
	if f.Kind != "" {
		q += ` AND kind=?`
		args = append(args, f.Kind)
	}
	if f.DerivedFrom != 0 {
		q += ` AND derived_from_document_id=?`
		args = append(args, f.DerivedFrom)
	}
	if f.ListingID != 0 {
		q += ` AND listing_id=?`
		args = append(args, f.ListingID)
	}
	q += ` ORDER BY kind ASC, is_default DESC, name ASC`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Document
	for rows.Next() {
		d, err := scanDocument(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DocumentPatch carries a partial document update. Nil pointers are skipped.
type DocumentPatch struct {
	Name      *string `json:"name"`
	Body      *string `json:"body"`
	Format    *string `json:"format"`
	IsDefault *bool   `json:"is_default"`
	Notes     *string `json:"notes"`
}

// PatchDocument applies a partial update and returns the fresh row.
func (s *Store) PatchDocument(id int64, p DocumentPatch) (*Document, error) {
	existing, err := s.GetDocument(id)
	if err != nil {
		return nil, err
	}
	var sets []string
	var args []any
	if p.Name != nil {
		if strings.TrimSpace(*p.Name) == "" {
			return nil, fmt.Errorf("%w: name must not be blank", ErrInvalidDocument)
		}
		sets = append(sets, "name=?")
		args = append(args, *p.Name)
	}
	if p.Body != nil {
		sets = append(sets, "body=?")
		args = append(args, *p.Body)
	}
	if p.Format != nil {
		if !ValidDocumentFormat(*p.Format) {
			return nil, fmt.Errorf("%w: unknown format %q: use one of %s",
				ErrInvalidDocument, *p.Format, strings.Join(DocumentFormats, ", "))
		}
		sets = append(sets, "format=?")
		args = append(args, *p.Format)
	}
	if p.IsDefault != nil {
		sets = append(sets, "is_default=?")
		args = append(args, boolToInt(*p.IsDefault))
	}
	if p.Notes != nil {
		sets = append(sets, "notes=?")
		args = append(args, *p.Notes)
	}
	if len(sets) == 0 {
		return existing, nil
	}
	ts := now()
	sets = append(sets, "updated_at=?")
	args = append(args, ts, id)

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE documents SET `+strings.Join(sets, ", ")+` WHERE id=?`, args...)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	if p.IsDefault != nil && *p.IsDefault {
		if err := clearOtherDefaults(tx, existing.Kind, id, ts); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetDocument(id)
}

// DeleteDocument removes a document. There is no soft delete here: a document
// carries no dedupe signature a poller could rediscover, so keeping the row would
// buy nothing.
func (s *Store) DeleteDocument(id int64) error {
	res, err := s.db.Exec(`DELETE FROM documents WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

const documentCols = `SELECT id, name, kind, format, body, source_filename, content_type,
	blob_sha256, byte_size, extracted_at, is_default, derived_from_document_id, listing_id,
	notes, created_at, updated_at`

func scanDocument(sc scanner) (*Document, error) {
	var d Document
	var isDefault int
	err := sc.Scan(&d.ID, &d.Name, &d.Kind, &d.Format, &d.Body, &d.SourceFilename,
		&d.ContentType, &d.BlobSHA256, &d.ByteSize, &d.ExtractedAt, &isDefault,
		&d.DerivedFromDocumentID, &d.ListingID, &d.Notes, &d.CreatedAt, &d.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	d.IsDefault = isDefault != 0
	return &d, nil
}

// ---- Tailor requests ----

// CreateTailorRequest queues an ask to customize one document for one role.
//
// Both ids must resolve to live rows, and the listing must carry a description
// substantial enough to tailor against. Tailoring against a job title alone
// produces exactly the generic letter this feature exists to avoid, so a thin
// description is refused here rather than discovered later by whoever reads the
// output.
//
// The bar is IsThinDescription, the same predicate that flags a listing as
// description_thin — not merely "not empty". "We are hiring" is thirteen
// characters and tells a writer nothing, so a guard that only caught the empty
// string would wave through the exact case it exists to stop, and every such
// listing is already flagged as incomplete elsewhere in this file.
func (s *Store) CreateTailorRequest(documentID, listingID int64, instructions string) (*TailorRequest, error) {
	if documentID <= 0 {
		return nil, fmt.Errorf("%w: document_id required", ErrInvalidTailorRequest)
	}
	if listingID <= 0 {
		return nil, fmt.Errorf("%w: listing_id required", ErrInvalidTailorRequest)
	}
	if _, err := s.GetDocument(documentID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("%w: document_id %d does not name a document",
				ErrInvalidTailorRequest, documentID)
		}
		return nil, err
	}
	listing, err := s.GetListing(listingID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("%w: listing_id %d does not name a live listing",
				ErrInvalidTailorRequest, listingID)
		}
		return nil, err
	}
	if IsThinDescription(listing.Description) {
		return nil, fmt.Errorf(
			"%w: listing %d has only %d characters of description, under the %d needed to tailor against — fetch the full posting text first (POST /listings with the body, then retry)",
			ErrInvalidTailorRequest, listingID,
			len(strings.TrimSpace(listing.Description)), ThinDescriptionMinChars)
	}
	ts := now()
	res, err := s.db.Exec(`
		INSERT INTO tailor_requests (document_id, listing_id, instructions, status, created_at, updated_at)
		VALUES (?,?,?,?,?,?)`,
		documentID, listingID, instructions, TailorStatusPending, ts, ts)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.GetTailorRequest(id)
}

// GetTailorRequest fetches one tailor request by id.
func (s *Store) GetTailorRequest(id int64) (*TailorRequest, error) {
	row := s.db.QueryRow(tailorCols+` FROM tailor_requests WHERE id=?`, id)
	return scanTailorRequest(row)
}

// TailorRequestFilter narrows ListTailorRequests.
type TailorRequestFilter struct {
	Status    string
	ListingID int64
}

// ListTailorRequests returns tailor requests matching the filter, oldest first
// within a status — the dispatcher wants the queue in the order it was asked for.
func (s *Store) ListTailorRequests(f TailorRequestFilter) ([]*TailorRequest, error) {
	q := tailorCols + ` FROM tailor_requests WHERE 1=1`
	var args []any
	if f.Status != "" {
		q += ` AND status=?`
		args = append(args, f.Status)
	}
	if f.ListingID != 0 {
		q += ` AND listing_id=?`
		args = append(args, f.ListingID)
	}
	q += ` ORDER BY created_at ASC, id ASC`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*TailorRequest
	for rows.Next() {
		t, err := scanTailorRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TailorRequestPatch is the dispatcher's write channel. Nil pointers are skipped.
type TailorRequestPatch struct {
	Status           *string `json:"status"`
	ResultDocumentID *int64  `json:"result_document_id"`
	AgentSessionID   *string `json:"agent_session_id"`
	Error            *string `json:"error"`
	StartedAt        *int64  `json:"started_at"`
	FinishedAt       *int64  `json:"finished_at"`
	Instructions     *string `json:"instructions"`
}

// PatchTailorRequest applies a partial update and returns the fresh row.
func (s *Store) PatchTailorRequest(id int64, p TailorRequestPatch) (*TailorRequest, error) {
	var sets []string
	var args []any
	if p.Status != nil {
		if !ValidTailorStatus(*p.Status) {
			return nil, fmt.Errorf("%w: unknown status %q: use one of %s",
				ErrInvalidTailorRequest, *p.Status, strings.Join(TailorStatuses, ", "))
		}
		sets = append(sets, "status=?")
		args = append(args, *p.Status)
	}
	if p.ResultDocumentID != nil {
		// A result id that names nothing is worse than none: the UI would offer a
		// "view the tailored copy" link onto a 404 and the request would look done.
		if *p.ResultDocumentID != 0 {
			if _, err := s.GetDocument(*p.ResultDocumentID); err != nil {
				if errors.Is(err, ErrNotFound) {
					return nil, fmt.Errorf("%w: result_document_id %d does not name a document",
						ErrInvalidTailorRequest, *p.ResultDocumentID)
				}
				return nil, err
			}
		}
		sets = append(sets, "result_document_id=?")
		args = append(args, *p.ResultDocumentID)
	}
	if p.AgentSessionID != nil {
		sets = append(sets, "agent_session_id=?")
		args = append(args, *p.AgentSessionID)
	}
	if p.Error != nil {
		sets = append(sets, "error=?")
		args = append(args, *p.Error)
	}
	if p.StartedAt != nil {
		sets = append(sets, "started_at=?")
		args = append(args, *p.StartedAt)
	}
	if p.FinishedAt != nil {
		sets = append(sets, "finished_at=?")
		args = append(args, *p.FinishedAt)
	}
	if p.Instructions != nil {
		sets = append(sets, "instructions=?")
		args = append(args, *p.Instructions)
	}
	if len(sets) == 0 {
		return s.GetTailorRequest(id)
	}
	sets = append(sets, "updated_at=?")
	args = append(args, now(), id)
	res, err := s.db.Exec(`UPDATE tailor_requests SET `+strings.Join(sets, ", ")+` WHERE id=?`, args...)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	return s.GetTailorRequest(id)
}

// DeleteTailorRequest removes a tailor request. The document it produced, if any,
// is left alone — it is a document in its own right.
func (s *Store) DeleteTailorRequest(id int64) error {
	res, err := s.db.Exec(`DELETE FROM tailor_requests WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

const tailorCols = `SELECT id, document_id, listing_id, instructions, status, result_document_id,
	agent_session_id, error, created_at, started_at, finished_at, updated_at`

func scanTailorRequest(sc scanner) (*TailorRequest, error) {
	var t TailorRequest
	err := sc.Scan(&t.ID, &t.DocumentID, &t.ListingID, &t.Instructions, &t.Status,
		&t.ResultDocumentID, &t.AgentSessionID, &t.Error, &t.CreatedAt, &t.StartedAt,
		&t.FinishedAt, &t.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// ---- Applications ----

// UpsertApplication inserts or updates the application for a listing, keyed by
// listing_id. There is at most one application per listing.
//
// The listing_id must resolve to a live listing: an application is a record of
// applying to a specific posting, and one pointing at a listing that does not
// exist is not a record of anything. Rejecting it here is what keeps the join on
// ids honest.
func (s *Store) UpsertApplication(a *Application) (*Application, bool, error) {
	if a.ListingID <= 0 {
		return nil, false, fmt.Errorf("%w: listing_id required", ErrInvalidApplication)
	}
	if _, err := s.GetListing(a.ListingID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, false, fmt.Errorf("%w: listing_id %d does not name a live listing",
				ErrInvalidApplication, a.ListingID)
		}
		return nil, false, err
	}
	if a.Status == "" {
		a.Status = ApplicationStatusDraft
	}
	if !ValidApplicationStatus(a.Status) {
		return nil, false, fmt.Errorf("%w: unknown status %q: use one of %s",
			ErrInvalidApplication, a.Status, strings.Join(ApplicationStatuses, ", "))
	}
	ts := now()

	var existingID int64
	err := s.db.QueryRow(`SELECT id FROM applications WHERE listing_id=?`, a.ListingID).Scan(&existingID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		res, err := s.db.Exec(`
			INSERT INTO applications
			  (listing_id, status, resume_document_id, cover_letter_document_id,
			   cover_letter_body, answers, agent_session_id, submitted_at, error,
			   created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			a.ListingID, a.Status, a.ResumeDocumentID, a.CoverLetterDocumentID,
			a.CoverLetterBody, marshalAnswers(a.Answers), a.AgentSessionID, a.SubmittedAt,
			a.Error, ts, ts)
		if err != nil {
			return nil, false, err
		}
		id, _ := res.LastInsertId()
		got, err := s.GetApplication(id)
		return got, true, err
	case err != nil:
		return nil, false, err
	default:
		_, err := s.db.Exec(`
			UPDATE applications SET
			  status=?, resume_document_id=?, cover_letter_document_id=?, cover_letter_body=?,
			  answers=?, agent_session_id=?, submitted_at=?, error=?, updated_at=?
			WHERE id=?`,
			a.Status, a.ResumeDocumentID, a.CoverLetterDocumentID, a.CoverLetterBody,
			marshalAnswers(a.Answers), a.AgentSessionID, a.SubmittedAt, a.Error, ts, existingID)
		if err != nil {
			return nil, false, err
		}
		got, err := s.GetApplication(existingID)
		return got, false, err
	}
}

// GetApplication fetches one application by id.
func (s *Store) GetApplication(id int64) (*Application, error) {
	row := s.db.QueryRow(applicationCols+` FROM applications WHERE id=?`, id)
	return scanApplication(row)
}

// ApplicationFilter narrows ListApplications.
type ApplicationFilter struct {
	Status    string
	ListingID int64
}

// ListApplications returns applications matching the filter, newest first.
func (s *Store) ListApplications(f ApplicationFilter) ([]*Application, error) {
	q := applicationCols + ` FROM applications WHERE 1=1`
	var args []any
	if f.Status != "" {
		q += ` AND status=?`
		args = append(args, f.Status)
	}
	if f.ListingID != 0 {
		q += ` AND listing_id=?`
		args = append(args, f.ListingID)
	}
	q += ` ORDER BY updated_at DESC, id DESC`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Application
	for rows.Next() {
		a, err := scanApplication(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ApplicationPatch carries a partial application update. Nil pointers are skipped.
type ApplicationPatch struct {
	Status                *string              `json:"status"`
	ResumeDocumentID      *int64               `json:"resume_document_id"`
	CoverLetterDocumentID *int64               `json:"cover_letter_document_id"`
	CoverLetterBody       *string              `json:"cover_letter_body"`
	Answers               *[]ApplicationAnswer `json:"answers"`
	AgentSessionID        *string              `json:"agent_session_id"`
	SubmittedAt           *int64               `json:"submitted_at"`
	Error                 *string              `json:"error"`
}

// PatchApplication applies a partial update and returns the fresh row.
func (s *Store) PatchApplication(id int64, p ApplicationPatch) (*Application, error) {
	var sets []string
	var args []any
	if p.Status != nil {
		if !ValidApplicationStatus(*p.Status) {
			return nil, fmt.Errorf("%w: unknown status %q: use one of %s",
				ErrInvalidApplication, *p.Status, strings.Join(ApplicationStatuses, ", "))
		}
		sets = append(sets, "status=?")
		args = append(args, *p.Status)
	}
	if p.ResumeDocumentID != nil {
		sets = append(sets, "resume_document_id=?")
		args = append(args, *p.ResumeDocumentID)
	}
	if p.CoverLetterDocumentID != nil {
		sets = append(sets, "cover_letter_document_id=?")
		args = append(args, *p.CoverLetterDocumentID)
	}
	if p.CoverLetterBody != nil {
		sets = append(sets, "cover_letter_body=?")
		args = append(args, *p.CoverLetterBody)
	}
	if p.Answers != nil {
		sets = append(sets, "answers=?")
		args = append(args, marshalAnswers(*p.Answers))
	}
	if p.AgentSessionID != nil {
		sets = append(sets, "agent_session_id=?")
		args = append(args, *p.AgentSessionID)
	}
	if p.SubmittedAt != nil {
		sets = append(sets, "submitted_at=?")
		args = append(args, *p.SubmittedAt)
	}
	if p.Error != nil {
		sets = append(sets, "error=?")
		args = append(args, *p.Error)
	}
	if len(sets) == 0 {
		return s.GetApplication(id)
	}
	sets = append(sets, "updated_at=?")
	args = append(args, now(), id)
	res, err := s.db.Exec(`UPDATE applications SET `+strings.Join(sets, ", ")+` WHERE id=?`, args...)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	return s.GetApplication(id)
}

// DeleteApplication removes an application.
func (s *Store) DeleteApplication(id int64) error {
	res, err := s.db.Exec(`DELETE FROM applications WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

const applicationCols = `SELECT id, listing_id, status, resume_document_id,
	cover_letter_document_id, cover_letter_body, answers, agent_session_id, submitted_at,
	error, created_at, updated_at`

func scanApplication(sc scanner) (*Application, error) {
	var a Application
	var answers string
	err := sc.Scan(&a.ID, &a.ListingID, &a.Status, &a.ResumeDocumentID, &a.CoverLetterDocumentID,
		&a.CoverLetterBody, &answers, &a.AgentSessionID, &a.SubmittedAt, &a.Error,
		&a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	a.Answers = unmarshalAnswers(answers)
	return &a, nil
}
