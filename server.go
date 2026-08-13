package jobstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RegisterHandlers wires all routes onto the mux.
func RegisterHandlers(mux *http.ServeMux, s *Store) {
	h := &handler{s: s}
	mux.HandleFunc("GET /health", h.health)
	mux.HandleFunc("GET /role-families", h.listRoleFamilies)

	mux.HandleFunc("GET /listings", h.listListings)
	mux.HandleFunc("POST /listings", h.upsertListing)
	mux.HandleFunc("POST /listings/prune", h.pruneStaleListings)
	mux.HandleFunc("GET /listings/{id}", h.getListing)
	mux.HandleFunc("PATCH /listings/{id}", h.patchListing)
	mux.HandleFunc("DELETE /listings/{id}", h.deleteListing)
	mux.HandleFunc("POST /listings/{id}/restore", h.restoreListing)

	mux.HandleFunc("GET /sources", h.listSources)
	mux.HandleFunc("POST /sources", h.upsertSource)
	mux.HandleFunc("GET /sources/{id}", h.getSource)
	mux.HandleFunc("PATCH /sources/{id}", h.patchSource)
	mux.HandleFunc("POST /sources/{id}/ran", h.markSourceRan)
	mux.HandleFunc("DELETE /sources/{id}", h.deleteSource)

	mux.HandleFunc("GET /documents", h.listDocuments)
	mux.HandleFunc("POST /documents", h.upsertDocument)
	mux.HandleFunc("POST /documents/upload", h.uploadDocument)
	mux.HandleFunc("GET /documents/{id}", h.getDocument)
	mux.HandleFunc("GET /documents/{id}/file", h.getDocumentFile)
	mux.HandleFunc("GET /documents/{id}/render", h.renderDocument)
	mux.HandleFunc("POST /documents/{id}/tailor", h.tailorDocument)
	mux.HandleFunc("PATCH /documents/{id}", h.patchDocument)
	mux.HandleFunc("DELETE /documents/{id}", h.deleteDocument)

	mux.HandleFunc("GET /tailor-requests", h.listTailorRequests)
	mux.HandleFunc("GET /tailor-requests/{id}", h.getTailorRequest)
	mux.HandleFunc("PATCH /tailor-requests/{id}", h.patchTailorRequest)
	mux.HandleFunc("DELETE /tailor-requests/{id}", h.deleteTailorRequest)

	mux.HandleFunc("GET /applications", h.listApplications)
	mux.HandleFunc("POST /applications", h.upsertApplication)
	mux.HandleFunc("GET /applications/{id}", h.getApplication)
	mux.HandleFunc("PATCH /applications/{id}", h.patchApplication)
	mux.HandleFunc("POST /applications/{id}/submit", h.submitApplication)
	mux.HandleFunc("DELETE /applications/{id}", h.deleteApplication)

	mux.HandleFunc("GET /applications/{id}/events", h.listApplicationEvents)
	mux.HandleFunc("POST /applications/{id}/events", h.recordApplicationEvent)

	mux.HandleFunc("GET /applications/{id}/emails", h.listApplicationEmails)
	mux.HandleFunc("POST /applications/{id}/emails", h.linkApplicationEmail)
	mux.HandleFunc("PATCH /application-emails/{id}", h.patchApplicationEmail)
	mux.HandleFunc("DELETE /application-emails/{id}", h.deleteApplicationEmail)
	mux.HandleFunc("POST /application-emails/{id}/stage", h.advanceStageFromEmail)

	mux.HandleFunc("GET /applications/{id}/tasks", h.listApplicationTasks)
	mux.HandleFunc("POST /applications/{id}/tasks", h.linkApplicationTask)
	mux.HandleFunc("POST /applications/{id}/tasks/standard", h.createStandardApplicationTasks)
	mux.HandleFunc("DELETE /application-tasks/{id}", h.deleteApplicationTask)
}

