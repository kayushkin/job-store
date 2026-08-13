package jobstore

import (
	"errors"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	// Point noteboard at a port nothing listens on. The default is the real
	// noteboard on this host, and a test suite that quietly read — or worse,
	// wrote — the user's actual todos would be a test suite with side effects.
	// A test that wants noteboard stands up its own stub and overrides this.
	s.SetNoteboardBaseURL("http://127.0.0.1:1")
	t.Cleanup(func() { s.Close() })
	return s
}

func TestProposedSourceIsNeverDue(t *testing.T) {
	s := openTestStore(t)

	proposed, _, err := s.UpsertSource(&Source{
		Name: "scouted board", Kind: KindBoard, Target: "https://example.com/careers",
		Status: SourceStatusProposed, ProposedBy: "job scout",
	})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if proposed.Status != SourceStatusProposed {
		t.Errorf("status = %q, want %q", proposed.Status, SourceStatusProposed)
	}
	// A proposal must be inert, or an agent could hand itself new work to run.
	if proposed.Enabled {
		t.Error("a proposed source must not be enabled")
	}

	due, err := s.ListSources(SourceFilter{OnlyDue: true})
	if err != nil {
		t.Fatalf("list due: %v", err)
	}
	for _, src := range due {
		if src.ID == proposed.ID {
			t.Fatal("a proposed source reached the due list; the dispatcher would fetch it")
		}
	}
}

// Even with the due cutoff pushed far into the future, a proposed source must
// stay out of the queue. The gate is status, not timing.
func TestProposedSourceIsNotDueEvenFarInTheFuture(t *testing.T) {
	s := openTestStore(t)
	proposed, _, err := s.UpsertSource(&Source{
		Name: "scouted board", Kind: KindBoard, Status: SourceStatusProposed,
	})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	due, err := s.ListSources(SourceFilter{OnlyDue: true, DueBefore: now() + 365*24*3600})
	if err != nil {
		t.Fatalf("list due: %v", err)
	}
	for _, src := range due {
		if src.ID == proposed.ID {
			t.Fatal("a proposed source became due once enough time passed; it must never become due")
		}
	}
}

func TestApprovingAProposedSourceMakesItDue(t *testing.T) {
	s := openTestStore(t)
	proposed, _, err := s.UpsertSource(&Source{
		Name: "scouted board", Kind: KindBoard, Status: SourceStatusProposed,
	})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}

	active := SourceStatusActive
	approved, err := s.PatchSource(proposed.ID, SourcePatch{Status: &active})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if approved.Status != SourceStatusActive || !approved.Enabled {
		t.Fatalf("approved = status %q enabled %v, want active + enabled", approved.Status, approved.Enabled)
	}

	due, err := s.ListSources(SourceFilter{OnlyDue: true})
	if err != nil {
		t.Fatalf("list due: %v", err)
	}
	found := false
	for _, src := range due {
		if src.ID == approved.ID {
			found = true
		}
	}
	if !found {
		t.Error("an approved source should be due now, not waiting out a cadence it never ran")
	}
}

func TestReproposingDoesNotUndoADecision(t *testing.T) {
	s := openTestStore(t)
	first, _, err := s.UpsertSource(&Source{
		Name: "junk board", Kind: KindBoard, Target: "https://example.com/a",
		Status: SourceStatusProposed,
	})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	rejected := SourceStatusRejected
	if _, err := s.PatchSource(first.ID, SourcePatch{Status: &rejected}); err != nil {
		t.Fatalf("reject: %v", err)
	}

	// The scout finds it again next month and re-proposes it. The rejection has to
	// survive, or every scout run resurrects everything already turned down.
	again, created, err := s.UpsertSource(&Source{
		Name: "junk board", Kind: KindBoard, Target: "https://example.com/b",
		Status: SourceStatusProposed,
	})
	if err != nil {
		t.Fatalf("re-propose: %v", err)
	}
	if created {
		t.Error("re-proposing the same name created a second row")
	}
	if again.Status != SourceStatusRejected {
		t.Errorf("status = %q, want it to stay %q", again.Status, SourceStatusRejected)
	}
	if again.Target != "https://example.com/b" {
		t.Errorf("target = %q, want the refreshed value", again.Target)
	}
}

func TestReRunDoesNotResurrectAPausedSource(t *testing.T) {
	s := openTestStore(t)
	src, _, err := s.UpsertSource(&Source{Name: "noisy board", Kind: KindBoard})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	off := false
	if _, err := s.PatchSource(src.ID, SourcePatch{Enabled: &off}); err != nil {
		t.Fatalf("pause: %v", err)
	}

	again, _, err := s.UpsertSource(&Source{Name: "noisy board", Kind: KindBoard, Enabled: true})
	if err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if again.Enabled {
		t.Error("a re-upsert switched a paused source back on")
	}
}

