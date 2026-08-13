package jobstore

import "encoding/json"

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

// Application is one application against one listing, and it carries two states
// that must never collapse into one field:
//
//   - Stage is where you stand WITH THE EMPLOYER, and the source of truth for the
//     pipeline from the moment an application exists.
//   - AgentStatus is what the AUTOMATION has done. An agent failing to fill a form
//     and a company rejecting you are not the same event.
//
// AgentStatus is the field previously called Status (column `status`, JSON
// `status`). It was renamed because "status" on a record that tracks an employer
// relationship no longer described what it holds.
type Application struct {
	ID int64 `json:"id"`
	// ListingID is job-store's own listing id. It must resolve to a live listing
	// — join on ids, never on a company or title, which collide.
	ListingID int64 `json:"listing_id"`
	// Stage is the employer-facing lifecycle: drafting -> ready -> submitted ->
	// acknowledged -> screen -> interview -> onsite -> offer, plus the terminal
	// rejected, withdrawn and ghosted. Every change to it appends an
	// ApplicationEvent in the same transaction, so a stage can never move without
	// leaving a trace.
	Stage string `json:"stage"`
	// AgentStatus is what the automation has done: draft, ready, submitted, failed.
	AgentStatus      string `json:"agent_status"`
	ResumeDocumentID int64  `json:"resume_document_id"`
	// ResumeBodySHA256 pins the resume's CONTENT at the moment the automation
	// reached submitted. The document id alone answers "which file" and not "which
	// version", so without this an edited resume would quietly claim to be the copy
	// an employer holds. Written once, never rewritten.
	ResumeBodySHA256 string `json:"resume_body_sha256"`
	// ResumeDrifted is derived on every read, never stored: it reports that the
	// resume document's body no longer hashes to the pinned value. Not an error —
	// it is the normal result of improving your resume — but you should know the
	// copy an employer holds is not the copy on your screen.
	ResumeDrifted         bool                `json:"resume_drifted"`
	CoverLetterDocumentID int64               `json:"cover_letter_document_id"`
	CoverLetterBody       string              `json:"cover_letter_body"`
	Answers               []ApplicationAnswer `json:"answers"`
	AgentSessionID        string              `json:"agent_session_id"`
	SubmittedAt           int64               `json:"submitted_at"`
	Error                 string              `json:"error"`
	CreatedAt             int64               `json:"created_at"`
	UpdatedAt             int64               `json:"updated_at"`
	// EmailCount is how many CONFIRMED messages are attached. Proposed links are
	// left out: a matcher's guess is not mail from the employer until a person has
	// said it is.
	EmailCount int `json:"email_count"`
	// TaskCount is how many noteboard todos are linked, which this store knows
	// without asking anyone.
	TaskCount int `json:"task_count"`
	// OpenTaskCount is how many of those todos noteboard currently reports as
	// open. It is a pointer because null has to mean "could not tell" — noteboard
	// unreachable, or nobody asked — and 0 has to keep meaning "none outstanding".
	// The GET routes populate it; a write response leaves it null rather than
	// making every write wait on noteboard.
	OpenTaskCount *int `json:"open_task_count"`
	// LastActivityAt is the newest thing that actually HAPPENED: the latest
	// timeline event or confirmed email. Deliberately not updated_at — an agent
	// retrying a write is not a company writing back, and the days-quiet reading
	// this feeds is about employer silence.
	LastActivityAt int64 `json:"last_activity_at"`
	// Tasks is present only on GET /applications/{id}?expand=tasks. It is read
	// through noteboard at request time and is never a copy of what noteboard
	// holds.
	Tasks *ApplicationTasksExpansion `json:"tasks,omitempty"`
}

// ApplicationEvent is one entry in an application's timeline. Append-only: a
// stage change writes one of these in the same transaction as the change itself,
// because "when did I apply, when did they reply, how long have they been
// silent" is a question a single mutable stage column cannot answer.
//
// StageFrom and StageTo are both empty on an event that records something which
// happened without moving the stage — a note.
type ApplicationEvent struct {
	ID            int64  `json:"id"`
	ApplicationID int64  `json:"application_id"`
	StageFrom     string `json:"stage_from"`
	StageTo       string `json:"stage_to"`
	Note          string `json:"note"`
	Source        string `json:"source"`
	OccurredAt    int64  `json:"occurred_at"`
	CreatedAt     int64  `json:"created_at"`
}