// uploadMemoryLimit is how much of a multipart upload ParseMultipartForm keeps
// in memory before spilling the rest to a temp file. A resume fits comfortably;
// anything larger is written to disk rather than held.
const uploadMemoryLimit int64 = 4 << 20

type handler struct{ s *Store }

func (h *handler) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// listRoleFamilies serves the canonical vocabulary in display order. A UI builds
// its filter from this rather than from the distinct values it happens to see in
// the current listings, so an empty family still appears and a stale one never
// does.
func (h *handler) listRoleFamilies(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"role_families": RoleFamilies})
}

// ---- listings ----

func (h *handler) listListings(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	// A caller may filter with any synonym ("SWE", "MLE"); resolve it to the
	// canonical name the rows actually carry. An unresolvable value is rejected
	// rather than silently matching nothing.
	roleFamily := q.Get("role_family")
	if roleFamily != "" {
		canonical, ok := NormalizeRoleFamily(roleFamily)
		if !ok {
			writeErr(w, http.StatusBadRequest, ErrUnknownRoleFamily(roleFamily).Error())
			return
		}
		roleFamily = canonical
	}
	f := ListingFilter{
		Status:          q.Get("status"),
		Company:         q.Get("company"),
		RoleFamily:      roleFamily,
		SourceID:        atoi64(q.Get("source_id")),
		Seniority:       q.Get("seniority"),
		PostedFrom:      atoi64(q.Get("posted_from")),
		PostedTo:        atoi64(q.Get("posted_to")),
		Query:           q.Get("q"),
		ThinDescription: isTrue(q.Get("thin_description")),
		Limit:           int(atoi64(q.Get("limit"))),
		IncludeDeleted:  isTrue(q.Get("include_deleted")),
	}
	// remote is tri-state: absent means "either", present means filter on it.
	if raw := q.Get("remote"); raw != "" {
		remote := isTrue(raw)
		f.Remote = &remote
	}
	listings, err := h.s.ListListings(f)
	if respondStoreError(w, err) {
		return
	}
	if listings == nil {
		listings = []*Listing{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"listings": listings})
}

