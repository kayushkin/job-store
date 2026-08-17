package jobstore

import (
	"fmt"
	"strings"
	"testing"
)

// The two tables below decide what lands in the user's todo queue for every
// application they make, and nothing else on this box names their contents. The
// tests around CreateStandardApplicationTasks all take their expectations out of
// the tables themselves — which is right for the claim they make, that one todo
// is created per shipped row — so a row that changed or vanished left the suite
// green. What was missing is the other claim: that these particular rows ship.
//
// The cost when that fires is invisible. An application quietly stops getting
// one of its follow-ups, or gets one whose wording or due date nobody agreed to.
// There is no error, no log line, and the reminder surfaces have nothing to
// notice missing.

// TestStandardApplicationTasksShipExactlyTheseThreeFollowUps owns a literal of
// the shipped table so a change to it has to be deliberate. Changing the table
// and this literal together is a two-line edit and entirely allowed; changing
// the table alone is what this catches.
func TestStandardApplicationTasksShipExactlyTheseThreeFollowUps(t *testing.T) {
	want := []StandardApplicationTask{
		{TitleFormat: "Follow up with %s about the %s application if no reply in 7 days", DueInDays: 7},
		{TitleFormat: "Research %s and prepare questions for the %s interview", DueInDays: 3},
		{TitleFormat: "Record the outcome of the %s %s application, or mark it ghosted", DueInDays: 30},
	}

	if len(StandardApplicationTasks) != len(want) {
		t.Fatalf("StandardApplicationTasks has %d row(s), want %d: every application "+
			"gets one todo per row, so a row added or dropped here silently changes what "+
			"the user is reminded to do", len(StandardApplicationTasks), len(want))
	}
	for i := range want {
		got := StandardApplicationTasks[i]
		if got.TitleFormat != want[i].TitleFormat {
			t.Errorf("row %d title format = %q, want %q", i, got.TitleFormat, want[i].TitleFormat)
		}
		if got.DueInDays != want[i].DueInDays {
			t.Errorf("row %d comes due in %d day(s), want %d: the offset is the whole "+
				"point of the follow-up, and nothing downstream re-derives it",
				i, got.DueInDays, want[i].DueInDays)
		}
	}
}

// TestStandardApplicationTaskTagsAreJobsAndPersonal pins the tag set for the same
// reason. These tags are how the reminder surfaces tell the user's own life work
// from coding work: the nightly worker's queue read excludes `personal`, so a
// dropped tag does not merely mislabel these todos, it puts the user's job
// follow-ups in front of an unattended agent.
func TestStandardApplicationTaskTagsAreJobsAndPersonal(t *testing.T) {
	want := []string{"jobs", "personal"}
	if strings.Join(StandardApplicationTaskTags, ",") != strings.Join(want, ",") {
		t.Errorf("StandardApplicationTaskTags = %v, want %v", StandardApplicationTaskTags, want)
	}
}

// The literals above pin the rows that ship today. This one pins the contract
// every row has to satisfy, so a row added tomorrow is held even by a change
// that updates the literal to match. Store.CreateStandardApplicationTasks renders
// each row with fmt.Sprintf(TitleFormat, listing.Company, listing.Title), so the
// field's documented contract — the company, then the role title — holds exactly
// when the format carries two plain %s verbs and nothing else.
//
// The plainness is the load-bearing half, and it is not pedantry. Comparing the
// rendered positions of the two values instead would assert nothing: with two
// plain verbs fmt fills them in order, so company-first is true by construction
// and no row could ever redden it. An indexed verb — "%[2]s at %[1]s" — is the
// one way a format can take the arguments in the other order, and it is what a
// future author reaching for a different sentence would write.
func TestEveryStandardTaskFormatTakesTheCompanyThenTheRole(t *testing.T) {
	const company = "Example AI"
	const role = "Staff Inference Engineer"

	for i, task := range StandardApplicationTasks {
		// Escaped %% is a literal percent, not a verb, so it does not count
		// against either total.
		verbs := strings.ReplaceAll(task.TitleFormat, "%%", "")
		if plain, all := strings.Count(verbs, "%s"), strings.Count(verbs, "%"); plain != 2 || all != 2 {
			t.Errorf("row %d format %q has %d plain %%s verb(s) out of %d verb(s), want 2 of 2: "+
				"the call site passes the company and then the role positionally, so any other "+
				"shape either drops a value or takes them in an order the field does not promise",
				i, task.TitleFormat, plain, all)
			continue
		}

		title := fmt.Sprintf(task.TitleFormat, company, role)
		// fmt reports a verb it cannot satisfy in the output itself, and that
		// output is the todo title the user reads.
		if strings.Contains(title, "%!") {
			t.Errorf("row %d renders to %q, carrying an fmt error into the user's todo title", i, title)
			continue
		}
		if !strings.Contains(title, company) || !strings.Contains(title, role) {
			t.Errorf("row %d renders to %q, which drops the company or the role: a todo "+
				"naming neither cannot be matched to an application by eye", i, title)
		}
	}
}

// A follow-up is a thing to do later. An offset of zero or less makes the todo
// due the moment the application is recorded, which puts it straight into the
// overdue pile the reminder coordinator escalates.
func TestEveryStandardTaskComesDueInTheFuture(t *testing.T) {
	for i, task := range StandardApplicationTasks {
		if task.DueInDays <= 0 {
			t.Errorf("row %d comes due in %d day(s): a follow-up due at or before "+
				"creation is overdue the instant it exists", i, task.DueInDays)
		}
	}
}