// ApplicationEmail joins an application to one message in mailstack (:8195),
// which owns it. job-store keeps identifiers plus just enough to render a row
// without a round-trip; it never becomes a second copy of your mail.
//
// The join key is (ApplicationID, AccountID, MessageID) — mailstack's own
// per-account id, always present. RFCMessageID is carried because it is the only
// identifier stable across accounts and folders, but it is OPTIONAL per RFC 5322
// §3.6.4 and may be empty, so it is a dedup aid and never the key.
type ApplicationEmail struct {
	ID            int64  `json:"id"`
	ApplicationID int64  `json:"application_id"`
	AccountID     string `json:"account_id"`
	MessageID     string `json:"message_id"`
	RFCMessageID  string `json:"rfc_message_id"`
	ThreadID      string `json:"thread_id"`
	Direction     string `json:"direction"`
	Subject       string `json:"subject"`
	FromAddress   string `json:"from_address"`
	OccurredAt    int64  `json:"occurred_at"`
	LinkedBy      string `json:"linked_by"`
	// Status gates what the link may do. A matcher may only propose; a human
	// confirms it to linked. Only a linked email may drive a stage change —
	// guessing that mail from a company domain belongs to a given application is
	// exactly the guess that files a rejection under the wrong job.
	Status    string `json:"status"`
	CreatedAt int64  `json:"created_at"`
}

// ApplicationTask links an application to a noteboard todo, and stores its id and
// nothing else. noteboard owns the title, body and status; copying any of them
// here would create a second truth that drifts the first time one is edited.
type ApplicationTask struct {
	ID            int64 `json:"id"`
	ApplicationID int64 `json:"application_id"`
	// NoteboardID is the noteboard item uuid, the only reference kept.
	NoteboardID string `json:"noteboard_id"`
	CreatedAt   int64  `json:"created_at"`
}

// ApplicationTasksExpansion is what ?expand=tasks returns: the linked todos as
// noteboard itself serves them.
//
// Error is the whole point of the type. If noteboard cannot be reached, or a
// linked todo cannot be read, that is said out loud — an application silently
// showing zero outstanding tasks is worse than one that admits it cannot tell.
// Items is nil (JSON null) when nothing could be read at all, so "no tasks" and
// "could not tell" can never be confused.
type ApplicationTasksExpansion struct {
	Items []ApplicationTaskExpansion `json:"items"`
	Error string                     `json:"error,omitempty"`
}

