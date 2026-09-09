package jobstore

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// newTestApplication makes a listing and the application against it, which is the
// starting point of nearly every check below.
func newTestApplication(t *testing.T, s *Store) (*Listing, *Application) {
	t.Helper()
	l := newTestListing(t, s, "Example AI", "Staff Inference Engineer")
	app, created, err := s.UpsertApplication(&Application{ListingID: l.ID})
	if err != nil {
		t.Fatalf("create application: %v", err)
	}
	if !created {
		t.Fatal("the first application for a listing was not reported as created")
	}
	return l, app
}

// ---- the rename ----

func TestApplicationCarriesAgentStatusAndStageSeparately(t *testing.T) {
	s := openTestStore(t)
	_, app := newTestApplication(t, s)
	if app.AgentStatus != ApplicationAgentStatusDraft {
		t.Errorf("agent_status = %q, want %q", app.AgentStatus, ApplicationAgentStatusDraft)
	}
	if app.Stage != ApplicationStageDrafting {
		t.Errorf("stage = %q, want %q", app.Stage, ApplicationStageDrafting)
	}

	// An agent failing to fill a form and a company rejecting you are not the same
	// event, and must not be able to overwrite each other.
	failed := ApplicationAgentStatusFailed
	rejected := ApplicationStageRejected
	got, err := s.PatchApplication(app.ID, ApplicationPatch{AgentStatus: &failed})
	if err != nil {
		t.Fatalf("patch agent_status: %v", err)
	}
	if got.Stage != ApplicationStageDrafting {
		t.Errorf("stage = %q after an automation failure, want it untouched", got.Stage)
	}
	got, err = s.PatchApplication(app.ID, ApplicationPatch{Stage: &rejected})
	if err != nil {
		t.Fatalf("patch stage: %v", err)
	}
	if got.AgentStatus != ApplicationAgentStatusFailed {
		t.Errorf("agent_status = %q after an employer rejection, want it untouched", got.AgentStatus)
	}
}

func TestUnknownStageAndAgentStatusAreRejected(t *testing.T) {
	s := openTestStore(t)
	_, app := newTestApplication(t, s)
	bogus := "ghosting"
	if _, err := s.PatchApplication(app.ID, ApplicationPatch{Stage: &bogus}); !errors.Is(err, ErrInvalidApplication) {
		t.Errorf("unknown stage error = %v, want ErrInvalidApplication", err)
	}
	if _, err := s.PatchApplication(app.ID, ApplicationPatch{AgentStatus: &bogus}); !errors.Is(err, ErrInvalidApplication) {
		t.Errorf("unknown agent_status error = %v, want ErrInvalidApplication", err)
	}
	// The whole vocabulary has to be in the message or a caller cannot retry
	// without guessing. "The whole vocabulary" is the claim, and this loop
	// reads it from ApplicationStages, so it asks nothing about a stage that
	// has been deleted from the table. Measured 2026-08-14: dropping Ready,
	// Interview, Onsite, Offer, Withdrawn or Ghosted left the package green
	// six times out of eleven. The floor is what makes the claim hold.
	_, err := s.PatchApplication(app.ID, ApplicationPatch{Stage: &bogus})
	named := 0
	for _, stage := range ApplicationStages {
		named++
		if !strings.Contains(err.Error(), stage) {
			t.Fatalf("the rejection %q does not name the stage %q", err, stage)
		}
	}
	if named < applicationStageFloor {
		t.Errorf("the rejection was checked against %d stages, want at least %d: a stage has been dropped "+
			"from ApplicationStages, so it is gone from the vocabulary and from this check at once",
			named, applicationStageFloor)
	}
}

// Raise it when the board gains a stage; lower it only in the same commit that
// drops one from ApplicationStages, because dropping a stage narrows what
// PatchApplication will accept.
const applicationStageFloor = 11

