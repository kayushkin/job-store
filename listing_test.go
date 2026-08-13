package jobstore

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fullDescription is long enough to clear ThinDescriptionMinChars, so a test
// that is not about thin descriptions is not accidentally about them.
const fullDescription = "We are looking for a senior backend engineer to work on our distributed " +
	"payments platform. You will design and operate Go services on Kubernetes, own the " +
	"reliability of the ledger, and work with product and data teams on new payment rails. " +
	"Requirements: 6+ years of backend experience, deep knowledge of Postgres, and a track " +
	"record of running production systems on call."

func newTestListing(t *testing.T, s *Store, company, title string) *Listing {
	t.Helper()
	l, _, err := s.UpsertListing(&Listing{
		Company: company, Title: title, Location: "Remote (US)", Description: fullDescription,
	})
	if err != nil {
		t.Fatalf("create listing: %v", err)
	}
	return l
}

// A re-poll refreshes what the posting says and nothing about what the user
// decided. This is the property that lets a poller run every day without ever
// having to know which rows a person has touched.
func TestRepollUpsertDoesNotOverwriteTheUserDecision(t *testing.T) {
	s := openTestStore(t)
	created := newTestListing(t, s, "Acme", "Senior Backend Engineer")

	interested := ListingStatusInterested
	notes := "great team, apply before Friday"
	relevance := 91
	decided, err := s.PatchListing(created.ID, ListingPatch{
		Status: &interested, Notes: &notes, Relevance: &relevance,
	})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}

	// The poller comes back the next day with a refreshed description and its own
	// (lower) score, and does not know a person has been here.
	repolled, wasCreated, err := s.UpsertListing(&Listing{
		Company: "Acme", Title: "Senior Backend Engineer", Location: "Remote (US)",
		Description: fullDescription + " Updated: now also owns the fraud service.",
		CompRaw:     "$210k–$260k", Status: ListingStatusCandidate, Relevance: 12,
		Notes: "poller notes that should never land",
	})
	if err != nil {
		t.Fatalf("re-poll: %v", err)
	}
	if wasCreated {
		t.Fatal("the re-poll created a second row; the sig did not dedupe")
	}
	if repolled.ID != decided.ID {
		t.Fatalf("re-poll hit id %d, want %d", repolled.ID, decided.ID)
	}
	if repolled.Status != ListingStatusInterested {
		t.Errorf("status = %q, want the user's %q to survive", repolled.Status, ListingStatusInterested)
	}
	if repolled.Notes != notes {
		t.Errorf("notes = %q, want the user's notes to survive", repolled.Notes)
	}
	if repolled.Relevance != relevance {
		t.Errorf("relevance = %d, want the user's %d to survive", repolled.Relevance, relevance)
	}
	if !strings.Contains(repolled.Description, "fraud service") {
		t.Error("the description was not refreshed; a re-poll must still update what the posting says")
	}
	if repolled.CompRaw != "$210k–$260k" {
		t.Errorf("comp_raw = %q, want the refreshed value", repolled.CompRaw)
	}
}

// A re-poll that arrives without the posting body must not blank a description
// somebody already paid a page fetch for.
func TestRepollWithNoDescriptionKeepsTheOneAlreadyFetched(t *testing.T) {
	s := openTestStore(t)
	created := newTestListing(t, s, "Acme", "Senior Backend Engineer")
	if created.DescriptionFetchedAt == 0 {
		t.Fatal("description_fetched_at was not stamped when a description was written")
	}

	repolled, _, err := s.UpsertListing(&Listing{
		Company: "Acme", Title: "Senior Backend Engineer", Location: "Remote (US)",
		Description: "",
	})
	if err != nil {
		t.Fatalf("re-poll: %v", err)
	}
	if repolled.Description != created.Description {
		t.Errorf("description = %q, want the previously fetched body to survive a bodyless re-poll",
			repolled.Description)
	}
	if repolled.DescriptionFetchedAt != created.DescriptionFetchedAt {
		t.Error("description_fetched_at moved on a re-poll that fetched no description")
	}
}

