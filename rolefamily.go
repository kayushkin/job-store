package jobstore

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// The canonical role-family vocabulary.
//
// A role family answers one question: which kind of job is this, in terms of how
// the reader decides whether to apply. It is deliberately short, so a poller
// cannot invent a new bucket every time it meets a new job title — "Senior
// Backend Engineer II", "Staff SWE, Platform" and "Software Development Engineer
// III" are one family, not three. Anything more specific than these six belongs
// in tags, not here: NormalizeListingRoleFamily keeps the specific word as a tag.
const (
	// RoleFamilyEngineering is software engineering that is not primarily about
	// models or data: backend, frontend, full stack, platform, infrastructure,
	// security, mobile, embedded, QA, tooling.
	RoleFamilyEngineering = "Engineering"
	// RoleFamilyAI is machine learning and applied AI: ML engineering, research
	// engineering, applied science, LLM and agent work, MLOps.
	RoleFamilyAI = "AI"
	// RoleFamilyData is data engineering, analytics, data science and BI.
	RoleFamilyData = "Data"
	// RoleFamilyProduct is product management, program management and TPM work.
	RoleFamilyProduct = "Product"
	// RoleFamilyDesign is product design, UX, UI, design research and brand.
	RoleFamilyDesign = "Design"
	// RoleFamilyLeadership is people and org leadership: engineering manager,
	// director, VP, head of, C-level.
	RoleFamilyLeadership = "Leadership"
)

// RoleFamilyDefinition describes one family for consumers that render the
// vocabulary (the dash filter bar) or write against it (the poller prompt).
type RoleFamilyDefinition struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// RoleFamilies is the whole vocabulary in display order. The order is also the
// tiebreak rank, so a UI listing these must keep it: two equally good matches
// resolve to whichever family comes first here.
var RoleFamilies = []RoleFamilyDefinition{
	{RoleFamilyEngineering, "Backend, frontend, full stack, platform, infrastructure, security, mobile"},
	{RoleFamilyAI, "ML engineering, research engineering, applied AI, LLM and agents"},
	{RoleFamilyData, "Data engineering, analytics, data science, BI"},
	{RoleFamilyProduct, "Product management, program management, TPM"},
	{RoleFamilyDesign, "Product design, UX, UI, design research, brand"},
	{RoleFamilyLeadership, "Engineering manager, director, VP, head of, CTO"},
}

// RoleFamilyNames returns just the names, in display order.
func RoleFamilyNames() []string {
	out := make([]string, 0, len(RoleFamilies))
	for _, f := range RoleFamilies {
		out = append(out, f.Name)
	}
	return out
}

// roleFamilyRank gives each canonical name its display position, so callers can
// sort by family without re-deriving the order.
var roleFamilyRank = func() map[string]int {
	m := make(map[string]int, len(RoleFamilies))
	for i, f := range RoleFamilies {
		m[f.Name] = i
	}
	return m
}()

// RoleFamilyRank is the display position of a canonical family name; an unknown
// name sorts last.
func RoleFamilyRank(name string) int {
	if r, ok := roleFamilyRank[name]; ok {
		return r
	}
	return len(RoleFamilies)
}

