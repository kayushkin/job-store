package jobstore

import (
	"errors"
	"strings"
	"testing"
)

func TestCanonicalNamesResolveToThemselves(t *testing.T) {
	for _, f := range RoleFamilies {
		for _, spelling := range []string{f.Name, strings.ToLower(f.Name), strings.ToUpper(f.Name)} {
			got, ok := NormalizeRoleFamily(spelling)
			if !ok || got != f.Name {
				t.Errorf("NormalizeRoleFamily(%q) = %q, %v; want %q, true", spelling, got, ok, f.Name)
			}
		}
	}
}

// The longest matching phrase decides, not the first word that happens to be in
// the table. Every case here has a shorter phrase inside it that resolves
// somewhere else, so a scan that stopped at the short match would get them wrong.
func TestLongestSpanBeatsTheShortMatch(t *testing.T) {
	cases := []struct {
		raw          string
		want         string
		shortMatch   string
		shortMatchTo string
	}{
		{"Engineering Manager", RoleFamilyLeadership, "engineering", RoleFamilyEngineering},
		{"Senior Engineering Manager", RoleFamilyLeadership, "engineering", RoleFamilyEngineering},
		{"Data Engineer", RoleFamilyData, "engineer", RoleFamilyEngineering},
		{"ML Platform Engineer", RoleFamilyAI, "platform", RoleFamilyEngineering},
		{"Data Platform Engineer", RoleFamilyData, "platform", RoleFamilyEngineering},
		{"Director of Engineering", RoleFamilyLeadership, "engineering", RoleFamilyEngineering},
		{"Head of Design", RoleFamilyLeadership, "design", RoleFamilyDesign},
		{"Product Manager", RoleFamilyProduct, "manager", RoleFamilyLeadership},
		{"Technical Program Manager", RoleFamilyProduct, "manager", RoleFamilyLeadership},
		{"UX Researcher", RoleFamilyDesign, "ux", RoleFamilyDesign},
		{"Research Engineer", RoleFamilyAI, "engineer", RoleFamilyEngineering},
		{"Business Analyst", RoleFamilyProduct, "analyst", RoleFamilyData},
		{"AI Infrastructure Engineer", RoleFamilyAI, "infrastructure", RoleFamilyEngineering},
		{"Staff Machine Learning Platform Engineer", RoleFamilyAI, "platform", RoleFamilyEngineering},
		{"Data Infrastructure Engineer", RoleFamilyData, "infrastructure", RoleFamilyEngineering},
		{"Senior Analytics Engineer", RoleFamilyData, "engineer", RoleFamilyEngineering},
	}
	for _, c := range cases {
		// Establish that the short phrase really does point elsewhere, so this test
		// keeps testing longest-span rather than passing by coincidence if the
		// synonym table changes.
		if short, ok := NormalizeRoleFamily(c.shortMatch); !ok || short != c.shortMatchTo {
			t.Fatalf("premise broken: %q resolves to %q, expected %q", c.shortMatch, short, c.shortMatchTo)
		}
		got, ok := NormalizeRoleFamily(c.raw)
		if !ok || got != c.want {
			t.Errorf("NormalizeRoleFamily(%q) = %q, %v; want %q (the short match %q would give %q)",
				c.raw, got, ok, c.want, c.shortMatch, c.shortMatchTo)
		}
	}
}