// The note on a stage change has to survive the JSON boundary, and that is a
// different claim from "the store persists a note" — which is why this test
// speaks HTTP.
//
// The field was `event_note` here and `note` on POST /applications/{id}/events.
// Every store-level test passed, because a Go struct literal does not care what
// the json tag says. Over the wire, a caller sending `note` — which is what the
// UI sent, from a box captioned "recruiter emailed, screen on Tuesday" — hit a
// deliberately lenient decoder, had the field dropped, and got a 200 back. The
// stage moved and the reason for it was gone.
//
// So: one name for one concept, and a test at the layer where the names differ.
func TestAStageNoteSurvivesTheWireAndIsNotSilentlyDropped(t *testing.T) {
	srv, s := newTestServer(t)
	_, app := newTestApplication(t, s)

	const note = "recruiter emailed, screen on Tuesday"
	req, err := http.NewRequest(http.MethodPatch,
		fmt.Sprintf("%s/applications/%d", srv.URL, app.ID),
		strings.NewReader(`{"stage":"screen","note":`+strconv.Quote(note)+`}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	events, err := s.ListApplicationEvents(app.ID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	var moved *ApplicationEvent
	for i := range events {
		if events[i].StageTo == ApplicationStageScreen {
			moved = events[i]
		}
	}
	if moved == nil {
		t.Fatalf("no event recorded the move to screen; events = %+v", events)
	}
	if moved.Note != note {
		t.Errorf("event note = %q, want %q — the note was dropped crossing the wire", moved.Note, note)
	}
}

// The old field name has to be refused rather than ignored. A caller still
// sending `status` would otherwise get a 200 and no change, and would go on
// believing it had recorded something.
func TestTheOldStatusFieldIsRefusedRatherThanIgnored(t *testing.T) {
	srv, s := newTestServer(t)
	_, app := newTestApplication(t, s)

	for _, probe := range []struct {
		what   string
		method string
		path   string
		body   string
	}{
		{"a create", http.MethodPost, "/applications", fmt.Sprintf(`{"listing_id":%d,"status":"ready"}`, app.ListingID)},
		{"a patch", http.MethodPatch, fmt.Sprintf("/applications/%d", app.ID), `{"status":"submitted"}`},
		{"a list filter", http.MethodGet, "/applications?status=draft", ""},
	} {
		req, err := http.NewRequest(probe.method, srv.URL+probe.path, strings.NewReader(probe.body))
		if err != nil {
			t.Fatalf("%s: build request: %v", probe.what, err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", probe.what, err)
		}
		var body struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s with the old `status` field = HTTP %d, want 400", probe.what, resp.StatusCode)
			continue
		}
		if !strings.Contains(body.Error, "agent_status") || !strings.Contains(body.Error, "stage") {
			t.Errorf("%s: the rejection %q does not name both replacements", probe.what, body.Error)
		}
	}
}

// The rename has to be finished everywhere, not just where the compiler would
// have complained. A leftover `status` on an application in SQL, in a name, or in
// the docs is the lie this test exists to catch.
func TestTheApplicationStatusRenameLeftNoStragglers(t *testing.T) {
	s := openTestStore(t)
	columns, err := s.columnNames("applications")
	if err != nil {
		t.Fatalf("read columns: %v", err)
	}
	if columns["status"] {
		t.Error("applications still has a `status` column")
	}
	for _, column := range []string{"agent_status", "stage", "resume_body_sha256"} {
		if !columns[column] {
			t.Errorf("applications is missing the %q column", column)
		}
	}

	// The wire shape is part of the rename: a `status` key on an application would
	// still be read by a client, whatever the column is called.
	encoded, err := json.Marshal(&Application{})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, present := fields["status"]; present {
		t.Error("an application is still encoded with a `status` field")
	}
	for _, field := range []string{"agent_status", "stage", "resume_body_sha256", "resume_drifted"} {
		if _, present := fields[field]; !present {
			t.Errorf("an application is not encoded with a %q field", field)
		}
	}

	// Prose that explains the rename is welcome; SQL and identifiers that still act
	// on the old name are not.
	stragglers := []string{
		"ApplicationStatuses",
		"ValidApplicationStatus",
		"ApplicationStatusDraft",
		"ApplicationStatusReady",
		"ApplicationStatusSubmitted",
		"ApplicationStatusFailed",
		"applications SET status",
		"a.Status",
		"app.Status",
	}
	err = filepath.WalkDir(".", func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		switch filepath.Ext(path) {
		case ".go", ".sql", ".md", ".sh":
		default:
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(body)
		for _, straggler := range stragglers {
			// This test names every straggler itself, so it would otherwise fail on
			// its own source.
			if path == "application_test.go" {
				continue
			}
			if strings.Contains(text, straggler) {
				t.Errorf("%s still says %q: the rename did not reach it", path, straggler)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}

// A database written before the rename must come through it whole: the column
// renamed in place with every value intact, and the stage backfilled only where
// the old value actually implies one.
func TestMigrationRenamesInPlaceAndKeepsEveryValue(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite3", filepath.Join(dir, "job-store.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// The applications table exactly as it was before this change.
	if _, err := db.Exec(`
		CREATE TABLE applications (
		    id                       INTEGER PRIMARY KEY AUTOINCREMENT,
		    listing_id               INTEGER NOT NULL,
		    status                   TEXT NOT NULL DEFAULT 'draft',
		    resume_document_id       INTEGER NOT NULL DEFAULT 0,
		    cover_letter_document_id INTEGER NOT NULL DEFAULT 0,
		    cover_letter_body        TEXT NOT NULL DEFAULT '',
		    answers                  TEXT NOT NULL DEFAULT '',
		    agent_session_id         TEXT NOT NULL DEFAULT '',
		    submitted_at             INTEGER NOT NULL DEFAULT 0,
		    error                    TEXT NOT NULL DEFAULT '',
		    created_at               INTEGER NOT NULL,
		    updated_at               INTEGER NOT NULL
		);
		INSERT INTO applications (id, listing_id, status, cover_letter_body, created_at, updated_at)
		VALUES (1, 11, 'submitted', 'dear hiring manager', 1, 1),
		       (2, 22, 'failed', '', 2, 2),
		       (3, 33, 'ready', '', 3, 3);`); err != nil {
		t.Fatalf("seed old schema: %v", err)
	}
	db.Close()

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("open store on a pre-rename database: %v", err)
	}
	defer s.Close()

	rows, err := s.db.Query(`SELECT id, agent_status, stage, cover_letter_body FROM applications ORDER BY id`)
	if err != nil {
		t.Fatalf("read migrated rows: %v", err)
	}
	defer rows.Close()
	type migrated struct{ agentStatus, stage, coverLetter string }
	got := map[int64]migrated{}
	for rows.Next() {
		var id int64
		var m migrated
		if err := rows.Scan(&id, &m.agentStatus, &m.stage, &m.coverLetter); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[id] = m
	}
	want := map[int64]migrated{
		1: {ApplicationAgentStatusSubmitted, ApplicationStageSubmitted, "dear hiring manager"},
		2: {ApplicationAgentStatusFailed, ApplicationStageDrafting, ""},
		3: {ApplicationAgentStatusReady, ApplicationStageReady, ""},
	}
	if len(got) != len(want) {
		t.Fatalf("migrated %d row(s), want %d — the migration lost data", len(got), len(want))
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("row %d = %+v, want %+v", id, got[id], w)
		}
	}

	// Idempotent: a second boot changes nothing and does not fail.
	s.Close()
	again, err := Open(dir)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer again.Close()
	var agentStatus string
	if err := again.db.QueryRow(`SELECT agent_status FROM applications WHERE id=1`).Scan(&agentStatus); err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if agentStatus != ApplicationAgentStatusSubmitted {
		t.Errorf("agent_status = %q after a second boot, want it unchanged", agentStatus)
	}
}

// Both column names present at once means one fact has two homes, and this
// migration must refuse rather than pick the one that happens to be first.
func TestMigrationRefusesWhenBothColumnNamesExist(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite3", filepath.Join(dir, "job-store.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE applications (
		    id           INTEGER PRIMARY KEY AUTOINCREMENT,
		    listing_id   INTEGER NOT NULL,
		    status       TEXT NOT NULL DEFAULT 'draft',
		    agent_status TEXT NOT NULL DEFAULT 'draft',
		    created_at   INTEGER NOT NULL,
		    updated_at   INTEGER NOT NULL
		);`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	db.Close()

	if _, err := Open(dir); err == nil {
		t.Fatal("a table with both status and agent_status opened anyway; one of them would be silently ignored")
	}
}

// ---- the timeline ----

func TestAStageChangeAlwaysLeavesATrace(t *testing.T) {
	s := openTestStore(t)
	_, app := newTestApplication(t, s)

	events, err := s.ListApplicationEvents(app.ID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 1 || events[0].StageTo != ApplicationStageDrafting {
		t.Fatalf("events after create = %+v, want the stage it started in", events)
	}

	submitted := ApplicationStageSubmitted
	note := "applied through the careers page"
	if _, err := s.PatchApplication(app.ID, ApplicationPatch{Stage: &submitted, Note: &note}); err != nil {
		t.Fatalf("patch stage: %v", err)
	}
	events, err = s.ListApplicationEvents(app.ID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2 — the stage change appended nothing", len(events))
	}
	last := events[1]
	if last.StageFrom != ApplicationStageDrafting || last.StageTo != ApplicationStageSubmitted {
		t.Errorf("event = %q -> %q, want drafting -> submitted", last.StageFrom, last.StageTo)
	}
	if last.Note != note {
		t.Errorf("note = %q, want %q", last.Note, note)
	}
	if last.Source != EventSourceUser {
		t.Errorf("source = %q, want %q", last.Source, EventSourceUser)
	}

	// Re-sending the stage it is already in is not a change and must not invent a
	// second event.
	if _, err := s.PatchApplication(app.ID, ApplicationPatch{Stage: &submitted}); err != nil {
		t.Fatalf("re-patch: %v", err)
	}
	events, _ = s.ListApplicationEvents(app.ID)
	if len(events) != 2 {
		t.Errorf("events = %d after re-sending the same stage, want 2", len(events))
	}
}

// The event and the stage are written in one transaction, so a failure to record
// the change has to take the change with it. Anything else leaves a stage nobody
// can account for.
func TestAStageChangeRollsBackWhenItsEventCannotBeWritten(t *testing.T) {
	s := openTestStore(t)
	_, app := newTestApplication(t, s)

	if _, err := s.db.Exec(`DROP TABLE application_events`); err != nil {
		t.Fatalf("drop events table: %v", err)
	}
	interview := ApplicationStageInterview
	if _, err := s.PatchApplication(app.ID, ApplicationPatch{Stage: &interview}); err == nil {
		t.Fatal("the stage change succeeded with no timeline to record it in")
	}
	// Read the column straight out of the table: every read path above it now
	// touches the timeline too, and this check is about the one column.
	var stage string
	if err := s.db.QueryRow(`SELECT stage FROM applications WHERE id=?`, app.ID).Scan(&stage); err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if stage != ApplicationStageDrafting {
		t.Errorf("stage = %q, want it rolled back to %q — a stage moved with no trace",
			stage, ApplicationStageDrafting)
	}
}

func TestAnEventCanRecordANoteButNeverAStageChange(t *testing.T) {
	s := openTestStore(t)
	_, app := newTestApplication(t, s)

	got, err := s.RecordApplicationEvent(app.ID, ApplicationEvent{
		Note: "recruiter called, said the team is still interviewing", Source: EventSourceUser,
	})
	if err != nil {
		t.Fatalf("record note: %v", err)
	}
	if got.StageFrom != "" || got.StageTo != "" {
		t.Errorf("a note event carries stages %q -> %q, want neither", got.StageFrom, got.StageTo)
	}

	if _, err := s.RecordApplicationEvent(app.ID, ApplicationEvent{Note: ""}); !errors.Is(err, ErrInvalidApplicationEvent) {
		t.Errorf("empty note error = %v, want ErrInvalidApplicationEvent", err)
	}
	// Writing a stage_to here would let the timeline claim a move the application
	// never made.
	_, err = s.RecordApplicationEvent(app.ID, ApplicationEvent{
		Note: "they said yes", StageTo: ApplicationStageOffer,
	})
	if !errors.Is(err, ErrInvalidApplicationEvent) {
		t.Errorf("stage_to on a note error = %v, want ErrInvalidApplicationEvent", err)
	}
	after, _ := s.GetApplication(app.ID)
	if after.Stage != ApplicationStageDrafting {
		t.Errorf("stage = %q, want the note route to have left it alone", after.Stage)
	}
	if _, err := s.RecordApplicationEvent(app.ID, ApplicationEvent{Note: "x", Source: "telepathy"}); !errors.Is(err, ErrInvalidApplicationEvent) {
		t.Errorf("unknown source error = %v, want ErrInvalidApplicationEvent", err)
	}
}

// ---- emails ----

func TestOnlyALinkedEmailMayDriveAStageChange(t *testing.T) {
	s := openTestStore(t)
	_, app := newTestApplication(t, s)

	proposed, created, err := s.LinkApplicationEmail(app.ID, &ApplicationEmail{
		AccountID: "gmail-personal", MessageID: "18f2c9a1b",
		Subject: "Your application to Example AI", FromAddress: "careers@example.ai",
		LinkedBy: EmailLinkedByMatcher, OccurredAt: now(),
	})
	if err != nil {
		t.Fatalf("propose link: %v", err)
	}
	if !created {
		t.Error("the first link for a message was not reported as created")
	}
	if proposed.Status != EmailLinkStatusProposed {
		t.Fatalf("a matcher's link is %q, want %q — a matcher must not self-approve",
			proposed.Status, EmailLinkStatusProposed)
	}

	if _, err := s.AdvanceApplicationStageFromEmail(proposed.ID, ApplicationStageAcknowledged, ""); !errors.Is(err, ErrInvalidApplicationEmail) {
		t.Fatalf("a proposed email drove a stage change: err = %v", err)
	}
	after, _ := s.GetApplication(app.ID)
	if after.Stage != ApplicationStageDrafting {
		t.Fatalf("stage = %q, want it untouched by a proposed email", after.Stage)
	}
	events, _ := s.ListApplicationEvents(app.ID)
	if len(events) != 1 {
		t.Fatalf("events = %d, want only the creation event", len(events))
	}

	linked := EmailLinkStatusLinked
	if _, err := s.PatchApplicationEmail(proposed.ID, ApplicationEmailPatch{Status: &linked}); err != nil {
		t.Fatalf("confirm link: %v", err)
	}
	moved, err := s.AdvanceApplicationStageFromEmail(proposed.ID, ApplicationStageAcknowledged, "")
	if err != nil {
		t.Fatalf("advance from a confirmed email: %v", err)
	}
	if moved.Stage != ApplicationStageAcknowledged {
		t.Errorf("stage = %q, want %q", moved.Stage, ApplicationStageAcknowledged)
	}
	events, _ = s.ListApplicationEvents(app.ID)
	last := events[len(events)-1]
	if last.Source != EventSourceEmail {
		t.Errorf("event source = %q, want %q", last.Source, EventSourceEmail)
	}
	if !strings.Contains(last.Note, "careers@example.ai") {
		t.Errorf("event note %q does not say which message moved it", last.Note)
	}
}

// A matcher asking for a confirmed link is refused outright rather than quietly
// downgraded, so it learns it cannot self-approve.
func TestAMatcherCannotAskForAConfirmedLink(t *testing.T) {
	s := openTestStore(t)
	_, app := newTestApplication(t, s)
	_, _, err := s.LinkApplicationEmail(app.ID, &ApplicationEmail{
		AccountID: "gmail-personal", MessageID: "abc",
		LinkedBy: EmailLinkedByMatcher, Status: EmailLinkStatusLinked,
	})
	if !errors.Is(err, ErrInvalidApplicationEmail) {
		t.Errorf("error = %v, want ErrInvalidApplicationEmail", err)
	}
}

func TestEmailLinksAreKeyedOnMailstackIdsAndNeverOnTheRFCMessageID(t *testing.T) {
	s := openTestStore(t)
	_, app := newTestApplication(t, s)

	// Two different messages, neither of which carries an RFC Message-ID — it is
	// optional per RFC 5322 §3.6.4, and keying on it would collapse these two.
	first, _, err := s.LinkApplicationEmail(app.ID, &ApplicationEmail{
		AccountID: "gmail-personal", MessageID: "msg-1", Subject: "Thanks for applying",
	})
	if err != nil {
		t.Fatalf("link first: %v", err)
	}
	second, _, err := s.LinkApplicationEmail(app.ID, &ApplicationEmail{
		AccountID: "gmail-personal", MessageID: "msg-2", Subject: "Scheduling a screen",
	})
	if err != nil {
		t.Fatalf("link second: %v", err)
	}
	if first.ID == second.ID {
		t.Fatal("two messages with no RFC Message-ID collapsed onto one row")
	}
	if first.Status != EmailLinkStatusLinked {
		t.Errorf("a link a person made is %q, want %q", first.Status, EmailLinkStatusLinked)
	}

	// The same message id under a different account is a different message.
	other, _, err := s.LinkApplicationEmail(app.ID, &ApplicationEmail{
		AccountID: "outlook-work", MessageID: "msg-1",
	})
	if err != nil {
		t.Fatalf("link other account: %v", err)
	}
	if other.ID == first.ID {
		t.Error("the same message id in two accounts collapsed onto one row")
	}

	// Re-posting the same (account, message) refreshes what it says, and never the
	// verdict — a matcher re-running must not undo a rejection.
	rejected := EmailLinkStatusRejected
	if _, err := s.PatchApplicationEmail(first.ID, ApplicationEmailPatch{Status: &rejected}); err != nil {
		t.Fatalf("reject: %v", err)
	}
	again, created, err := s.LinkApplicationEmail(app.ID, &ApplicationEmail{
		AccountID: "gmail-personal", MessageID: "msg-1", Subject: "Thanks for applying (resent)",
		RFCMessageID: "<CAF=abc@mail.example.ai>", LinkedBy: EmailLinkedByMatcher,
	})
	if err != nil {
		t.Fatalf("re-link: %v", err)
	}
	if created || again.ID != first.ID {
		t.Error("re-posting the same mailstack message created a second row")
	}
	if again.Status != EmailLinkStatusRejected {
		t.Errorf("status = %q after a re-post, want it to stay %q", again.Status, EmailLinkStatusRejected)
	}
	if again.Subject != "Thanks for applying (resent)" {
		t.Errorf("subject = %q, want the refreshed value", again.Subject)
	}
	if again.RFCMessageID != "CAF=abc@mail.example.ai" {
		t.Errorf("rfc_message_id = %q, want the brackets stripped as mailstack stores it", again.RFCMessageID)
	}
}

func TestAnEmailLinkNeedsMailstackIdentifiers(t *testing.T) {
	s := openTestStore(t)
	_, app := newTestApplication(t, s)
	_, _, err := s.LinkApplicationEmail(app.ID, &ApplicationEmail{
		RFCMessageID: "CAF=abc@mail.example.ai", Subject: "no mailstack ids here",
	})
	if !errors.Is(err, ErrInvalidApplicationEmail) {
		t.Fatalf("error = %v, want ErrInvalidApplicationEmail", err)
	}
	if !strings.Contains(err.Error(), "rfc_message_id") {
		t.Errorf("the rejection %q does not explain why the RFC id cannot stand in", err)
	}
}

// ---- tasks ----

// noteboardStub stands in for noteboard: it hands back the ids it assigns, and
// records what was asked of it.
type noteboardStub struct {
	*httptest.Server
	mu      sync.Mutex
	created []NoteboardTodo
	items   map[string]NoteboardTodo
	done    map[string]bool
}

// markDone is how a test says a todo was completed in noteboard, without
// job-store ever being told — which is the point: job-store holds the id and asks.
func (stub *noteboardStub) markDone(noteboardID string) {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	stub.done[noteboardID] = true
}

func newNoteboardStub(t *testing.T) *noteboardStub {
	t.Helper()
	stub := &noteboardStub{items: map[string]NoteboardTodo{}, done: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/items", func(w http.ResponseWriter, r *http.Request) {
		var todo NoteboardTodo
		if err := json.NewDecoder(r.Body).Decode(&todo); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		stub.mu.Lock()
		id := fmt.Sprintf("nb-%d", len(stub.created)+1)
		stub.created = append(stub.created, todo)
		stub.items[id] = todo
		stub.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": id, "type": "todo", "title": todo.Title, "tags": todo.Tags, "status": "open",
		})
	})
	// The bulk read behind open_task_count: every open todo in one request.
	mux.HandleFunc("GET /api/items", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("status") != "open" {
			http.Error(w, `{"error":"this stub only serves the open-todo query"}`, http.StatusBadRequest)
			return
		}
		stub.mu.Lock()
		items := make([]map[string]any, 0, len(stub.items))
		for id, todo := range stub.items {
			if stub.done[id] {
				continue
			}
			items = append(items, map[string]any{"id": id, "type": "todo", "title": todo.Title, "status": "open"})
		}
		stub.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(items)
	})
	mux.HandleFunc("GET /api/items/{id}", func(w http.ResponseWriter, r *http.Request) {
		stub.mu.Lock()
		todo, ok := stub.items[r.PathValue("id")]
		status := "open"
		if stub.done[r.PathValue("id")] {
			status = "done"
		}
		stub.mu.Unlock()
		if !ok {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": r.PathValue("id"), "type": "todo", "title": todo.Title,
			"tags": todo.Tags, "status": status,
		})
	})
	stub.Server = httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	return stub
}