// roleFamilySynonyms maps the job-title vocabulary a poller naturally meets onto
// the canonical six. Keys are normalized by normalizeLabel (lowercased,
// punctuation collapsed to single spaces), so "Back-End", "back end" and
// "BACK END" all hit the "back end" key.
//
// Multi-word keys exist so a longer phrase can beat a shorter one that is also
// present: "engineering manager" is Leadership even though a bare "engineering"
// is Engineering, and "data platform" is Data even though a bare "platform" is
// Engineering. That is the whole point of the longest-span scan below — a title
// is resolved by its most specific phrase, not by whichever word appears first.
// A rule worth stating because breaking it silently miscategorizes half the
// board: an Engineering entry is a BARE specialism ("backend", "platform",
// "infrastructure"), not that specialism with "engineer" glued on. "Backend
// Engineer" already resolves through the two one-word matches, and adding
// "backend engineer" as a two-word key only creates a two-word tie that
// Engineering wins on rank — which is how "ML Platform Engineer" and "AI
// Infrastructure Engineer" both came out as plain Engineering. The compound keys
// that survive here are the ones whose first word is NOT itself an Engineering
// term ("test engineer", "sales engineer"), so they add reach instead of noise.
var roleFamilySynonyms = map[string]string{
	// --- Engineering ---
	"engineering":                   RoleFamilyEngineering,
	"engineer":                      RoleFamilyEngineering,
	"software":                      RoleFamilyEngineering,
	"software development engineer": RoleFamilyEngineering,
	"swe":                           RoleFamilyEngineering,
	"sde":                           RoleFamilyEngineering,
	"sdet":                          RoleFamilyEngineering,
	"developer":                     RoleFamilyEngineering,
	"dev":                           RoleFamilyEngineering,
	"programmer":                    RoleFamilyEngineering,
	"coder":                         RoleFamilyEngineering,
	"backend":                       RoleFamilyEngineering,
	"back end":                      RoleFamilyEngineering,
	"server side":                   RoleFamilyEngineering,
	"frontend":                      RoleFamilyEngineering,
	"front end":                     RoleFamilyEngineering,
	"ui engineer":                   RoleFamilyEngineering,
	"web developer":                 RoleFamilyEngineering,
	"web development":               RoleFamilyEngineering,
	"full stack":                    RoleFamilyEngineering,
	"fullstack":                     RoleFamilyEngineering,
	"platform":                      RoleFamilyEngineering,
	"infra":                         RoleFamilyEngineering,
	"infrastructure":                RoleFamilyEngineering,
	"sre":                           RoleFamilyEngineering,
	"site reliability":              RoleFamilyEngineering,
	"reliability engineer":          RoleFamilyEngineering,
	"devops":                        RoleFamilyEngineering,
	"cloud":                         RoleFamilyEngineering,
	"systems":                       RoleFamilyEngineering,
	"distributed systems":           RoleFamilyEngineering,
	"kernel":                        RoleFamilyEngineering,
	"security":                      RoleFamilyEngineering,
	"appsec":                        RoleFamilyEngineering,
	"application security":          RoleFamilyEngineering,
	"product security":              RoleFamilyEngineering,
	"infosec":                       RoleFamilyEngineering,
	"cybersecurity":                 RoleFamilyEngineering,
	"offensive security":            RoleFamilyEngineering,
	"penetration tester":            RoleFamilyEngineering,
	"pentester":                     RoleFamilyEngineering,
	"red team":                      RoleFamilyEngineering,
	"cryptography":                  RoleFamilyEngineering,
	"mobile":                        RoleFamilyEngineering,
	"ios":                           RoleFamilyEngineering,
	"android":                       RoleFamilyEngineering,
	"react native":                  RoleFamilyEngineering,
	"flutter":                       RoleFamilyEngineering,
	"embedded":                      RoleFamilyEngineering,
	"firmware":                      RoleFamilyEngineering,
	"hardware engineer":             RoleFamilyEngineering,
	"robotics":                      RoleFamilyEngineering,
	"qa":                            RoleFamilyEngineering,
	"quality assurance":             RoleFamilyEngineering,
	"test engineer":                 RoleFamilyEngineering,
	"automation engineer":           RoleFamilyEngineering,
	"release engineer":              RoleFamilyEngineering,
	"build engineer":                RoleFamilyEngineering,
	"compiler engineer":             RoleFamilyEngineering,
	"graphics engineer":             RoleFamilyEngineering,
	"game developer":                RoleFamilyEngineering,
	"game engineer":                 RoleFamilyEngineering,
	"solutions engineer":            RoleFamilyEngineering,
	"sales engineer":                RoleFamilyEngineering,
	"support engineer":              RoleFamilyEngineering,
	"integration engineer":          RoleFamilyEngineering,
	"api engineer":                  RoleFamilyEngineering,
	"network engineer":              RoleFamilyEngineering,
	"performance engineer":          RoleFamilyEngineering,
	"tools engineer":                RoleFamilyEngineering,
	"tooling":                       RoleFamilyEngineering,
	"devex":                         RoleFamilyEngineering,
	"devrel":                        RoleFamilyEngineering,
	"solutions architect":           RoleFamilyEngineering,

	// --- AI ---
	"ai":                          RoleFamilyAI,
	"artificial intelligence":     RoleFamilyAI,
	"ai engineer":                 RoleFamilyAI,
	"ml":                          RoleFamilyAI,
	"mle":                         RoleFamilyAI,
	"machine learning":            RoleFamilyAI,
	"machine learning engineer":   RoleFamilyAI,
	"ml engineer":                 RoleFamilyAI,
	"machine learning scientist":  RoleFamilyAI,
	"deep learning":               RoleFamilyAI,
	"deep learning engineer":      RoleFamilyAI,
	"research engineer":           RoleFamilyAI,
	"research engineering":        RoleFamilyAI,
	"research scientist":          RoleFamilyAI,
	"applied scientist":           RoleFamilyAI,
	"applied science":             RoleFamilyAI,
	"applied ai":                  RoleFamilyAI,
	"applied research":            RoleFamilyAI,
	"llm":                         RoleFamilyAI,
	"llm engineer":                RoleFamilyAI,
	"large language model":        RoleFamilyAI,
	"large language models":       RoleFamilyAI,
	"genai":                       RoleFamilyAI,
	"genai engineer":              RoleFamilyAI,
	"generative ai":               RoleFamilyAI,
	"foundation models":           RoleFamilyAI,
	"model training":              RoleFamilyAI,
	"pretraining":                 RoleFamilyAI,
	"post training":               RoleFamilyAI,
	"nlp":                         RoleFamilyAI,
	"nlp engineer":                RoleFamilyAI,
	"natural language processing": RoleFamilyAI,
	"computer vision":             RoleFamilyAI,
	"speech recognition":          RoleFamilyAI,
	"mlops":                       RoleFamilyAI,
	"mlops engineer":              RoleFamilyAI,
	"ml infrastructure":           RoleFamilyAI,
	"ml platform":                 RoleFamilyAI,
	"ai platform":                 RoleFamilyAI,
	"ai infrastructure":           RoleFamilyAI,
	"ai researcher":               RoleFamilyAI,
	"ai research":                 RoleFamilyAI,
	"ml researcher":               RoleFamilyAI,
	"prompt engineer":             RoleFamilyAI,
	"prompt engineering":          RoleFamilyAI,
	"agent engineer":              RoleFamilyAI,
	"agents":                      RoleFamilyAI,
	"ai safety":                   RoleFamilyAI,
	"alignment":                   RoleFamilyAI,
	"reinforcement learning":      RoleFamilyAI,
	"recommendation systems":      RoleFamilyAI,
	"search relevance":            RoleFamilyAI,
	"ranking engineer":            RoleFamilyAI,
	"perception engineer":         RoleFamilyAI,
	"autonomy engineer":           RoleFamilyAI,

	// --- Data ---
	"data":                   RoleFamilyData,
	"data engineer":          RoleFamilyData,
	"data engineering":       RoleFamilyData,
	"analytics":              RoleFamilyData,
	"analytics engineer":     RoleFamilyData,
	"analytics engineering":  RoleFamilyData,
	"analyst":                RoleFamilyData,
	"data analyst":           RoleFamilyData,
	"data scientist":         RoleFamilyData,
	"data science":           RoleFamilyData,
	"bi":                     RoleFamilyData,
	"business intelligence":  RoleFamilyData,
	"bi developer":           RoleFamilyData,
	"bi analyst":             RoleFamilyData,
	"etl":                    RoleFamilyData,
	"etl developer":          RoleFamilyData,
	"etl engineer":           RoleFamilyData,
	"bi engineer":            RoleFamilyData,
	"data warehouse":         RoleFamilyData,
	"data warehousing":       RoleFamilyData,
	"data platform":          RoleFamilyData,
	"data infrastructure":    RoleFamilyData,
	"data architect":         RoleFamilyData,
	"database administrator": RoleFamilyData,
	"dba":                    RoleFamilyData,
	"big data":               RoleFamilyData,
	"spark engineer":         RoleFamilyData,
	"dbt":                    RoleFamilyData,
	"reporting analyst":      RoleFamilyData,
	"quantitative analyst":   RoleFamilyData,
	"quant":                  RoleFamilyData,
	"statistician":           RoleFamilyData,
	"decision scientist":     RoleFamilyData,
	"product analyst":        RoleFamilyData,
	"growth analyst":         RoleFamilyData,
	"marketing analyst":      RoleFamilyData,
	"data modeling":          RoleFamilyData,
	"data governance":        RoleFamilyData,
	"data quality":           RoleFamilyData,
	"streaming data":         RoleFamilyData,
	"pipeline engineer":      RoleFamilyData,

	// --- Product ---
	"product":                      RoleFamilyProduct,
	"pm":                           RoleFamilyProduct,
	"product manager":              RoleFamilyProduct,
	"product management":           RoleFamilyProduct,
	"technical product manager":    RoleFamilyProduct,
	"tpm":                          RoleFamilyProduct,
	"senior product manager":       RoleFamilyProduct,
	"group product manager":        RoleFamilyProduct,
	"gpm":                          RoleFamilyProduct,
	"principal product manager":    RoleFamilyProduct,
	"associate product manager":    RoleFamilyProduct,
	"apm":                          RoleFamilyProduct,
	"product owner":                RoleFamilyProduct,
	"program manager":              RoleFamilyProduct,
	"program management":           RoleFamilyProduct,
	"technical program manager":    RoleFamilyProduct,
	"project manager":              RoleFamilyProduct,
	"project management":           RoleFamilyProduct,
	"product operations":           RoleFamilyProduct,
	"product ops":                  RoleFamilyProduct,
	"growth product manager":       RoleFamilyProduct,
	"platform product manager":     RoleFamilyProduct,
	"ai product manager":           RoleFamilyProduct,
	"data product manager":         RoleFamilyProduct,
	"product marketing":            RoleFamilyProduct,
	"product marketing manager":    RoleFamilyProduct,
	"pmm":                          RoleFamilyProduct,
	"business analyst":             RoleFamilyProduct,
	"scrum master":                 RoleFamilyProduct,
	"agile coach":                  RoleFamilyProduct,
	"delivery manager":             RoleFamilyProduct,
	"product strategy":             RoleFamilyProduct,
	"technical program management": RoleFamilyProduct,

	// --- Design ---
	"design":                   RoleFamilyDesign,
	"designer":                 RoleFamilyDesign,
	"product design":           RoleFamilyDesign,
	"product designer":         RoleFamilyDesign,
	"ux":                       RoleFamilyDesign,
	"ux design":                RoleFamilyDesign,
	"ux designer":              RoleFamilyDesign,
	"ui":                       RoleFamilyDesign,
	"ui design":                RoleFamilyDesign,
	"ui designer":              RoleFamilyDesign,
	"ui ux":                    RoleFamilyDesign,
	"ux ui":                    RoleFamilyDesign,
	"user experience":          RoleFamilyDesign,
	"user experience designer": RoleFamilyDesign,
	"user interface":           RoleFamilyDesign,
	"interaction design":       RoleFamilyDesign,
	"interaction designer":     RoleFamilyDesign,
	"visual design":            RoleFamilyDesign,
	"visual designer":          RoleFamilyDesign,
	"graphic design":           RoleFamilyDesign,
	"graphic designer":         RoleFamilyDesign,
	"design research":          RoleFamilyDesign,
	"design researcher":        RoleFamilyDesign,
	"ux research":              RoleFamilyDesign,
	"ux researcher":            RoleFamilyDesign,
	"user research":            RoleFamilyDesign,
	"user researcher":          RoleFamilyDesign,
	"brand":                    RoleFamilyDesign,
	"brand design":             RoleFamilyDesign,
	"brand designer":           RoleFamilyDesign,
	"brand identity":           RoleFamilyDesign,
	"motion design":            RoleFamilyDesign,
	"motion designer":          RoleFamilyDesign,
	"content design":           RoleFamilyDesign,
	"content designer":         RoleFamilyDesign,
	"ux writer":                RoleFamilyDesign,
	"ux writing":               RoleFamilyDesign,
	"design system":            RoleFamilyDesign,
	"design systems":           RoleFamilyDesign,
	"design ops":               RoleFamilyDesign,
	"designops":                RoleFamilyDesign,
	"creative director":        RoleFamilyDesign,
	"art director":             RoleFamilyDesign,
	"industrial design":        RoleFamilyDesign,
	"service design":           RoleFamilyDesign,
	"web design":               RoleFamilyDesign,
	"web designer":             RoleFamilyDesign,
	"illustrator":              RoleFamilyDesign,
	"prototyper":               RoleFamilyDesign,

	// --- Leadership ---
	"leadership":                    RoleFamilyLeadership,
	"manager":                       RoleFamilyLeadership,
	"engineering manager":           RoleFamilyLeadership,
	"em":                            RoleFamilyLeadership,
	"senior engineering manager":    RoleFamilyLeadership,
	"software engineering manager":  RoleFamilyLeadership,
	"engineering management":        RoleFamilyLeadership,
	"development manager":           RoleFamilyLeadership,
	"tech lead manager":             RoleFamilyLeadership,
	"tlm":                           RoleFamilyLeadership,
	"people manager":                RoleFamilyLeadership,
	"director":                      RoleFamilyLeadership,
	"director of engineering":       RoleFamilyLeadership,
	"engineering director":          RoleFamilyLeadership,
	"director of product":           RoleFamilyLeadership,
	"director of design":            RoleFamilyLeadership,
	"director of data":              RoleFamilyLeadership,
	"senior director":               RoleFamilyLeadership,
	"vp":                            RoleFamilyLeadership,
	"vp engineering":                RoleFamilyLeadership,
	"vp of engineering":             RoleFamilyLeadership,
	"vice president of engineering": RoleFamilyLeadership,
	"vp product":                    RoleFamilyLeadership,
	"vp of product":                 RoleFamilyLeadership,
	"svp":                           RoleFamilyLeadership,
	"evp":                           RoleFamilyLeadership,
	"head of":                       RoleFamilyLeadership,
	"head of engineering":           RoleFamilyLeadership,
	"head of product":               RoleFamilyLeadership,
	"head of design":                RoleFamilyLeadership,
	"head of data":                  RoleFamilyLeadership,
	"head of ai":                    RoleFamilyLeadership,
	"cto":                           RoleFamilyLeadership,
	"chief technology officer":      RoleFamilyLeadership,
	"chief technical officer":       RoleFamilyLeadership,
	"cpo":                           RoleFamilyLeadership,
	"chief product officer":         RoleFamilyLeadership,
	"ceo":                           RoleFamilyLeadership,
	"coo":                           RoleFamilyLeadership,
	"cio":                           RoleFamilyLeadership,
	"ciso":                          RoleFamilyLeadership,
	"chief architect":               RoleFamilyLeadership,
	"executive":                     RoleFamilyLeadership,
	"general manager":               RoleFamilyLeadership,
	"site lead":                     RoleFamilyLeadership,
	"org lead":                      RoleFamilyLeadership,
}