// ApplicationTaskExpansion is one linked todo as noteboard serves it. Item is the
// noteboard record passed through unchanged — this layer is transparent, and a
// narrower copy of noteboard's shape here would be a second schema to drift.
type ApplicationTaskExpansion struct {
	ID          int64           `json:"id"`
	NoteboardID string          `json:"noteboard_id"`
	CreatedAt   int64           `json:"created_at"`
	Item        json.RawMessage `json:"item,omitempty"`
	Error       string          `json:"error,omitempty"`
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

// Application agent statuses: what the AUTOMATION has done. This is the field
// once called simply "status", renamed because it never described the employer
// relationship the record now tracks — an agent failing to fill a form and a
// company rejecting you are not the same event.
const (
	ApplicationAgentStatusDraft     = "draft"
	ApplicationAgentStatusReady     = "ready"
	ApplicationAgentStatusSubmitted = "submitted"
	ApplicationAgentStatusFailed    = "failed"
)

// ApplicationAgentStatuses is the whole vocabulary, so an error can name it.
var ApplicationAgentStatuses = []string{
	ApplicationAgentStatusDraft,
	ApplicationAgentStatusReady,
	ApplicationAgentStatusSubmitted,
	ApplicationAgentStatusFailed,
}

// ValidApplicationAgentStatus reports whether an agent status is one this store
// recognizes.
func ValidApplicationAgentStatus(agentStatus string) bool {
	for _, s := range ApplicationAgentStatuses {
		if s == agentStatus {
			return true
		}
	}
	return false
}

// Application stages: where you stand WITH THE EMPLOYER, and the source of truth
// for the pipeline from the moment an application exists.
//
// ghosted is a real stage, not a missing value. Silence is the most common
// outcome in a job search, and a pipeline that can only say "submitted" forever
// cannot tell you what to chase.
const (
	ApplicationStageDrafting     = "drafting"
	ApplicationStageReady        = "ready"
	ApplicationStageSubmitted    = "submitted"
	ApplicationStageAcknowledged = "acknowledged"
	ApplicationStageScreen       = "screen"
	ApplicationStageInterview    = "interview"
	ApplicationStageOnsite       = "onsite"
	ApplicationStageOffer        = "offer"
	ApplicationStageRejected     = "rejected"
	ApplicationStageWithdrawn    = "withdrawn"
	ApplicationStageGhosted      = "ghosted"
)

// ApplicationStages is the whole lifecycle in order, the three terminal stages
// last, so an error message can name it and a UI can build its board from it
// rather than from the values it happens to observe.
var ApplicationStages = []string{
	ApplicationStageDrafting,
	ApplicationStageReady,
	ApplicationStageSubmitted,
	ApplicationStageAcknowledged,
	ApplicationStageScreen,
	ApplicationStageInterview,
	ApplicationStageOnsite,
	ApplicationStageOffer,
	ApplicationStageRejected,
	ApplicationStageWithdrawn,
	ApplicationStageGhosted,
}

// ValidApplicationStage reports whether a stage is one this store recognizes.
func ValidApplicationStage(stage string) bool {
	for _, s := range ApplicationStages {
		if s == stage {
			return true
		}
	}
	return false
}

// Who caused an application event.
const (
	EventSourceUser  = "user"
	EventSourceAgent = "agent"
	EventSourceEmail = "email"
)

// EventSources is the whole vocabulary, so an error can name it.
var EventSources = []string{EventSourceUser, EventSourceAgent, EventSourceEmail}

// ValidEventSource reports whether an event source is one this store recognizes.
func ValidEventSource(source string) bool {
	for _, s := range EventSources {
		if s == source {
			return true
		}
	}
	return false
}

// Who linked an email to an application. A matcher is the only one of the three
// that is guessing, which is why its links arrive proposed.
const (
	EmailLinkedByUser    = "user"
	EmailLinkedByAgent   = "agent"
	EmailLinkedByMatcher = "matcher"
)

// EmailLinkers is the whole vocabulary, so an error can name it.
var EmailLinkers = []string{EmailLinkedByUser, EmailLinkedByAgent, EmailLinkedByMatcher}

// ValidEmailLinker reports whether a linked_by value is one this store recognizes.
func ValidEmailLinker(linkedBy string) bool {
	for _, l := range EmailLinkers {
		if l == linkedBy {
			return true
		}
	}
	return false
}

// Link states for an application email. Same discipline as a scouted source: a
// guess arrives proposed and a human confirms it.
const (
	EmailLinkStatusProposed = "proposed"
	EmailLinkStatusLinked   = "linked"
	EmailLinkStatusRejected = "rejected"
)

// EmailLinkStatuses is the whole vocabulary, so an error can name it.
var EmailLinkStatuses = []string{EmailLinkStatusProposed, EmailLinkStatusLinked, EmailLinkStatusRejected}

// ValidEmailLinkStatus reports whether an email link status is one this store
// recognizes.
func ValidEmailLinkStatus(status string) bool {
	for _, s := range EmailLinkStatuses {
		if s == status {
			return true
		}
	}
	return false
}

// Which way an email went.
const (
	EmailDirectionInbound  = "inbound"
	EmailDirectionOutbound = "outbound"
)

// EmailDirections is the whole vocabulary, so an error can name it.
var EmailDirections = []string{EmailDirectionInbound, EmailDirectionOutbound}

// ValidEmailDirection reports whether a direction is one this store recognizes.
func ValidEmailDirection(direction string) bool {
	for _, d := range EmailDirections {
		if d == direction {
			return true
		}
	}
	return false
}