func TestStandardTasksAreCreatedInNoteboardAndStoredByTheIdItReturns(t *testing.T) {
	s := openTestStore(t)
	stub := newNoteboardStub(t)
	s.SetNoteboardBaseURL(stub.URL)
	_, app := newTestApplication(t, s)

	tasks, err := s.CreateStandardApplicationTasks(app.ID)
	if err != nil {
		t.Fatalf("create standard tasks: %v", err)
	}
	if len(tasks) != len(StandardApplicationTasks) {
		t.Fatalf("created %d task(s), want %d", len(tasks), len(StandardApplicationTasks))
	}
	stub.mu.Lock()
	created := append([]NoteboardTodo(nil), stub.created...)
	stub.mu.Unlock()
	if len(created) != len(StandardApplicationTasks) {
		t.Fatalf("noteboard was asked for %d todo(s), want %d", len(created), len(StandardApplicationTasks))
	}
	for i, todo := range created {
		if !strings.Contains(todo.Title, "Example AI") || !strings.Contains(todo.Title, "Staff Inference Engineer") {
			t.Errorf("todo %d title %q names neither the company nor the role", i, todo.Title)
		}
		if strings.Join(todo.Tags, ",") != strings.Join(StandardApplicationTaskTags, ",") {
			t.Errorf("todo %d tags = %v, want %v", i, todo.Tags, StandardApplicationTaskTags)
		}
		if todo.DueAt == "" {
			t.Errorf("todo %d has no due date, so nothing will ever surface it", i)
		}
	}
	// The ids stored are the ones noteboard assigned, never ones invented here.
	for i, task := range tasks {
		want := fmt.Sprintf("nb-%d", i+1)
		if task.NoteboardID != want {
			t.Errorf("task %d links %q, want the id noteboard returned (%q)", i, task.NoteboardID, want)
		}
	}
	// And nothing else about the todo came along: this table stores ids only.
	columns, err := s.columnNames("application_tasks")
	if err != nil {
		t.Fatalf("read columns: %v", err)
	}
	for column := range columns {
		switch column {
		case "id", "application_id", "noteboard_id", "created_at":
		default:
			t.Errorf("application_tasks has a %q column: noteboard owns the content, not this store", column)
		}
	}

	// Running it again would put a second copy of every follow-up in the queue.
	if _, err := s.CreateStandardApplicationTasks(app.ID); !errors.Is(err, ErrInvalidApplicationTask) {
		t.Errorf("a second standard-task run error = %v, want ErrInvalidApplicationTask", err)
	}
}