var labelPunctuation = regexp.MustCompile(`[^a-z0-9]+`)

// normalizeLabel lowercases a freeform label and collapses everything that is not
// a letter or digit into single spaces, so "Back-End!" and "back end" agree.
func normalizeLabel(s string) string {
	return strings.TrimSpace(labelPunctuation.ReplaceAllString(strings.ToLower(s), " "))
}

// ErrInvalidRoleFamily marks every off-vocabulary role-family error, so the HTTP
// layer can answer 400 (the caller sent a bad value) instead of 500.
var ErrInvalidRoleFamily = errors.New("invalid role family")

// ErrUnknownRoleFamily reports a label that is not in the vocabulary and has no
// synonym. It names the whole vocabulary so an agent reading the HTTP 400 can
// retry with a valid value instead of guessing again.
func ErrUnknownRoleFamily(raw string) error {
	return fmt.Errorf("%w %q: use one of %s (put anything more specific in tags)",
		ErrInvalidRoleFamily, raw, strings.Join(RoleFamilyNames(), ", "))
}

// NormalizeRoleFamily resolves a freeform job label to its canonical family. It
// matches the canonical names themselves first, then scans the synonym table for
// phrases inside a multi-word label ("Staff Backend Engineer" -> Engineering).
//
// Two rules settle a label that could go more than one way:
//   - Longer phrases beat shorter ones, so "Engineering Manager" is Leadership
//     even though a bare "engineering" is Engineering, and "data platform" is
//     Data even though a bare "platform" is Engineering. A job title's meaning
//     lives in its most specific phrase.
//   - Among equally long matches, the family earliest in the vocabulary wins.
//
// It reports whether the label resolved at all; it never guesses a default,
// because a silently wrong bucket is worse than a visible rejection.
func NormalizeRoleFamily(raw string) (string, bool) {
	label := normalizeLabel(raw)
	if label == "" {
		return "", false
	}
	for _, f := range RoleFamilies {
		if label == strings.ToLower(f.Name) {
			return f.Name, true
		}
	}
	words := strings.Fields(label)
	for span := len(words); span >= 1; span-- {
		best := ""
		for start := 0; start+span <= len(words); start++ {
			phrase := strings.Join(words[start:start+span], " ")
			canonical, ok := roleFamilySynonyms[phrase]
			if !ok {
				continue
			}
			if best == "" || RoleFamilyRank(canonical) < RoleFamilyRank(best) {
				best = canonical
			}
		}
		if best != "" {
			return best, true
		}
	}
	return "", false
}