func TestSynonymTableCoversRealJobTitleVocabulary(t *testing.T) {
	cases := map[string]string{
		"SWE":                          RoleFamilyEngineering,
		"SDE II":                       RoleFamilyEngineering,
		"Back-End Engineer":            RoleFamilyEngineering,
		"Front End Developer":          RoleFamilyEngineering,
		"Full-Stack Engineer":          RoleFamilyEngineering,
		"Infrastructure Engineer":      RoleFamilyEngineering,
		"Site Reliability Engineer":    RoleFamilyEngineering,
		"DevOps Engineer":              RoleFamilyEngineering,
		"Staff Platform Engineer":      RoleFamilyEngineering,
		"Application Security Analyst": RoleFamilyEngineering,
		"AppSec Engineer":              RoleFamilyEngineering,
		"iOS Engineer":                 RoleFamilyEngineering,
		"Android Developer":            RoleFamilyEngineering,
		"Embedded Software Engineer":   RoleFamilyEngineering,
		"MLE":                          RoleFamilyAI,
		"Machine Learning Engineer":    RoleFamilyAI,
		"Deep Learning Engineer":       RoleFamilyAI,
		"Applied Scientist":            RoleFamilyAI,
		"LLM Engineer":                 RoleFamilyAI,
		"NLP Engineer":                 RoleFamilyAI,
		"Computer Vision Engineer":     RoleFamilyAI,
		"MLOps Engineer":               RoleFamilyAI,
		"Analytics Engineer":           RoleFamilyData,
		"Data Scientist":               RoleFamilyData,
		"BI Developer":                 RoleFamilyData,
		"ETL Developer":                RoleFamilyData,
		"PM":                           RoleFamilyProduct,
		"Group Product Manager":        RoleFamilyProduct,
		"TPM":                          RoleFamilyProduct,
		"Product Owner":                RoleFamilyProduct,
		"UI/UX Designer":               RoleFamilyDesign,
		"Product Designer":             RoleFamilyDesign,
		"Design Researcher":            RoleFamilyDesign,
		"Brand Designer":               RoleFamilyDesign,
		"EM":                           RoleFamilyLeadership,
		"VP Engineering":               RoleFamilyLeadership,
		"Head of Engineering":          RoleFamilyLeadership,
		"CTO":                          RoleFamilyLeadership,
		"Tech Lead Manager":            RoleFamilyLeadership,
	}
	for raw, want := range cases {
		got, ok := NormalizeRoleFamily(raw)
		if !ok || got != want {
			t.Errorf("NormalizeRoleFamily(%q) = %q, %v; want %q", raw, got, ok, want)
		}
	}
}

// The synonym table is the whole point of the vocabulary: it is what lets six
// buckets absorb the thousands of titles a job board uses. A table that quietly
// shrank would start rejecting ordinary postings.
func TestSynonymTableIsLargeEnoughToBeUseful(t *testing.T) {
	const minimum = 150
	if len(roleFamilySynonyms) < minimum {
		t.Errorf("synonym table has %d entries, want at least %d", len(roleFamilySynonyms), minimum)
	}
	for phrase, family := range roleFamilySynonyms {
		if normalizeLabel(phrase) != phrase {
			t.Errorf("synonym key %q is not in normalizeLabel form (%q); it can never be matched",
				phrase, normalizeLabel(phrase))
		}
		if RoleFamilyRank(family) == len(RoleFamilies) {
			t.Errorf("synonym %q maps to %q, which is not a canonical family", phrase, family)
		}
	}
}

// Engineering sits first in the vocabulary, so it wins every tie. That makes a
// redundant Engineering compound actively harmful: adding "platform engineer"
// alongside "platform" creates a two-word Engineering match that ties with — and
// therefore beats — "ml platform", and every ML platform role silently lands in
// Engineering. The rule is that an Engineering key must not be another
// Engineering key with one more word tacked on; span-1 already covers those.
func TestNoEngineeringSynonymIsARedundantExtensionOfAnother(t *testing.T) {
	for phrase, family := range roleFamilySynonyms {
		if family != RoleFamilyEngineering {
			continue
		}
		words := strings.Fields(phrase)
		if len(words) < 2 {
			continue
		}
		prefix := strings.Join(words[:len(words)-1], " ")
		if roleFamilySynonyms[prefix] == RoleFamilyEngineering {
			t.Errorf("synonym %q is %q plus one word, both Engineering: it adds no reach and "+
				"creates a %d-word tie that Engineering wins on rank, burying AI and Data titles",
				phrase, prefix, len(words))
		}
	}
}