func TestExpandingTasksReadsThemThroughNoteboardAtRequestTime(t *testing.T) {
	s := openTestStore(t)
	stub := newNoteboardStub(t)
	s.SetNoteboardBaseURL(stub.URL)
	_, app := newTestApplication(t, s)
	if _, err := s.CreateStandardApplicationTasks(app.ID); err != nil {
		t.Fatalf("create standard tasks: %v", err)
	}

	expansion, err := s.ExpandApplicationTasks(app.ID)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if expansion.Error != "" {
		t.Fatalf("expansion reported %q with noteboard up", expansion.Error)
	}
	if len(expansion.Items) != len(StandardApplicationTasks) {
		t.Fatalf("expanded %d item(s), want %d", len(expansion.Items), len(StandardApplicationTasks))
	}
	var first map[string]any
	if err := json.Unmarshal(expansion.Items[0].Item, &first); err != nil {
		t.Fatalf("the expanded item is not noteboard's own JSON: %v", err)
	}
	if first["title"] == nil || first["status"] != "open" {
		t.Errorf("expanded item = %v, want noteboard's record passed through unchanged", first)
	}
}

// An unreachable noteboard has to be said out loud. An application quietly
// showing zero outstanding tasks is worse than one that admits it cannot tell.
func TestExpandingTasksFailsLoudlyWhenNoteboardIsDown(t *testing.T) {
	s := openTestStore(t)
	stub := newNoteboardStub(t)
	s.SetNoteboardBaseURL(stub.URL)
	_, app := newTestApplication(t, s)
	if _, err := s.CreateStandardApplicationTasks(app.ID); err != nil {
		t.Fatalf("create standard tasks: %v", err)
	}
	stub.Close() // noteboard goes away

	expansion, err := s.ExpandApplicationTasks(app.ID)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if expansion.Error == "" {
		t.Fatal("an unreachable noteboard produced no error at all")
	}
	if expansion.Items != nil {
		t.Errorf("items = %v with noteboard down, want null so it can never read as \"no tasks\"",
			expansion.Items)
	}

	// Over HTTP the same thing: the application is served, the task field says it
	// could not be read.
	mux := http.NewServeMux()
	RegisterHandlers(mux, s)
	httpSrv := httptest.NewServer(mux)
	defer httpSrv.Close()
	resp, err := http.Get(fmt.Sprintf("%s/applications/%d?expand=tasks", httpSrv.URL, app.ID))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — the application itself is still readable", resp.StatusCode)
	}
	var got Application
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Tasks == nil || got.Tasks.Error == "" {
		t.Fatalf("tasks = %+v, want an explicit error", got.Tasks)
	}
	if got.Tasks.Items != nil {
		t.Errorf("tasks.items = %v, want null rather than an empty list", got.Tasks.Items)
	}
}