func TestThinDescriptionIsFlaggedAndAuditable(t *testing.T) {
	s := openTestStore(t)
	thin, _, err := s.UpsertListing(&Listing{
		Company: "Acme", Title: "Backend Engineer", Description: "Come build cool stuff with us!",
	})
	if err != nil {
		t.Fatalf("create thin listing: %v", err)
	}
	// A partial listing beats a lost one: it is stored, but it says it is thin.
	if !thin.DescriptionThin {
		t.Error("description_thin = false on a one-line description; the poller is never told to go back")
	}

	blank, _, err := s.UpsertListing(&Listing{Company: "Acme", Title: "Frontend Engineer"})
	if err != nil {
		t.Fatalf("create blank listing: %v", err)
	}
	if !blank.DescriptionThin {
		t.Error("description_thin = false on an empty description")
	}
	if blank.DescriptionFetchedAt != 0 {
		t.Error("description_fetched_at was stamped for a listing with no description")
	}

	full := newTestListing(t, s, "Acme", "Staff Backend Engineer")
	if full.DescriptionThin {
		t.Error("description_thin = true on a full posting body")
	}

	audit, err := s.ListListings(ListingFilter{ThinDescription: true})
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if len(audit) != 2 {
		t.Fatalf("thin_description listed %d rows, want the 2 thin ones", len(audit))
	}
	for _, l := range audit {
		if l.ID == full.ID {
			t.Error("the audit filter returned a listing with a full description")
		}
	}
}