func (h *handler) upsertListing(w http.ResponseWriter, r *http.Request) {
	// Accept either epoch ints (posted_at/closes_at) or ISO strings
	// (posted/closes) so a discovering poller can post the dates a page shows
	// without computing Unix time itself.
	var req struct {
		Listing
		PostedISO string `json:"posted"`
		ClosesISO string `json:"closes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	l := req.Listing
	if l.PostedAt == 0 && req.PostedISO != "" {
		ts, err := parseWhen(req.PostedISO)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad posted: "+err.Error())
			return
		}
		l.PostedAt = ts
	}
	if l.ClosesAt == 0 && req.ClosesISO != "" {
		ts, err := parseWhen(req.ClosesISO)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad closes: "+err.Error())
			return
		}
		l.ClosesAt = ts
	}
	got, created, err := h.s.UpsertListing(&l)
	if respondStoreError(w, err) {
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, got)
}

// pruneStaleListings soft-deletes every candidate listing that has closed or gone
// stale, so the candidate list only ever holds postings still worth a decision.
// Body is optional: {"grace_days": 45}.
//
// The schedule lives in the scheduler (:8092), which calls this — the store owns
// the rule for what counts as stale, not for how often to apply it.
func (h *handler) pruneStaleListings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		GraceDays *int `json:"grace_days"`
	}
	// An empty body is the ordinary case for a cron call; anything present must
	// still parse.
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	grace := DefaultPruneGraceDays
	if req.GraceDays != nil {
		if *req.GraceDays < 0 {
			writeErr(w, http.StatusBadRequest, "grace_days must not be negative")
			return
		}
		if *req.GraceDays > 0 {
			grace = *req.GraceDays
		}
	}
	pruned, err := h.s.PruneStaleListings(grace)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pruned": pruned, "grace_days": grace})
}

func (h *handler) getListing(w http.ResponseWriter, r *http.Request) {
	id := atoi64(r.PathValue("id"))
	get := h.s.GetListing
	if isTrue(r.URL.Query().Get("include_deleted")) {
		get = h.s.GetListingIncludingDeleted
	}
	got, err := get(id)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, got)
}

// patchListing is the decision channel: it accepts the fields only a person (or
// an agent acting for one) should set. Everything describing what the posting
// says is rewritten by POST /listings, which is safe to re-run and deliberately
// cannot flip a status.
func (h *handler) patchListing(w http.ResponseWriter, r *http.Request) {
	id := atoi64(r.PathValue("id"))
	var p ListingPatch
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	got, err := h.s.PatchListing(id, p)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, got)
}

// deleteListing soft-deletes by default, so the row keeps its dedupe signature
// and can be restored. ?hard=true destroys it outright.
func (h *handler) deleteListing(w http.ResponseWriter, r *http.Request) {
	id := atoi64(r.PathValue("id"))
	hard := isTrue(r.URL.Query().Get("hard"))
	var err error
	if hard {
		err = h.s.PurgeListing(id)
	} else {
		err = h.s.SoftDeleteListing(id)
	}
	if respondStoreError(w, err) {
		return
	}
	status := "deleted"
	if hard {
		status = "purged"
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": status})
}

func (h *handler) restoreListing(w http.ResponseWriter, r *http.Request) {
	id := atoi64(r.PathValue("id"))
	got, err := h.s.RestoreListing(id)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, got)
}

// ---- sources ----

func (h *handler) listSources(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	// SourceFilter.DueBefore is deliberately not read from the query: the due
	// cutoff is always now for a real caller. A test injects it directly.
	f := SourceFilter{
		Kind:    q.Get("kind"),
		Status:  q.Get("status"),
		OnlyDue: isTrue(q.Get("due")),
	}
	sources, err := h.s.ListSources(f)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if sources == nil {
		sources = []*Source{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": sources})
}

func (h *handler) upsertSource(w http.ResponseWriter, r *http.Request) {
	var src Source
	if err := json.NewDecoder(r.Body).Decode(&src); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	got, created, err := h.s.UpsertSource(&src)
	if respondStoreError(w, err) {
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, got)
}

func (h *handler) getSource(w http.ResponseWriter, r *http.Request) {
	id := atoi64(r.PathValue("id"))
	got, err := h.s.GetSource(id)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, got)
}

// patchSource is the decision channel: it accepts the fields only a person should
// set (status, enabled, cadence, next run, notes). Everything describing WHERE to
// look is rewritten by POST /sources, which is safe to re-run and deliberately
// cannot flip a status.
func (h *handler) patchSource(w http.ResponseWriter, r *http.Request) {
	id := atoi64(r.PathValue("id"))
	var p SourcePatch
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	got, err := h.s.PatchSource(id, p)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, got)
}

func (h *handler) markSourceRan(w http.ResponseWriter, r *http.Request) {
	id := atoi64(r.PathValue("id"))
	got, err := h.s.MarkSourceRan(id)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, got)
}

func (h *handler) deleteSource(w http.ResponseWriter, r *http.Request) {
	id := atoi64(r.PathValue("id"))
	err := h.s.DeleteSource(id)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// ---- documents ----

func (h *handler) listDocuments(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	docs, err := h.s.ListDocuments(DocumentFilter{
		Kind:        q.Get("kind"),
		DerivedFrom: atoi64(q.Get("derived_from")),
		ListingID:   atoi64(q.Get("listing_id")),
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if docs == nil {
		docs = []*Document{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"documents": docs})
}

// uploadDocument takes a real file, stores the original bytes and the text
// extracted from them, and refuses the whole upload if the text cannot be read.
//
// The size cap is enforced twice on purpose: MaxBytesReader stops the body at
// the socket so a huge upload never reaches memory, and the store checks the
// length again so a caller reaching UploadDocument directly gets the same limit.
func (h *handler) uploadDocument(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, MaxUploadBytes)
	if err := r.ParseMultipartForm(uploadMemoryLimit); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf(
			"could not read the upload (limit %d bytes): %v", MaxUploadBytes, err))
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	file, header, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "missing form field \"file\": "+err.Error())
		return
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "could not read the uploaded file: "+err.Error())
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		// Falling back to the filename would be a guess with a real cost: two
		// resumes uploaded as resume.pdf would collide on the unique name and the
		// second would silently overwrite the first.
		writeErr(w, http.StatusBadRequest, "missing form field \"name\": a document needs a name of its own, distinct from the filename")
		return
	}
	got, created, err := h.s.UploadDocument(Upload{
		Name:        name,
		Kind:        r.FormValue("kind"),
		Filename:    header.Filename,
		ContentType: header.Header.Get("Content-Type"),
		Data:        data,
		Notes:       r.FormValue("notes"),
		IsDefault:   isTrue(r.FormValue("is_default")),
	})
	if respondStoreError(w, err) {
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, got)
}

// getDocumentFile serves the original uploaded bytes, so a PDF previews natively
// in an iframe and downloads as the file the user actually made. A document that
// was typed in has no original, and says so with a 404 rather than handing back
// the extracted text under a PDF content type.
func (h *handler) getDocumentFile(w http.ResponseWriter, r *http.Request) {
	id := atoi64(r.PathValue("id"))
	doc, err := h.s.GetDocument(id)
	if respondStoreError(w, err) {
		return
	}
	if doc.BlobSHA256 == "" {
		writeErr(w, http.StatusNotFound,
			"this document was typed in, not uploaded, so it has no original file — read its body instead")
		return
	}
	data, err := h.s.ReadBlob(doc.BlobSHA256)
	if respondStoreError(w, err) {
		return
	}
	contentType := doc.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	filename := doc.SourceFilename
	if filename == "" {
		filename = doc.Name
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Content-Disposition", "inline; filename=\""+strings.ReplaceAll(filename, `"`, "")+"\"")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// tailorDocument queues a request to customize this document for one role, and
// answers 202: the work has been accepted, not done. A dispatcher script runs the
// agent, the same split as the pollers.
func (h *handler) tailorDocument(w http.ResponseWriter, r *http.Request) {
	id := atoi64(r.PathValue("id"))
	var req struct {
		ListingID    int64  `json:"listing_id"`
		Instructions string `json:"instructions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	got, err := h.s.CreateTailorRequest(id, req.ListingID, req.Instructions)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusAccepted, got)
}

func (h *handler) upsertDocument(w http.ResponseWriter, r *http.Request) {
	var d Document
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	got, created, err := h.s.UpsertDocument(&d)
	if respondStoreError(w, err) {
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, got)
}

func (h *handler) getDocument(w http.ResponseWriter, r *http.Request) {
	id := atoi64(r.PathValue("id"))
	got, err := h.s.GetDocument(id)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, got)
}

// renderDocument serves the document body as HTML. The conversion is server-side
// so the preview an application agent reads and the preview the browser shows are
// the same bytes.
func (h *handler) renderDocument(w http.ResponseWriter, r *http.Request) {
	id := atoi64(r.PathValue("id"))
	doc, err := h.s.GetDocument(id)
	if respondStoreError(w, err) {
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, RenderDocument(doc))
}

func (h *handler) patchDocument(w http.ResponseWriter, r *http.Request) {
	id := atoi64(r.PathValue("id"))
	var p DocumentPatch
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	got, err := h.s.PatchDocument(id, p)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, got)
}

func (h *handler) deleteDocument(w http.ResponseWriter, r *http.Request) {
	id := atoi64(r.PathValue("id"))
	err := h.s.DeleteDocument(id)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// ---- tailor requests ----

func (h *handler) listTailorRequests(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	reqs, err := h.s.ListTailorRequests(TailorRequestFilter{
		Status:    q.Get("status"),
		ListingID: atoi64(q.Get("listing_id")),
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if reqs == nil {
		reqs = []*TailorRequest{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tailor_requests": reqs})
}

func (h *handler) getTailorRequest(w http.ResponseWriter, r *http.Request) {
	id := atoi64(r.PathValue("id"))
	got, err := h.s.GetTailorRequest(id)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, got)
}

// patchTailorRequest is the dispatcher's write channel: it moves a request
// through pending -> running -> ready and points it at the document it produced.
func (h *handler) patchTailorRequest(w http.ResponseWriter, r *http.Request) {
	id := atoi64(r.PathValue("id"))
	var p TailorRequestPatch
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	got, err := h.s.PatchTailorRequest(id, p)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, got)
}

func (h *handler) deleteTailorRequest(w http.ResponseWriter, r *http.Request) {
	id := atoi64(r.PathValue("id"))
	err := h.s.DeleteTailorRequest(id)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// ---- applications ----

// listApplications filters on the two states separately. `status` is refused
// rather than ignored: it named the field now called agent_status, and a filter
// silently dropped would answer with every application instead of the ones asked
// for — which reads as "nothing matched that" only after you count them.
func (h *handler) listApplications(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Has("status") {
		writeErr(w, http.StatusBadRequest, ErrStatusRenamed().Error())
		return
	}
	apps, err := h.s.ListApplications(ApplicationFilter{
		Stage:       q.Get("stage"),
		AgentStatus: q.Get("agent_status"),
		ListingID:   atoi64(q.Get("listing_id")),
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if apps == nil {
		apps = []*Application{}
	}
	// One noteboard read for the whole page, so a board can render "3 of 5 done"
	// without a request per row. A noteboard that is down leaves every
	// open_task_count null — "could not tell", never 0 — and the applications
	// themselves are still served.
	if err := h.s.CountOpenApplicationTasks(apps); err != nil {
		log.Printf("[job-store] open task counts unavailable: %v", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"applications": apps})
}

func (h *handler) upsertApplication(w http.ResponseWriter, r *http.Request) {
	// The body is decoded twice on purpose: once to catch a caller still sending
	// the old `status`, which Application no longer has a field for and would
	// therefore drop on the floor, and once for the record itself.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "could not read the body: "+err.Error())
		return
	}
	var legacy struct {
		Status *string `json:"status"`
	}
	if err := json.Unmarshal(body, &legacy); err == nil && legacy.Status != nil {
		writeErr(w, http.StatusBadRequest, ErrStatusRenamed().Error())
		return
	}
	var a Application
	if err := json.Unmarshal(body, &a); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	got, created, err := h.s.UpsertApplication(&a)
	if respondStoreError(w, err) {
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, got)
}

// getApplication serves one application, and with ?expand=tasks also reads its
// linked todos through noteboard at request time.
//
// A failed expansion is reported in the field itself rather than swallowed: the
// application is still worth serving, but the task list must never come back as
// an empty array when the truth is that nothing could be read.
func (h *handler) getApplication(w http.ResponseWriter, r *http.Request) {
	id := atoi64(r.PathValue("id"))
	got, err := h.s.GetApplication(id)
	if respondStoreError(w, err) {
		return
	}
	if err := h.s.CountOpenApplicationTasks([]*Application{got}); err != nil {
		log.Printf("[job-store] open task count unavailable for application %d: %v", id, err)
	}
	for _, expand := range r.URL.Query()["expand"] {
		for _, field := range strings.Split(expand, ",") {
			if strings.TrimSpace(field) != "tasks" {
				continue
			}
			tasks, err := h.s.ExpandApplicationTasks(id)
			if respondStoreError(w, err) {
				return
			}
			got.Tasks = tasks
		}
	}
	writeJSON(w, http.StatusOK, got)
}

func (h *handler) patchApplication(w http.ResponseWriter, r *http.Request) {
	id := atoi64(r.PathValue("id"))
	var p ApplicationPatch
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	got, err := h.s.PatchApplication(id, p)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, got)
}

// submitApplication is phase 3 and says so. Answering 501 with the list of what
// is missing is the honest response: a 200 that stored a status of "submitted"
// would be the service claiming an application exists somewhere it does not.
func (h *handler) submitApplication(w http.ResponseWriter, r *http.Request) {
	id := atoi64(r.PathValue("id"))
	if _, err := h.s.GetApplication(id); respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusNotImplemented, map[string]any{
		"error": "submitting an application is not implemented (phase 3)",
		"missing": []string{
			"an application agent that can drive a careers-page form or an ATS API",
			"a browser/automation harness for the sites that need one",
			"per-site credential handling via auth-store (:8303)",
			"a human confirmation gate before anything is sent under your name",
		},
		"do_instead": "record the outcome yourself: PATCH /applications/" +
			strconv.FormatInt(id, 10) +
			` {"agent_status":"submitted","stage":"submitted","submitted_at":<epoch>}` +
			" — agent_status is what the automation did, stage is where you stand with the employer",
	})
}

func (h *handler) deleteApplication(w http.ResponseWriter, r *http.Request) {
	id := atoi64(r.PathValue("id"))
	err := h.s.DeleteApplication(id)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// ---- application events ----

func (h *handler) listApplicationEvents(w http.ResponseWriter, r *http.Request) {
	id := atoi64(r.PathValue("id"))
	if _, err := h.s.GetApplication(id); respondStoreError(w, err) {
		return
	}
	events, err := h.s.ListApplicationEvents(id)
	if respondStoreError(w, err) {
		return
	}
	if events == nil {
		events = []*ApplicationEvent{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

// recordApplicationEvent appends a note to the timeline: something that happened
// without moving the stage. A stage change goes through PATCH /applications/{id},
// which appends its own event — so this route cannot write one, and a timeline
// can never claim a move the application never made.
func (h *handler) recordApplicationEvent(w http.ResponseWriter, r *http.Request) {
	id := atoi64(r.PathValue("id"))
	var e ApplicationEvent
	if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	got, err := h.s.RecordApplicationEvent(id, e)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, got)
}

// ---- application emails ----

func (h *handler) listApplicationEmails(w http.ResponseWriter, r *http.Request) {
	id := atoi64(r.PathValue("id"))
	if _, err := h.s.GetApplication(id); respondStoreError(w, err) {
		return
	}
	emails, err := h.s.ListApplicationEmails(id)
	if respondStoreError(w, err) {
		return
	}
	if emails == nil {
		emails = []*ApplicationEmail{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"emails": emails})
}

func (h *handler) linkApplicationEmail(w http.ResponseWriter, r *http.Request) {
	id := atoi64(r.PathValue("id"))
	var e ApplicationEmail
	if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	got, created, err := h.s.LinkApplicationEmail(id, &e)
	if respondStoreError(w, err) {
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, got)
}

// patchApplicationEmail is the confirm/reject channel: a matcher proposes a link
// and a human decides. It is the only way a link becomes `linked`, and therefore
// the only way one becomes able to drive a stage.
func (h *handler) patchApplicationEmail(w http.ResponseWriter, r *http.Request) {
	id := atoi64(r.PathValue("id"))
	var p ApplicationEmailPatch
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	got, err := h.s.PatchApplicationEmail(id, p)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, got)
}

func (h *handler) deleteApplicationEmail(w http.ResponseWriter, r *http.Request) {
	id := atoi64(r.PathValue("id"))
	if respondStoreError(w, h.s.DeleteApplicationEmail(id)) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// advanceStageFromEmail is the one route through which mail moves an application,
// so "only a linked email may drive a stage change" is enforced in a single place
// instead of asked of every caller that reads mail. A proposed link is a 400.
func (h *handler) advanceStageFromEmail(w http.ResponseWriter, r *http.Request) {
	id := atoi64(r.PathValue("id"))
	var req struct {
		Stage string `json:"stage"`
		Note  string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	got, err := h.s.AdvanceApplicationStageFromEmail(id, req.Stage, req.Note)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, got)
}

// ---- application tasks ----

func (h *handler) listApplicationTasks(w http.ResponseWriter, r *http.Request) {
	id := atoi64(r.PathValue("id"))
	if _, err := h.s.GetApplication(id); respondStoreError(w, err) {
		return
	}
	tasks, err := h.s.ListApplicationTasks(id)
	if respondStoreError(w, err) {
		return
	}
	if tasks == nil {
		tasks = []*ApplicationTask{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": tasks})
}

// linkApplicationTask stores a noteboard todo id and nothing else. The todo has
// to exist in noteboard already: this store never invents an id for a row it does
// not own.
func (h *handler) linkApplicationTask(w http.ResponseWriter, r *http.Request) {
	id := atoi64(r.PathValue("id"))
	var req struct {
		NoteboardID string `json:"noteboard_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	got, created, err := h.s.LinkApplicationTask(id, req.NoteboardID)
	if respondStoreError(w, err) {
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, got)
}

// createStandardApplicationTasks creates the standard follow-ups in noteboard and
// links the ids noteboard hands back. A noteboard that cannot be reached is a 502
// and no links are claimed — the alternative is an application listing todos that
// were never created.
func (h *handler) createStandardApplicationTasks(w http.ResponseWriter, r *http.Request) {
	id := atoi64(r.PathValue("id"))
	tasks, err := h.s.CreateStandardApplicationTasks(id)
	if respondStoreError(w, err) {
		return
	}
	if tasks == nil {
		tasks = []*ApplicationTask{}
	}
	writeJSON(w, http.StatusCreated, map[string]any{"tasks": tasks})
}

func (h *handler) deleteApplicationTask(w http.ResponseWriter, r *http.Request) {
	id := atoi64(r.PathValue("id"))
	if respondStoreError(w, h.s.DeleteApplicationTask(id)) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// respondStoreError maps the store's sentinel errors onto status codes and
// reports whether it handled the response (true = caller should stop).
//
// Every "the caller sent something wrong" sentinel lands on 400 with the store's
// own message, which enumerates the valid values — so an agent reading the 400
// can retry without guessing. Anything unrecognized is a 500, because a bug in
// here must not be reported to a caller as their mistake.
func respondStoreError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not found")
		return true
	}
	// noteboard being down is neither the caller's mistake nor a bug in here, so it
	// gets the status code that says "a dependency failed" rather than one that
	// blames the caller or hides behind a 500.
	if errors.Is(err, ErrNoteboard) {
		writeErr(w, http.StatusBadGateway, err.Error())
		return true
	}
	for _, invalid := range []error{
		ErrInvalidSource, ErrInvalidListing, ErrInvalidRoleFamily,
		ErrInvalidDocument, ErrInvalidApplication, ErrInvalidTailorRequest,
		ErrInvalidApplicationEvent, ErrInvalidApplicationEmail, ErrInvalidApplicationTask,
		ErrExtractionFailed,
	} {
		if errors.Is(err, invalid) {
			writeErr(w, http.StatusBadRequest, err.Error())
			return true
		}
	}
	writeErr(w, http.StatusInternalServerError, err.Error())
	return true
}

func atoi64(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

// isTrue reads a boolean query flag, accepting the spellings a caller is likely
// to reach for from a shell or a browser.
func isTrue(s string) bool {
	switch strings.ToLower(s) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// parseWhen turns an ISO string into an epoch second. It accepts full RFC3339
// timestamps ("2026-08-24T09:00:00-07:00"), plain datetimes without a zone
// (treated as UTC), and date-only values ("2026-08-24"), which mean midnight UTC
// of that day — the precision a job board actually publishes.
func parseWhen(s string) (int64, error) {
	s = strings.TrimSpace(s)
	layouts := []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02"}
	for _, l := range layouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.Unix(), nil
		}
	}
	return 0, fmt.Errorf("unrecognized date %q: use RFC3339, 2006-01-02T15:04:05, or 2006-01-02", s)
}