func TestLinkingATaskStoresOnlyTheNoteboardID(t *testing.T) {
	s := openTestStore(t)
	_, app := newTestApplication(t, s)

	if _, _, err := s.LinkApplicationTask(app.ID, "  "); !errors.Is(err, ErrInvalidApplicationTask) {
		t.Errorf("blank noteboard_id error = %v, want ErrInvalidApplicationTask", err)
	}
	link, created, err := s.LinkApplicationTask(app.ID, "93c345b7-7536-4f37-a76a-25dc25d015a3")
	if err != nil {
		t.Fatalf("link: %v", err)
	}
	if !created {
		t.Error("the first link was not reported as created")
	}
	again, created, err := s.LinkApplicationTask(app.ID, "93c345b7-7536-4f37-a76a-25dc25d015a3")
	if err != nil {
		t.Fatalf("re-link: %v", err)
	}
	if created || again.ID != link.ID {
		t.Error("linking the same todo twice created a second row")
	}
	if err := s.DeleteApplicationTask(link.ID); err != nil {
		t.Fatalf("unlink: %v", err)
	}
	if _, err := s.GetApplicationTask(link.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v after unlinking, want ErrNotFound", err)
	}
}

// ---- which resume was actually sent ----

func TestTheSubmittedResumeIsPinnedAndDriftIsReportedWhenItIsEdited(t *testing.T) {
	s := openTestStore(t)
	doc, _, err := s.UpsertDocument(&Document{
		Name: "resume", Kind: DocumentKindResume, Body: "# Jane Dev\n\n- Go, SQLite",
	})
	if err != nil {
		t.Fatalf("create document: %v", err)
	}
	l := newTestListing(t, s, "Example AI", "Staff Inference Engineer")
	app, _, err := s.UpsertApplication(&Application{ListingID: l.ID, ResumeDocumentID: doc.ID})
	if err != nil {
		t.Fatalf("create application: %v", err)
	}
	if app.ResumeBodySHA256 != "" {
		t.Fatal("a draft application already pinned a resume; nothing has been sent yet")
	}

	submitted := ApplicationAgentStatusSubmitted
	sent, err := s.PatchApplication(app.ID, ApplicationPatch{AgentStatus: &submitted})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if sent.ResumeBodySHA256 == "" {
		t.Fatal("submitting pinned nothing: the record cannot say WHICH version was sent")
	}
	if sent.ResumeDrifted {
		t.Error("resume_drifted is true the moment it was pinned")
	}
	pinned := sent.ResumeBodySHA256

	// Improving the resume next month is normal — and the copy the employer holds
	// is now provably not the copy on screen.
	body := "# Jane Dev\n\n- Go, SQLite, and a new bullet point"
	if _, err := s.PatchDocument(doc.ID, DocumentPatch{Body: &body}); err != nil {
		t.Fatalf("edit document: %v", err)
	}
	after, err := s.GetApplication(app.ID)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if !after.ResumeDrifted {
		t.Error("the resume was edited and resume_drifted stayed false")
	}
	if after.ResumeBodySHA256 != pinned {
		t.Errorf("the pin changed to %q: it records what was SENT and must never be rewritten",
			after.ResumeBodySHA256)
	}
	// A list has to say the same thing a single read does.
	listed, err := s.ListApplications(ApplicationFilter{ListingID: l.ID})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 1 || !listed[0].ResumeDrifted {
		t.Error("the list does not report the drift the detail view does")
	}

	// Reaching submitted again must not restate history as today's resume.
	if _, err := s.PatchApplication(app.ID, ApplicationPatch{AgentStatus: &submitted}); err != nil {
		t.Fatalf("re-submit: %v", err)
	}
	final, _ := s.GetApplication(app.ID)
	if final.ResumeBodySHA256 != pinned {
		t.Error("a second submit rewrote the pin")
	}
}