func TestUpsertSourceRejectsUnknownKindAndStatus(t *testing.T) {
	s := openTestStore(t)
	if _, _, err := s.UpsertSource(&Source{Name: "a", Kind: "telepathy"}); !errors.Is(err, ErrInvalidSource) {
		t.Errorf("unknown kind error = %v, want ErrInvalidSource", err)
	}
	if _, _, err := s.UpsertSource(&Source{Name: "b", Kind: KindBoard, Status: "maybe"}); !errors.Is(err, ErrInvalidSource) {
		t.Errorf("unknown status error = %v, want ErrInvalidSource", err)
	}
}

func TestSourceDefaultsToActiveSoExistingCallersKeepWorking(t *testing.T) {
	s := openTestStore(t)
	src, _, err := s.UpsertSource(&Source{Name: "hand added", Kind: KindBoard})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if src.Status != SourceStatusActive || !src.Enabled {
		t.Errorf("got status %q enabled %v, want active + enabled", src.Status, src.Enabled)
	}
	if src.ProposedBy != "" {
		t.Errorf("proposed_by = %q, want empty for a source you added yourself", src.ProposedBy)
	}
	if src.CadenceHours != DefaultCadenceHours {
		t.Errorf("cadence_hours = %d, want the %d-hour default", src.CadenceHours, DefaultCadenceHours)
	}
	// A plain caller who never mentions status gets a source that actually runs.
	due, err := s.ListSources(SourceFilter{OnlyDue: true})
	if err != nil {
		t.Fatalf("list due: %v", err)
	}
	if len(due) != 1 || due[0].ID != src.ID {
		t.Errorf("due = %d source(s), want the one just added", len(due))
	}
}

func TestScoutAndBoardAreValidKinds(t *testing.T) {
	s := openTestStore(t)
	for _, kind := range SourceKinds {
		src, _, err := s.UpsertSource(&Source{
			Name: "source " + kind, Kind: kind, Target: "something", CadenceHours: 720,
		})
		if err != nil {
			t.Fatalf("create %s: %v", kind, err)
		}
		if src.Kind != kind {
			t.Errorf("kind = %q, want %q", src.Kind, kind)
		}
	}
}

// A late run must not queue up the runs it missed. Advancing from now rather
// than from the old next_run_at is what stops a source that was down for a week
// from firing seven times in a row when it comes back.
func TestMarkSourceRanAdvancesFromNow(t *testing.T) {
	s := openTestStore(t)
	src, _, err := s.UpsertSource(&Source{Name: "board", Kind: KindBoard, CadenceHours: 6})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Pretend it was due a week ago and nobody ran it.
	longOverdue := now() - 7*24*3600
	if _, err := s.PatchSource(src.ID, SourcePatch{NextRunAt: &longOverdue}); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	ran, err := s.MarkSourceRan(src.ID)
	if err != nil {
		t.Fatalf("mark ran: %v", err)
	}
	expected := ran.LastRunAt + 6*3600
	if ran.NextRunAt != expected {
		t.Errorf("next_run_at = %d, want %d (now + cadence, not old next + cadence)", ran.NextRunAt, expected)
	}
	if ran.NextRunAt <= now() {
		t.Error("a source that just ran is immediately due again; a late run would stampede")
	}
}

func TestSourceRoleFamiliesAreNormalizedOnWrite(t *testing.T) {
	s := openTestStore(t)
	src, _, err := s.UpsertSource(&Source{
		Name: "board", Kind: KindBoard, RoleFamilies: []string{"MLE", "backend", "swe"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	want := []string{RoleFamilyEngineering, RoleFamilyAI}
	if len(src.RoleFamilies) != len(want) {
		t.Fatalf("role_families = %v, want %v", src.RoleFamilies, want)
	}
	for i := range want {
		if src.RoleFamilies[i] != want[i] {
			t.Fatalf("role_families = %v, want %v", src.RoleFamilies, want)
		}
	}
}

func TestPatchSourceRejectsNonPositiveCadence(t *testing.T) {
	s := openTestStore(t)
	src, _, err := s.UpsertSource(&Source{Name: "board", Kind: KindBoard})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	zero := 0
	if _, err := s.PatchSource(src.ID, SourcePatch{CadenceHours: &zero}); !errors.Is(err, ErrInvalidSource) {
		t.Errorf("error = %v, want ErrInvalidSource", err)
	}
}

func TestGetSourceOnAMissingIDIsNotFound(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.GetSource(4242); !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}