// NormalizeListingRoleFamily canonicalizes a listing's role family in place. When
// the original label was more specific than the canonical name ("MLE", "backend",
// "ux researcher"), it is kept as a tag, so collapsing to six families loses no
// information and the specific word stays searchable.
//
// An empty family is left empty rather than rejected: a real posting that arrives
// unlabelled is still worth keeping, and the UI collects these under "Unsorted"
// where they can be sorted by hand. A non-empty label that means nothing is a
// different thing — that is a mistake, and it is rejected.
func NormalizeListingRoleFamily(l *Listing) error {
	if l.RoleFamily == "" {
		return nil
	}
	canonical, ok := NormalizeRoleFamily(l.RoleFamily)
	if !ok {
		return ErrUnknownRoleFamily(l.RoleFamily)
	}
	if specific := normalizeLabel(l.RoleFamily); specific != strings.ToLower(canonical) {
		l.Tags = appendTagOnce(l.Tags, specific)
	}
	l.RoleFamily = canonical
	return nil
}

// NormalizeSourceRoleFamilies canonicalizes a source's interest list in place,
// dropping blanks and duplicates and keeping vocabulary display order, so two
// sources with the same interests read identically.
func NormalizeSourceRoleFamilies(src *Source) error {
	if len(src.RoleFamilies) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(src.RoleFamilies))
	out := make([]string, 0, len(src.RoleFamilies))
	for _, raw := range src.RoleFamilies {
		if normalizeLabel(raw) == "" {
			continue
		}
		canonical, ok := NormalizeRoleFamily(raw)
		if !ok {
			return ErrUnknownRoleFamily(raw)
		}
		if !seen[canonical] {
			seen[canonical] = true
			out = append(out, canonical)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return RoleFamilyRank(out[i]) < RoleFamilyRank(out[j]) })
	src.RoleFamilies = out
	return nil
}

// appendTagOnce adds a tag unless it is already present (case-insensitively).
func appendTagOnce(tags []string, tag string) []string {
	for _, t := range tags {
		if strings.EqualFold(t, tag) {
			return tags
		}
	}
	return append(tags, tag)
}