func TestSubmittingWithAResumeThatNamesNoDocumentIsRefused(t *testing.T) {
	s := openTestStore(t)
	l := newTestListing(t, s, "Example AI", "Staff Inference Engineer")
	app, _, err := s.UpsertApplication(&Application{ListingID: l.ID, ResumeDocumentID: 4242})
	if err != nil {
		t.Fatalf("create application: %v", err)
	}
	submitted := ApplicationAgentStatusSubmitted
	if _, err := s.PatchApplication(app.ID, ApplicationPatch{AgentStatus: &submitted}); !errors.Is(err, ErrInvalidApplication) {
		t.Errorf("error = %v, want ErrInvalidApplication", err)
	}
	after, _ := s.GetApplication(app.ID)
	if after.AgentStatus == ApplicationAgentStatusSubmitted {
		t.Error("the application was marked submitted with nothing to pin as the copy that was sent")
	}
}

// ---- the listing hands over the pipeline ----

func TestCreatingAnApplicationMarksTheListingApplied(t *testing.T) {
	s := openTestStore(t)
	l := newTestListing(t, s, "Example AI", "Staff Inference Engineer")
	interested := ListingStatusInterested
	if _, err := s.PatchListing(l.ID, ListingPatch{Status: &interested}); err != nil {
		t.Fatalf("mark interested: %v", err)
	}

	if _, _, err := s.UpsertApplication(&Application{ListingID: l.ID}); err != nil {
		t.Fatalf("create application: %v", err)
	}
	got, err := s.GetListing(l.ID)
	if err != nil {
		t.Fatalf("re-read listing: %v", err)
	}
	if got.Status != ListingStatusApplied {
		t.Errorf("listing status = %q, want %q — the listing hands the pipeline to the application",
			got.Status, ListingStatusApplied)
	}
}

