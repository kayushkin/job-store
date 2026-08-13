package jobstore

// Listing is one job posting a source turned up. It moves through the pipeline
// statuses as the user and, later, an application agent act on it.
type Listing struct {
	ID             int64    `json:"id"`
	Sig            string   `json:"sig"`
	Title          string   `json:"title"`
	Company        string   `json:"company"`
	Location       string   `json:"location"`
	Remote         bool     `json:"remote"`
	EmploymentType string   `json:"employment_type"`
	Seniority      string   `json:"seniority"`
	RoleFamily     string   `json:"role_family"`
	Tags           []string `json:"tags"`
	// Description is the COMPLETE text of the posting, fetched from the posting
	// page itself — not a board's summary blurb. Everything downstream depends on
	// it: tailoring needs the real requirements, relevance scoring is meaningless
	// without them, and an application agent filling a form has nothing else.
	Description string `json:"description"`
	// DescriptionFetchedAt is when a non-empty description was last written. It
	// separates "nobody has fetched the posting body yet" from "the body really
	// is this short", which a length alone cannot tell you.
	DescriptionFetchedAt int64 `json:"description_fetched_at"`
	// DescriptionThin is derived, never stored: it reports that this listing's
	// description is too short to tailor a resume or score relevance against. It
	// is computed on every read from the description itself, so it can never
	// disagree with the column it describes.
	DescriptionThin bool   `json:"description_thin"`
	CompMin         int64  `json:"comp_min"`
	CompMax         int64  `json:"comp_max"`
	CompCurrency    string `json:"comp_currency"`
	// CompRaw is the compensation exactly as posted ("$180k–$240k + equity").
	// It is kept alongside the parsed numbers because the parse is lossy and the
	// posting's own words are what a person actually wants to read.
	CompRaw    string `json:"comp_raw"`
	URL        string `json:"url"`
	ApplyURL   string `json:"apply_url"`
	SourceID   int64  `json:"source_id"`
	SourceName string `json:"source_name"`
	Status     string `json:"status"`
	// Relevance is the discovering poller's own 0-100 score.
	Relevance int `json:"relevance"`
	// Notes are the user's, never a poller's — an upsert never writes here.
	Notes        string `json:"notes"`
	PostedAt     int64  `json:"posted_at"`
	ClosesAt     int64  `json:"closes_at"`
	DiscoveredAt int64  `json:"discovered_at"`
	UpdatedAt    int64  `json:"updated_at"`
	// DeletedAt is 0 for a live listing, otherwise when it was soft-deleted. A
	// deleted row keeps its sig, so a re-poll that rediscovers the same posting
	// dedupes against it instead of proposing it all over again.
	DeletedAt int64 `json:"deleted_at"`
}

// Source is a place to look for jobs, carrying its own cadence and effort tier.
//   - kind "board": fetch target and extract the openings listed on it. Cheapest
//     tier, shortest cadence. One company, or one aggregator page.
//   - kind "research": a few focused searches for target roles in location,
//     filtered by role families, seniorities and keywords.
//   - kind "scout": posts NO listings. It reads every source, whatever its status,
//     and proposes new ones for review.
//
// The service never branches on kind: kind plus cadence_hours plus model is the
// whole effort-tier system, and the dispatch semantics live in the poller prompt.
type Source struct {
	ID           int64    `json:"id"`
	Name         string   `json:"name"`
	Kind         string   `json:"kind"`
	Target       string   `json:"target"`
	Company      string   `json:"company"`
	Location     string   `json:"location"`
	RoleFamilies []string `json:"role_families"`
	Seniorities  []string `json:"seniorities"`
	Keywords     []string `json:"keywords"`
	CadenceHours int      `json:"cadence_hours"`
	Model        string   `json:"model"`
	Status       string   `json:"status"`
	Enabled      bool     `json:"enabled"`
	LastRunAt    int64    `json:"last_run_at"`
	NextRunAt    int64    `json:"next_run_at"`
	Notes        string   `json:"notes"`
	ProposedBy   string   `json:"proposed_by"`
	CreatedAt    int64    `json:"created_at"`
	UpdatedAt    int64    `json:"updated_at"`
}