func TestUnknownRoleFamilyNamesTheWholeVocabulary(t *testing.T) {
	_, ok := NormalizeRoleFamily("Underwater Basket Weaving")
	if ok {
		t.Fatal("an off-vocabulary label resolved; it must never guess a default")
	}
	err := ErrUnknownRoleFamily("Underwater Basket Weaving")
	if !errors.Is(err, ErrInvalidRoleFamily) {
		t.Errorf("error = %v, want it to wrap ErrInvalidRoleFamily", err)
	}
	for _, name := range RoleFamilyNames() {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not name %q; an agent reading the 400 cannot retry", err, name)
		}
	}
}

func TestNormalizeListingRoleFamilyKeepsTheSpecificLabelAsATag(t *testing.T) {
	l := &Listing{RoleFamily: "MLE"}
	if err := NormalizeListingRoleFamily(l); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if l.RoleFamily != RoleFamilyAI {
		t.Errorf("role family = %q, want %q", l.RoleFamily, RoleFamilyAI)
	}
	if len(l.Tags) != 1 || l.Tags[0] != "mle" {
		t.Errorf("tags = %v, want [mle]", l.Tags)
	}

	// Normalizing again must not add the tag twice.
	l.RoleFamily = "MLE"
	if err := NormalizeListingRoleFamily(l); err != nil {
		t.Fatalf("re-normalize: %v", err)
	}
	if len(l.Tags) != 1 {
		t.Errorf("tags = %v after re-normalizing, want the tag appended only once", l.Tags)
	}
}

func TestNormalizeListingRoleFamilyLeavesTheCanonicalNameOutOfTags(t *testing.T) {
	l := &Listing{RoleFamily: "engineering"}
	if err := NormalizeListingRoleFamily(l); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if l.RoleFamily != RoleFamilyEngineering {
		t.Errorf("role family = %q, want %q", l.RoleFamily, RoleFamilyEngineering)
	}
	if len(l.Tags) != 0 {
		t.Errorf("tags = %v, want none: the raw label was no more specific than the family", l.Tags)
	}
}

func TestEmptyRoleFamilyIsAllowedButNonsenseIsNot(t *testing.T) {
	unsorted := &Listing{RoleFamily: ""}
	if err := NormalizeListingRoleFamily(unsorted); err != nil {
		t.Errorf("an unlabelled listing must be kept, got %v", err)
	}
	if unsorted.RoleFamily != "" {
		t.Errorf("role family = %q, want it left empty for Unsorted", unsorted.RoleFamily)
	}

	nonsense := &Listing{RoleFamily: "Chief Vibes Officer of Nothing"}
	if err := NormalizeListingRoleFamily(nonsense); !errors.Is(err, ErrInvalidRoleFamily) {
		t.Errorf("error = %v, want ErrInvalidRoleFamily", err)
	}
}

func TestNormalizeSourceRoleFamiliesDedupesDropsBlanksAndSortsByRank(t *testing.T) {
	src := &Source{RoleFamilies: []string{"CTO", "", "  ", "backend", "SWE", "MLE", "Engineering"}}
	if err := NormalizeSourceRoleFamilies(src); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	want := []string{RoleFamilyEngineering, RoleFamilyAI, RoleFamilyLeadership}
	if len(src.RoleFamilies) != len(want) {
		t.Fatalf("families = %v, want %v", src.RoleFamilies, want)
	}
	for i := range want {
		if src.RoleFamilies[i] != want[i] {
			t.Fatalf("families = %v, want %v (display order, deduped)", src.RoleFamilies, want)
		}
	}
}

func TestNormalizeSourceRoleFamiliesRejectsNonsense(t *testing.T) {
	src := &Source{RoleFamilies: []string{"Engineering", "Sorcery"}}
	if err := NormalizeSourceRoleFamilies(src); !errors.Is(err, ErrInvalidRoleFamily) {
		t.Errorf("error = %v, want ErrInvalidRoleFamily", err)
	}
}