// Once an application exists it owns the pipeline, and the listing status may not
// answer the same question differently.
func TestTheListingStatusStopsMovingOnceAnApplicationExists(t *testing.T) {
	s := openTestStore(t)
	l, app := newTestApplication(t, s)

	dismissed := ListingStatusDismissed
	_, err := s.PatchListing(l.ID, ListingPatch{Status: &dismissed})
	if !errors.Is(err, ErrInvalidListing) {
		t.Fatalf("error = %v, want ErrInvalidListing", err)
	}
	if !strings.Contains(err.Error(), fmt.Sprint(app.ID)) {
		t.Errorf("the refusal %q does not point at the application to move instead", err)
	}
	got, _ := s.GetListing(l.ID)
	if got.Status != ListingStatusApplied {
		t.Errorf("listing status = %q, want it left at %q", got.Status, ListingStatusApplied)
	}
	// Everything else about the listing is still editable — only the pipeline
	// question has an owner now.
	notes := "phone screen went well"
	if _, err := s.PatchListing(l.ID, ListingPatch{Notes: &notes}); err != nil {
		t.Errorf("patching notes on an applied listing: %v", err)
	}
}

// The summary numbers a board needs have to come from this store in one read, and
// last_activity_at has to mean "something happened", not "a row was written".
func TestApplicationSummaryCountsComeBackWithTheRow(t *testing.T) {
	s := openTestStore(t)
	stub := newNoteboardStub(t)
	s.SetNoteboardBaseURL(stub.URL)
	_, app := newTestApplication(t, s)

	created, err := s.GetApplication(app.ID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if created.EmailCount != 0 || created.TaskCount != 0 {
		t.Errorf("counts on a new application = %d email(s), %d task(s), want none",
			created.EmailCount, created.TaskCount)
	}
	if created.LastActivityAt != created.CreatedAt {
		t.Errorf("last_activity_at = %d, want the creation event at %d",
			created.LastActivityAt, created.CreatedAt)
	}

	// A proposed link is a guess, not the employer writing back.
	proposed, _, err := s.LinkApplicationEmail(app.ID, &ApplicationEmail{
		AccountID: "gmail-personal", MessageID: "m-1", LinkedBy: EmailLinkedByMatcher,
		OccurredAt: now() + 3600,
	})
	if err != nil {
		t.Fatalf("propose link: %v", err)
	}
	withProposal, _ := s.GetApplication(app.ID)
	if withProposal.EmailCount != 0 {
		t.Errorf("email_count = %d, want a proposal not to count", withProposal.EmailCount)
	}
	if withProposal.LastActivityAt != created.LastActivityAt {
		t.Error("a proposed link counted as activity; it is a guess nobody has accepted")
	}

	linked := EmailLinkStatusLinked
	if _, err := s.PatchApplicationEmail(proposed.ID, ApplicationEmailPatch{Status: &linked}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	confirmed, _ := s.GetApplication(app.ID)
	if confirmed.EmailCount != 1 {
		t.Errorf("email_count = %d, want 1", confirmed.EmailCount)
	}
	if confirmed.LastActivityAt != proposed.OccurredAt {
		t.Errorf("last_activity_at = %d, want the email's own time %d",
			confirmed.LastActivityAt, proposed.OccurredAt)
	}

	// An automation retry rewrites updated_at and changes nothing that happened.
	session := "session-42"
	touched, err := s.PatchApplication(app.ID, ApplicationPatch{AgentSessionID: &session})
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	if touched.LastActivityAt != proposed.OccurredAt {
		t.Errorf("last_activity_at = %d after an agent write, want the last real event at %d",
			touched.LastActivityAt, proposed.OccurredAt)
	}

	// Open task counts: one read of noteboard for the whole set.
	tasks, err := s.CreateStandardApplicationTasks(app.ID)
	if err != nil {
		t.Fatalf("standard tasks: %v", err)
	}
	stub.markDone(tasks[0].NoteboardID)
	listed, err := s.ListApplications(ApplicationFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("listed %d application(s), want 1", len(listed))
	}
	if listed[0].TaskCount != len(tasks) {
		t.Errorf("task_count = %d, want %d", listed[0].TaskCount, len(tasks))
	}
	if listed[0].OpenTaskCount != nil {
		t.Error("open_task_count came back from the store without anyone asking noteboard")
	}
	if err := s.CountOpenApplicationTasks(listed); err != nil {
		t.Fatalf("count open tasks: %v", err)
	}
	if listed[0].OpenTaskCount == nil || *listed[0].OpenTaskCount != len(tasks)-1 {
		t.Errorf("open_task_count = %v, want %d — the one marked done in noteboard is not open",
			listed[0].OpenTaskCount, len(tasks)-1)
	}

	// noteboard down: null, never zero. Zero would read as "nothing outstanding".
	stub.Close()
	if err := s.CountOpenApplicationTasks(listed); err == nil {
		t.Error("an unreachable noteboard produced no error")
	}
	if listed[0].OpenTaskCount != nil {
		t.Errorf("open_task_count = %d with noteboard down, want null: a number here would be "+
			"either a stale answer or a made-up zero", *listed[0].OpenTaskCount)
	}
}

// ---- HTTP surface ----

func TestApplicationSubroutesOverHTTP(t *testing.T) {
	srv, s := newTestServer(t)
	stub := newNoteboardStub(t)
	s.SetNoteboardBaseURL(stub.URL)
	_, app := newTestApplication(t, s)
	base := fmt.Sprintf("%s/applications/%d", srv.URL, app.ID)

	post := func(t *testing.T, url, body string) *http.Response {
		t.Helper()
		resp, err := http.Post(url, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("post %s: %v", url, err)
		}
		return resp
	}

	// A stage change over HTTP appends its event.
	req, _ := http.NewRequest(http.MethodPatch, base, strings.NewReader(`{"stage":"submitted"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch status = %d, want 200", resp.StatusCode)
	}
	resp, err = http.Get(base + "/events")
	if err != nil {
		t.Fatalf("get events: %v", err)
	}
	var events struct {
		Events []ApplicationEvent `json:"events"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&events)
	resp.Body.Close()
	if len(events.Events) != 2 || events.Events[1].StageTo != ApplicationStageSubmitted {
		t.Fatalf("events = %+v, want the create plus the stage change", events.Events)
	}

	// A note.
	resp = post(t, base+"/events", `{"note":"referral submitted by a friend","source":"user"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("post event status = %d, want 201", resp.StatusCode)
	}

	// A proposed email, and the refusal to let it move anything.
	resp = post(t, base+"/emails",
		`{"account_id":"gmail-personal","message_id":"m-1","subject":"Thanks","linked_by":"matcher"}`)
	var email ApplicationEmail
	_ = json.NewDecoder(resp.Body).Decode(&email)
	resp.Body.Close()
	if email.Status != EmailLinkStatusProposed {
		t.Fatalf("linked_by matcher gave status %q, want proposed", email.Status)
	}
	stageURL := fmt.Sprintf("%s/application-emails/%d/stage", srv.URL, email.ID)
	resp = post(t, stageURL, `{"stage":"acknowledged"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a proposed email drove a stage over HTTP: status = %d, want 400", resp.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodPatch,
		fmt.Sprintf("%s/application-emails/%d", srv.URL, email.ID), strings.NewReader(`{"status":"linked"}`))
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	resp = post(t, stageURL, `{"stage":"acknowledged"}`)
	var moved Application
	_ = json.NewDecoder(resp.Body).Decode(&moved)
	resp.Body.Close()
	if moved.Stage != ApplicationStageAcknowledged {
		t.Errorf("stage = %q after a confirmed email, want acknowledged", moved.Stage)
	}

	// Standard tasks, then the expansion.
	resp = post(t, base+"/tasks/standard", "")
	var standard struct {
		Tasks []ApplicationTask `json:"tasks"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&standard)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || len(standard.Tasks) != len(StandardApplicationTasks) {
		t.Fatalf("standard tasks = %d (HTTP %d), want %d", len(standard.Tasks), resp.StatusCode,
			len(StandardApplicationTasks))
	}
	resp, err = http.Get(base + "?expand=tasks")
	if err != nil {
		t.Fatalf("get with expand: %v", err)
	}
	var expanded Application
	_ = json.NewDecoder(resp.Body).Decode(&expanded)
	resp.Body.Close()
	if expanded.Tasks == nil || expanded.Tasks.Error != "" || len(expanded.Tasks.Items) != len(StandardApplicationTasks) {
		t.Fatalf("expanded tasks = %+v", expanded.Tasks)
	}
	// Without the parameter the field is absent, not an empty list.
	resp, _ = http.Get(base)
	var plain map[string]json.RawMessage
	_ = json.NewDecoder(resp.Body).Decode(&plain)
	resp.Body.Close()
	if _, present := plain["tasks"]; present {
		t.Error("tasks came back without ?expand=tasks, which would look like a list read from noteboard")
	}

	// Empty collections encode as arrays, the same as every other list route.
	for path, key := range map[string]string{"/events": "events", "/emails": "emails", "/tasks": "tasks"} {
		_, other := newTestApplicationOnItsOwnListing(t, s)
		resp, err := http.Get(fmt.Sprintf("%s/applications/%d%s", srv.URL, other.ID, path))
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		var body map[string]json.RawMessage
		_ = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if string(body[key]) == "null" {
			t.Errorf("%s returned null for %q, want [] so a UI can map over it", path, key)
		}
	}
}

// distinctListingCounter keeps every listing a test makes on its own row, since
// the sig dedupes on company and title and there is at most one application per
// listing.
var distinctListingCounter atomic.Int64

// newTestApplicationOnItsOwnListing makes an application against a listing nobody
// else in the test is using.
func newTestApplicationOnItsOwnListing(t *testing.T, s *Store) (*Listing, *Application) {
	t.Helper()
	n := distinctListingCounter.Add(1)
	l := newTestListing(t, s, fmt.Sprintf("Company %d", n), fmt.Sprintf("Role %d", n))
	app, _, err := s.UpsertApplication(&Application{ListingID: l.ID})
	if err != nil {
		t.Fatalf("create application: %v", err)
	}
	return l, app
}