// Document is a resume or cover-letter variant, and it is two things at once:
// the original bytes exactly as uploaded, and the extracted text an agent can
// read and tailor. Both are kept — extraction is added alongside the original,
// never instead of it, because the file an employer receives should be the one
// the user made and not a markdown round-trip of it.
type Document struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Format string `json:"format"`
	// Body is the editable text. For an uploaded pdf or docx it is the extracted
	// text, which is a reading copy — the original stays in the blob.
	Body string `json:"body"`
	// SourceFilename is the name the file arrived under, for display only.
	SourceFilename string `json:"source_filename"`
	ContentType    string `json:"content_type"`
	// BlobSHA256 addresses the original bytes under <data dir>/blobs/. Empty for
	// a document typed in rather than uploaded, which therefore has no original
	// and no /file to serve.
	BlobSHA256 string `json:"blob_sha256"`
	ByteSize   int64  `json:"byte_size"`
	// ExtractedAt is when Body was derived from the blob.
	ExtractedAt int64 `json:"extracted_at"`
	// IsDefault marks the one variant of this kind reached for when nothing else
	// is chosen. At most one per kind: setting it clears the others, in one
	// transaction, because a default that could be ambiguous is not a default.
	IsDefault bool `json:"is_default"`
	// DerivedFromDocumentID is the document a tailored copy was written from, and
	// ListingID the role it was written for. Together they make every variant
	// traceable without the original ever being overwritten.
	DerivedFromDocumentID int64  `json:"derived_from_document_id"`
	ListingID             int64  `json:"listing_id"`
	Notes                 string `json:"notes"`
	CreatedAt             int64  `json:"created_at"`
	UpdatedAt             int64  `json:"updated_at"`
}

// TailorRequest is a queued ask to customize one document for one role. It is
// queued rather than synchronous: the service stays dumb and a dispatcher script
// runs the agent, the same split as the pollers.
//
// The dispatcher writes its output as a NEW document carrying
// DerivedFromDocumentID and ListingID, and points ResultDocumentID at it. The
// document it started from is never overwritten.
type TailorRequest struct {
	ID               int64  `json:"id"`
	DocumentID       int64  `json:"document_id"`
	ListingID        int64  `json:"listing_id"`
	Instructions     string `json:"instructions"`
	Status           string `json:"status"`
	ResultDocumentID int64  `json:"result_document_id"`
	AgentSessionID   string `json:"agent_session_id"`
	Error            string `json:"error"`
	CreatedAt        int64  `json:"created_at"`
	StartedAt        int64  `json:"started_at"`
	FinishedAt       int64  `json:"finished_at"`
	UpdatedAt        int64  `json:"updated_at"`
}

// Tailor request statuses. The UI shows pending -> running -> ready.
const (
	TailorStatusPending = "pending"
	TailorStatusRunning = "running"
	TailorStatusReady   = "ready"
	TailorStatusFailed  = "failed"
)

// TailorStatuses is the whole vocabulary, so an error can name it.
var TailorStatuses = []string{
	TailorStatusPending,
	TailorStatusRunning,
	TailorStatusReady,
	TailorStatusFailed,
}

// ValidTailorStatus reports whether a status is one this store recognizes.
func ValidTailorStatus(status string) bool {
	for _, s := range TailorStatuses {
		if s == status {
			return true
		}
	}
	return false
}

// ApplicationAnswer is one screening question and the answer given to it.
type ApplicationAnswer struct {
	Question string `json:"question"`
	Answer   string `json:"answer"`
}

// Application is one application against one listing. Phase 3: the table and CRUD
// are real so the pipeline has somewhere to write, but nothing submits one yet.
type Application struct {
	ID int64 `json:"id"`
	// ListingID is job-store's own listing id. It must resolve to a live listing
	// — join on ids, never on a company or title, which collide.
	ListingID             int64               `json:"listing_id"`
	Status                string              `json:"status"`
	ResumeDocumentID      int64               `json:"resume_document_id"`
	CoverLetterDocumentID int64               `json:"cover_letter_document_id"`
	CoverLetterBody       string              `json:"cover_letter_body"`
	Answers               []ApplicationAnswer `json:"answers"`
	AgentSessionID        string              `json:"agent_session_id"`
	SubmittedAt           int64               `json:"submitted_at"`
	Error                 string              `json:"error"`
	CreatedAt             int64               `json:"created_at"`
	UpdatedAt             int64               `json:"updated_at"`
}

// The listing pipeline. candidate -> the user picks -> interested -> an agent
// works it -> applying -> applied -> rejected (the company said no) or offer.
// dismissed is the user saying no; closed is the posting going away.
//
// ListingStatusInterested is the queue an application agent reads.
const (
	ListingStatusCandidate  = "candidate"
	ListingStatusInterested = "interested"
	ListingStatusApplying   = "applying"
	ListingStatusApplied    = "applied"
	ListingStatusRejected   = "rejected"
	ListingStatusOffer      = "offer"
	ListingStatusDismissed  = "dismissed"
	ListingStatusClosed     = "closed"
)

// ListingStatuses is the whole pipeline in order, so an error message can name it
// and a UI can build its filter from it rather than from observed values.
var ListingStatuses = []string{
	ListingStatusCandidate,
	ListingStatusInterested,
	ListingStatusApplying,
	ListingStatusApplied,
	ListingStatusRejected,
	ListingStatusOffer,
	ListingStatusDismissed,
	ListingStatusClosed,
}