// Full-text search has to reach into the posting body. "Who wants Kubernetes"
// is a question only the description can answer, and a title-only search
// answers it wrongly with silence.
func TestSearchMatchesATermThatAppearsOnlyInTheDescription(t *testing.T) {
	s := openTestStore(t)
	wanted := newTestListing(t, s, "Acme", "Senior Backend Engineer") // description mentions Kubernetes
	if _, _, err := s.UpsertListing(&Listing{
		Company: "Globex", Title: "Kubernetes-free Frontend Engineer",
		Description: "A React and TypeScript role. No infrastructure work at all, we promise. " +
			"You will build the customer dashboard, own the design system implementation, and " +
			"work closely with design on a component library used across three products.",
	}); err != nil {
		t.Fatalf("create other listing: %v", err)
	}

	got, err := s.ListListings(ListingFilter{Query: "ledger"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 1 || got[0].ID != wanted.ID {
		t.Fatalf("search for a description-only term returned %d row(s), want just the backend role", len(got))
	}

	// And the index tracks updates, not just inserts.
	if _, _, err := s.UpsertListing(&Listing{
		Company: "Acme", Title: "Senior Backend Engineer", Location: "Remote (US)",
		Description: "This role no longer mentions that word. It is now about building an " +
			"internal developer platform, with a focus on build systems, CI throughput and " +
			"the tooling that the product teams use every day to ship.",
	}); err != nil {
		t.Fatalf("re-poll: %v", err)
	}
	got, err = s.ListListings(ListingFilter{Query: "ledger"})
	if err != nil {
		t.Fatalf("search after update: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("search still matched %d row(s) on a term the refreshed posting no longer contains; the FTS triggers are not keeping up", len(got))
	}
}

func TestMalformedSearchQueryIsARejectionNotAnEmptyResult(t *testing.T) {
	s := openTestStore(t)
	newTestListing(t, s, "Acme", "Senior Backend Engineer")

	_, err := s.ListListings(ListingFilter{Query: `"unbalanced quote`})
	if !errors.Is(err, ErrInvalidListing) {
		t.Fatalf("error = %v, want ErrInvalidListing so the caller sees a 400 rather than silent zero rows", err)
	}
	if !strings.Contains(err.Error(), "unbalanced quote") {
		t.Errorf("error %q does not quote the offending query", err)
	}
}

// The status the user set is the record of the work. A sweep that took those
// rows away would be deleting history, not tidying an inbox.
func TestPruneNeverTouchesANonCandidateListing(t *testing.T) {
	s := openTestStore(t)
	longAgo := now() - 400*secondsPerDay

	for _, status := range []string{
		ListingStatusInterested, ListingStatusApplying, ListingStatusApplied,
		ListingStatusRejected, ListingStatusOffer, ListingStatusDismissed, ListingStatusClosed,
	} {
		l, _, err := s.UpsertListing(&Listing{
			Company: "Acme", Title: "Engineer " + status, Description: fullDescription,
			PostedAt: longAgo, ClosesAt: longAgo + secondsPerDay,
		})
		if err != nil {
			t.Fatalf("create %s: %v", status, err)
		}
		set := status
		if _, err := s.PatchListing(l.ID, ListingPatch{Status: &set}); err != nil {
			t.Fatalf("set %s: %v", status, err)
		}
	}
	stale, _, err := s.UpsertListing(&Listing{
		Company: "Acme", Title: "Untouched Engineer", Description: fullDescription,
		PostedAt: longAgo,
	})
	if err != nil {
		t.Fatalf("create stale candidate: %v", err)
	}

	pruned, err := s.PruneStaleListings(0)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("pruned %d listing(s), want only the untouched candidate", pruned)
	}
	if _, err := s.GetListing(stale.ID); !errors.Is(err, ErrNotFound) {
		t.Error("the stale candidate survived the sweep")
	}
	live, err := s.ListListings(ListingFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(live) != 7 {
		t.Errorf("%d listing(s) left live, want the 7 the user had touched", len(live))
	}
}

func TestPruneSweepsClosedAndStaleCandidates(t *testing.T) {
	s := openTestStore(t)
	closed, _, err := s.UpsertListing(&Listing{
		Company: "Acme", Title: "Closed Role", Description: fullDescription,
		PostedAt: now() - 3*secondsPerDay, ClosesAt: now() - secondsPerDay,
	})
	if err != nil {
		t.Fatalf("create closed: %v", err)
	}
	fresh, _, err := s.UpsertListing(&Listing{
		Company: "Acme", Title: "Fresh Role", Description: fullDescription,
		PostedAt: now() - secondsPerDay,
	})
	if err != nil {
		t.Fatalf("create fresh: %v", err)
	}

	pruned, err := s.PruneStaleListings(45)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("pruned %d, want 1 (the closed posting)", pruned)
	}
	if _, err := s.GetListing(closed.ID); !errors.Is(err, ErrNotFound) {
		t.Error("a posting whose closes_at has passed survived the sweep")
	}
	if _, err := s.GetListing(fresh.ID); err != nil {
		t.Errorf("a listing posted yesterday was swept: %v", err)
	}
}

// An undated listing has no instant to be measured against, so it waits for a
// real date rather than being swept on a guess.
func TestUndatedListingIsNeverStale(t *testing.T) {
	s := openTestStore(t)
	undated, _, err := s.UpsertListing(&Listing{
		Company: "Acme", Title: "Undated Role", Description: fullDescription,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.PruneStaleListings(1); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if _, err := s.GetListing(undated.ID); err != nil {
		t.Errorf("an undated listing was pruned: %v", err)
	}
}

// The row is the dedupe memory, which is why deletion is soft. A purge throws
// the memory away too, and the poster comes back as new.
func TestSoftDeleteKeepsTheDedupeMemoryAndRestoreBringsItBack(t *testing.T) {
	s := openTestStore(t)
	original := newTestListing(t, s, "Acme", "Senior Backend Engineer")
	if err := s.SoftDeleteListing(original.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	if _, err := s.GetListing(original.ID); !errors.Is(err, ErrNotFound) {
		t.Error("a soft-deleted listing still reads as live")
	}

	again, created, err := s.UpsertListing(&Listing{
		Company: "Acme", Title: "Senior Backend Engineer", Location: "Remote (US)",
		Description: fullDescription,
	})
	if err != nil {
		t.Fatalf("re-poll: %v", err)
	}
	if created || again.ID != original.ID {
		t.Error("a re-poll of a dismissed posting proposed it again as new; the sig memory was lost")
	}

	restored, err := s.RestoreListing(original.ID)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if restored.DeletedAt != 0 {
		t.Error("restore left the listing deleted")
	}

	if err := s.PurgeListing(original.ID); err != nil {
		t.Fatalf("purge: %v", err)
	}
	fresh, created, err := s.UpsertListing(&Listing{
		Company: "Acme", Title: "Senior Backend Engineer", Location: "Remote (US)",
		Description: fullDescription,
	})
	if err != nil {
		t.Fatalf("re-poll after purge: %v", err)
	}
	if !created || fresh.ID == original.ID {
		t.Error("after a purge the posting should come back as a new row")
	}
}

// A decision recorded on a swept row would be invisible everywhere else, so the
// patch refuses rather than accepting it silently.
func TestPatchingASoftDeletedListingIsNotFound(t *testing.T) {
	s := openTestStore(t)
	l := newTestListing(t, s, "Acme", "Senior Backend Engineer")
	if err := s.SoftDeleteListing(l.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	interested := ListingStatusInterested
	if _, err := s.PatchListing(l.ID, ListingPatch{Status: &interested}); !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

func TestPatchListingRejectsAnUnknownStatusAndRoleFamily(t *testing.T) {
	s := openTestStore(t)
	l := newTestListing(t, s, "Acme", "Senior Backend Engineer")
	bogus := "ghosted"
	if _, err := s.PatchListing(l.ID, ListingPatch{Status: &bogus}); !errors.Is(err, ErrInvalidListing) {
		t.Errorf("error = %v, want ErrInvalidListing", err)
	}
	nonsense := "Sorcery"
	if _, err := s.PatchListing(l.ID, ListingPatch{RoleFamily: &nonsense}); !errors.Is(err, ErrInvalidRoleFamily) {
		t.Errorf("error = %v, want ErrInvalidRoleFamily", err)
	}
}

func TestUndatedListingsSortLast(t *testing.T) {
	s := openTestStore(t)
	if _, _, err := s.UpsertListing(&Listing{
		Company: "Acme", Title: "Undated", Description: fullDescription,
	}); err != nil {
		t.Fatalf("create undated: %v", err)
	}
	if _, _, err := s.UpsertListing(&Listing{
		Company: "Acme", Title: "Old", Description: fullDescription, PostedAt: now() - 10*secondsPerDay,
	}); err != nil {
		t.Fatalf("create old: %v", err)
	}
	if _, _, err := s.UpsertListing(&Listing{
		Company: "Acme", Title: "New", Description: fullDescription, PostedAt: now() - secondsPerDay,
	}); err != nil {
		t.Fatalf("create new: %v", err)
	}

	got, err := s.ListListings(ListingFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := []string{"New", "Old", "Undated"}
	if len(got) != len(want) {
		t.Fatalf("listed %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Title != want[i] {
			t.Fatalf("order = %v, want %v", []string{got[0].Title, got[1].Title, got[2].Title}, want)
		}
	}
}

func TestSigIgnoresPunctuationAndCase(t *testing.T) {
	if Sig("Acme, Inc.", "Senior Backend Engineer", "Remote (US)") !=
		Sig("acme inc", "senior backend engineer", "remote us") {
		t.Error("two spellings of the same posting produced different sigs; they would duplicate")
	}
	if Sig("Acme", "Backend Engineer", "Reno") == Sig("Acme", "Backend Engineer", "Austin") {
		t.Error("two locations collapsed to one sig; one posting would hide the other")
	}
}

// ---- applications ----

func TestApplicationRequiresALiveListing(t *testing.T) {
	s := openTestStore(t)
	if _, _, err := s.UpsertApplication(&Application{ListingID: 9999}); !errors.Is(err, ErrInvalidApplication) {
		t.Errorf("error = %v, want ErrInvalidApplication", err)
	}

	l := newTestListing(t, s, "Acme", "Senior Backend Engineer")
	if err := s.SoftDeleteListing(l.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	if _, _, err := s.UpsertApplication(&Application{ListingID: l.ID}); !errors.Is(err, ErrInvalidApplication) {
		t.Errorf("error = %v on a swept listing, want ErrInvalidApplication", err)
	}
}

func TestApplicationRoundTripsItsAnswers(t *testing.T) {
	s := openTestStore(t)
	l := newTestListing(t, s, "Acme", "Senior Backend Engineer")
	app, created, err := s.UpsertApplication(&Application{
		ListingID: l.ID,
		Answers: []ApplicationAnswer{
			{Question: "Are you authorized to work in the US?", Answer: "Yes"},
			{Question: "Will you require sponsorship?", Answer: "No"},
		},
	})
	if err != nil {
		t.Fatalf("create application: %v", err)
	}
	if !created {
		t.Error("the first application for a listing was not reported as created")
	}
	if app.Status != ApplicationStatusDraft {
		t.Errorf("status = %q, want %q", app.Status, ApplicationStatusDraft)
	}
	if len(app.Answers) != 2 || app.Answers[1].Answer != "No" {
		t.Errorf("answers = %+v, want both screening pairs back", app.Answers)
	}

	again, created, err := s.UpsertApplication(&Application{ListingID: l.ID, Status: ApplicationStatusReady})
	if err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if created || again.ID != app.ID {
		t.Error("a second application for the same listing created a second row")
	}
}

// ---- HTTP surface ----

func newTestServer(t *testing.T) (*httptest.Server, *Store) {
	t.Helper()
	s := openTestStore(t)
	mux := http.NewServeMux()
	RegisterHandlers(mux, s)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, s
}

func TestApplicationWithUnknownListingIsA400(t *testing.T) {
	srv, _ := newTestServer(t)

	resp, err := http.Post(srv.URL+"/applications", "application/json",
		strings.NewReader(`{"listing_id":424242}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(body.Error, "424242") {
		t.Errorf("error %q does not name the offending listing_id", body.Error)
	}
}

func TestMalformedSearchOverHTTPIsA400(t *testing.T) {
	srv, _ := newTestServer(t)
	resp, err := http.Get(srv.URL + `/listings?q=%22unbalanced`)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a malformed FTS query", resp.StatusCode)
	}
}

func TestUpsertListingAcceptsISODates(t *testing.T) {
	srv, _ := newTestServer(t)
	resp, err := http.Post(srv.URL+"/listings", "application/json", strings.NewReader(
		`{"title":"Backend Engineer","company":"Acme","posted":"2026-08-01","closes":"2026-09-01T17:00:00-07:00"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	var got Listing
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.PostedAt == 0 || got.ClosesAt == 0 {
		t.Errorf("posted_at=%d closes_at=%d, want both parsed from the ISO strings", got.PostedAt, got.ClosesAt)
	}
	if !got.DescriptionThin {
		t.Error("a listing posted with no description should come back flagged description_thin")
	}
}

func TestListEndpointsEncodeEmptyCollectionsAsArrays(t *testing.T) {
	srv, _ := newTestServer(t)
	for path, key := range map[string]string{
		"/listings":        "listings",
		"/sources":         "sources",
		"/documents":       "documents",
		"/applications":    "applications",
		"/tailor-requests": "tailor_requests",
	} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		var body map[string]json.RawMessage
		err = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		if string(body[key]) != "[]" {
			t.Errorf("%s returned %q for %q, want [] so a UI can map over it", path, body[key], key)
		}
	}
}