// ValidListingStatus reports whether a status is one this store recognizes.
func ValidListingStatus(status string) bool {
	for _, s := range ListingStatuses {
		if s == status {
			return true
		}
	}
	return false
}

// Employment types seen on postings. These are descriptive, not validated: a
// posting is free to say something none of these covers, and rejecting the write
// would lose the listing over a label nobody reads programmatically.
const (
	EmploymentFullTime   = "full-time"
	EmploymentContract   = "contract"
	EmploymentInternship = "internship"
)

// Source kinds. The service never branches on these; the dispatcher does.
const (
	KindBoard    = "board"
	KindResearch = "research"
	KindScout    = "scout"
)

// SourceKinds is the whole vocabulary, so an error can name it.
var SourceKinds = []string{KindBoard, KindResearch, KindScout}

// Valid source statuses. A source a scout found is inert until it is approved:
// nothing fetches it, so an agent cannot grow the radar's running cost — or start
// pulling from a page nobody vetted — on its own.
//
// This is separate from Enabled on purpose. Enabled is a pause switch you reach
// for on a working source; status records whether the source was ever accepted at
// all. Collapsing them would make "I turned this off for now" indistinguishable
// from "I never agreed to this", and a re-poll could not tell which to respect.
const (
	SourceStatusActive   = "active"
	SourceStatusProposed = "proposed"
	SourceStatusRejected = "rejected"
)

// SourceStatuses is the whole vocabulary, so an error can name it.
var SourceStatuses = []string{SourceStatusActive, SourceStatusProposed, SourceStatusRejected}

// ValidSourceKind reports whether a kind is one the dispatcher knows how to run.
func ValidSourceKind(kind string) bool {
	for _, k := range SourceKinds {
		if k == kind {
			return true
		}
	}
	return false
}

// ValidSourceStatus reports whether a status is one this store recognizes.
func ValidSourceStatus(status string) bool {
	for _, s := range SourceStatuses {
		if s == status {
			return true
		}
	}
	return false
}

// Document kinds.
const (
	DocumentKindResume      = "resume"
	DocumentKindCoverLetter = "cover_letter"
)

// DocumentKinds is the whole vocabulary, so an error can name it.
var DocumentKinds = []string{DocumentKindResume, DocumentKindCoverLetter}

// ValidDocumentKind reports whether a document kind is one this store recognizes.
func ValidDocumentKind(kind string) bool {
	for _, k := range DocumentKinds {
		if k == kind {
			return true
		}
	}
	return false
}

// Document body formats. A markdown body is converted server-side by the render
// endpoint; an html body is served through the same wrapper unconverted; text,
// pdf and docx bodies are extracted text and render preformatted, because
// pdftotext -layout keeps the column structure a resume depends on and reflowing
// it would destroy the only layout information there is.
const (
	DocumentFormatMarkdown = "markdown"
	DocumentFormatHTML     = "html"
	DocumentFormatText     = "text"
	DocumentFormatPDF      = "pdf"
	DocumentFormatDOCX     = "docx"
)

// DocumentFormats is the whole vocabulary, so an error can name it.
var DocumentFormats = []string{
	DocumentFormatMarkdown,
	DocumentFormatHTML,
	DocumentFormatText,
	DocumentFormatPDF,
	DocumentFormatDOCX,
}

// DocumentHasOriginalBytes reports whether a format is one whose body is an
// extraction of an uploaded original, so /render must point the reader at /file
// for the true document rather than implying the text is the whole of it.
func DocumentHasOriginalBytes(format string) bool {
	return format == DocumentFormatPDF || format == DocumentFormatDOCX
}

// ValidDocumentFormat reports whether a body format is one this store recognizes.
func ValidDocumentFormat(format string) bool {
	for _, f := range DocumentFormats {
		if f == format {
			return true
		}
	}
	return false
}

// Application statuses.
const (
	ApplicationStatusDraft     = "draft"
	ApplicationStatusReady     = "ready"
	ApplicationStatusSubmitted = "submitted"
	ApplicationStatusFailed    = "failed"
)

// ApplicationStatuses is the whole vocabulary, so an error can name it.
var ApplicationStatuses = []string{
	ApplicationStatusDraft,
	ApplicationStatusReady,
	ApplicationStatusSubmitted,
	ApplicationStatusFailed,
}

// ValidApplicationStatus reports whether a status is one this store recognizes.
func ValidApplicationStatus(status string) bool {
	for _, s := range ApplicationStatuses {
		if s == status {
			return true
		}
	}
	return false
}
