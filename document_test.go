package jobstore

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func newTestDocument(t *testing.T, s *Store, name, kind string) *Document {
	t.Helper()
	d, _, err := s.UpsertDocument(&Document{
		Name: name, Kind: kind, Format: DocumentFormatMarkdown, Body: "# " + name,
	})
	if err != nil {
		t.Fatalf("create document %q: %v", name, err)
	}
	return d
}

// A default that could be ambiguous is not a default: the moment two resumes
// both claim it, an application agent picks one at random and nobody can say
// which resume was sent.
func TestSettingTheDefaultClearsEveryOtherDocumentOfThatKind(t *testing.T) {
	s := openTestStore(t)
	first := newTestDocument(t, s, "resume-general", DocumentKindResume)
	second := newTestDocument(t, s, "resume-ai", DocumentKindResume)
	letter := newTestDocument(t, s, "cover-general", DocumentKindCoverLetter)

	yes := true
	if _, err := s.PatchDocument(first.ID, DocumentPatch{IsDefault: &yes}); err != nil {
		t.Fatalf("set first default: %v", err)
	}
	if _, err := s.PatchDocument(letter.ID, DocumentPatch{IsDefault: &yes}); err != nil {
		t.Fatalf("set letter default: %v", err)
	}
	// The second resume takes the flag; the first must lose it in the same write.
	if _, err := s.PatchDocument(second.ID, DocumentPatch{IsDefault: &yes}); err != nil {
		t.Fatalf("set second default: %v", err)
	}

	assertExactlyOneDefault(t, s, DocumentKindResume, second.ID)
	// A different kind has its own default and must not be disturbed.
	assertExactlyOneDefault(t, s, DocumentKindCoverLetter, letter.ID)

	// An upsert is the other write path onto the same flag, and has to behave the
	// same way — otherwise "which resume is the default" depends on which endpoint
	// you happened to use.
	if _, _, err := s.UpsertDocument(&Document{
		Name: "resume-general", Kind: DocumentKindResume, Body: "# resume", IsDefault: true,
	}); err != nil {
		t.Fatalf("upsert as default: %v", err)
	}
	assertExactlyOneDefault(t, s, DocumentKindResume, first.ID)
}

func assertExactlyOneDefault(t *testing.T, s *Store, kind string, wantID int64) {
	t.Helper()
	docs, err := s.ListDocuments(DocumentFilter{Kind: kind})
	if err != nil {
		t.Fatalf("list %s: %v", kind, err)
	}
	var defaults []int64
	for _, d := range docs {
		if d.IsDefault {
			defaults = append(defaults, d.ID)
		}
	}
	if len(defaults) != 1 || defaults[0] != wantID {
		t.Fatalf("%s defaults = %v, want exactly [%d]", kind, defaults, wantID)
	}
}

func TestDocumentRejectsUnknownKindAndFormat(t *testing.T) {
	s := openTestStore(t)
	if _, _, err := s.UpsertDocument(&Document{Name: "a", Kind: "manifesto"}); !errors.Is(err, ErrInvalidDocument) {
		t.Errorf("unknown kind error = %v, want ErrInvalidDocument", err)
	}
	if _, _, err := s.UpsertDocument(&Document{Name: "b", Format: "rtf"}); !errors.Is(err, ErrInvalidDocument) {
		t.Errorf("unknown format error = %v, want ErrInvalidDocument", err)
	}
}

func TestDocumentFilterFindsTailoredCopies(t *testing.T) {
	s := openTestStore(t)
	original := newTestDocument(t, s, "resume-general", DocumentKindResume)
	listing := newTestListing(t, s, "Acme", "Senior Backend Engineer")

	if _, _, err := s.UpsertDocument(&Document{
		Name: "resume-acme", Kind: DocumentKindResume, Body: "# tailored",
		DerivedFromDocumentID: original.ID, ListingID: listing.ID,
	}); err != nil {
		t.Fatalf("create tailored copy: %v", err)
	}

	derived, err := s.ListDocuments(DocumentFilter{DerivedFrom: original.ID})
	if err != nil {
		t.Fatalf("list derived: %v", err)
	}
	if len(derived) != 1 || derived[0].Name != "resume-acme" {
		t.Fatalf("derived_from returned %d row(s), want the one tailored copy", len(derived))
	}
	forListing, err := s.ListDocuments(DocumentFilter{ListingID: listing.ID})
	if err != nil {
		t.Fatalf("list by listing: %v", err)
	}
	if len(forListing) != 1 || forListing[0].Name != "resume-acme" {
		t.Fatalf("listing_id returned %d row(s), want the one tailored copy", len(forListing))
	}
	// And the original is untouched — tailoring never overwrites what it started from.
	if again, err := s.GetDocument(original.ID); err != nil || again.Body != "# resume-general" {
		t.Errorf("the source document changed: %+v (%v)", again, err)
	}
}

// ---- upload and extraction ----

// minimalDOCX builds a real .docx in memory: a zip whose word/document.xml holds
// two paragraphs. Building it here rather than checking a binary fixture into
// the repo keeps what is being tested visible.
func minimalDOCX(t *testing.T, paragraphs ...string) []byte {
	t.Helper()
	var xml bytes.Buffer
	xml.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>`)
	for _, p := range paragraphs {
		xml.WriteString(`<w:p><w:r><w:t>` + p + `</w:t></w:r></w:p>`)
	}
	xml.WriteString(`</w:body></w:document>`)

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatalf("zip create: %v", err)
	}
	if _, err := w.Write(xml.Bytes()); err != nil {
		t.Fatalf("zip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func TestUploadExtractsDOCXTextAndKeepsTheOriginal(t *testing.T) {
	s := openTestStore(t)
	data := minimalDOCX(t, "Vlad Kayushkin", "Senior Backend Engineer", "Go, SQLite, Kubernetes")

	doc, created, err := s.UploadDocument(Upload{
		Name: "resume-docx", Kind: DocumentKindResume, Filename: "Resume.docx",
		ContentType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		Data:        data,
	})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if !created {
		t.Error("the first upload of a name was not reported as created")
	}
	if doc.Format != DocumentFormatDOCX {
		t.Errorf("format = %q, want %q", doc.Format, DocumentFormatDOCX)
	}
	for _, want := range []string{"Vlad Kayushkin", "Senior Backend Engineer", "Kubernetes"} {
		if !strings.Contains(doc.Body, want) {
			t.Errorf("extracted body is missing %q; body = %q", want, doc.Body)
		}
	}
	if !strings.Contains(doc.Body, "\n") {
		t.Error("paragraphs were run together; w:p must break lines")
	}
	if doc.ExtractedAt == 0 {
		t.Error("extracted_at was not stamped")
	}
	// The original is kept alongside the extraction, byte for byte.
	if doc.BlobSHA256 == "" || doc.ByteSize != int64(len(data)) {
		t.Fatalf("blob_sha256=%q byte_size=%d, want the original recorded", doc.BlobSHA256, doc.ByteSize)
	}
	original, err := s.ReadBlob(doc.BlobSHA256)
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	if !bytes.Equal(original, data) {
		t.Error("the stored original does not match the uploaded bytes")
	}
	if doc.SourceFilename != "Resume.docx" {
		t.Errorf("source_filename = %q, want the uploaded name", doc.SourceFilename)
	}
}

// minimalPDF builds a real one-page PDF holding the given lines, so the pdftotext
// path is exercised for real rather than only through its failure modes. It is
// assembled here rather than checked in as a fixture so the bytes under test are
// readable.
func minimalPDF(t *testing.T, lines ...string) []byte {
	t.Helper()
	var content bytes.Buffer
	content.WriteString("BT /F1 18 Tf 72 720 Td")
	for i, line := range lines {
		if i > 0 {
			content.WriteString(" 0 -24 Td")
		}
		content.WriteString(" (" + line + ") Tj")
	}
	content.WriteString(" ET")

	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R " +
			"/Resources << /Font << /F1 5 0 R >> >> >>",
		"<< /Length " + strconv.Itoa(content.Len()) + " >>\nstream\n" + content.String() + "\nendstream",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}

	var pdf bytes.Buffer
	pdf.WriteString("%PDF-1.4\n")
	offsets := make([]int, 0, len(objects))
	for i, object := range objects {
		offsets = append(offsets, pdf.Len())
		pdf.WriteString(strconv.Itoa(i+1) + " 0 obj\n" + object + "\nendobj\n")
	}
	xref := pdf.Len()
	pdf.WriteString("xref\n0 " + strconv.Itoa(len(objects)+1) + "\n0000000000 65535 f \n")
	for _, offset := range offsets {
		pdf.WriteString(strings.Repeat("0", 10-len(strconv.Itoa(offset))) + strconv.Itoa(offset) + " 00000 n \n")
	}
	pdf.WriteString("trailer\n<< /Size " + strconv.Itoa(len(objects)+1) + " /Root 1 0 R >>\nstartxref\n" +
		strconv.Itoa(xref) + "\n%%EOF\n")
	return pdf.Bytes()
}

func TestUploadExtractsPDFTextAndKeepsTheOriginal(t *testing.T) {
	s := openTestStore(t)
	data := minimalPDF(t, "VLAD KAYUSHKIN", "Staff Backend Engineer")

	doc, _, err := s.UploadDocument(Upload{
		Name: "resume-pdf", Kind: DocumentKindResume, Filename: "Resume.pdf",
		ContentType: "application/pdf", Data: data,
	})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if doc.Format != DocumentFormatPDF {
		t.Errorf("format = %q, want %q", doc.Format, DocumentFormatPDF)
	}
	for _, want := range []string{"VLAD KAYUSHKIN", "Staff Backend Engineer"} {
		if !strings.Contains(doc.Body, want) {
			t.Errorf("extracted body is missing %q; body = %q", want, doc.Body)
		}
	}
	original, err := s.ReadBlob(doc.BlobSHA256)
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	if !bytes.Equal(original, data) {
		t.Error("the stored original does not match the uploaded PDF byte for byte")
	}
}

// The failure that matters is not an error message — it is an empty body written
// as though nothing was wrong, found much later by an agent that submitted a
// blank resume. Every extraction failure below must leave no document at all.
func TestExtractionFailureIsAnErrorAndNeverAnEmptyBody(t *testing.T) {
	s := openTestStore(t)

	cases := []struct {
		name     string
		upload   Upload
		wantWord string
	}{
		{
			name: "a pdf pdftotext cannot read",
			upload: Upload{
				Name: "broken-pdf", Filename: "resume.pdf",
				Data: []byte("this is not a PDF at all, just text with a .pdf name"),
			},
			wantWord: "pdftotext",
		},
		{
			name: "a docx that is not a zip",
			upload: Upload{
				Name: "broken-docx", Filename: "resume.docx",
				Data: []byte("definitely not a zip archive"),
			},
			wantWord: "zip",
		},
		{
			name: "a zip with no word/document.xml",
			upload: Upload{
				Name: "wrong-zip", Filename: "resume.docx",
				Data: zipWithout(t),
			},
			wantWord: "word/document.xml",
		},
		{
			name: "a docx whose text is empty",
			upload: Upload{
				Name: "blank-docx", Filename: "resume.docx",
				Data: minimalDOCX(t, "", "   "),
			},
			wantWord: "no text",
		},
		{
			name: "a text file holding only whitespace",
			upload: Upload{
				Name: "blank-txt", Filename: "resume.txt",
				Data: []byte("   \n\t\n  "),
			},
			wantWord: "whitespace",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _, err := s.UploadDocument(c.upload)
			if err == nil {
				t.Fatalf("upload succeeded with body %q; a failed extraction must never be stored", got.Body)
			}
			if !errors.Is(err, ErrExtractionFailed) {
				t.Errorf("error = %v, want ErrExtractionFailed", err)
			}
			if !strings.Contains(err.Error(), c.wantWord) {
				t.Errorf("error %q does not name the cause (%q)", err, c.wantWord)
			}
			docs, listErr := s.ListDocuments(DocumentFilter{})
			if listErr != nil {
				t.Fatalf("list: %v", listErr)
			}
			for _, d := range docs {
				if d.Name == c.upload.Name {
					t.Fatalf("a document was written anyway, with body %q", d.Body)
				}
			}
		})
	}
}

func zipWithout(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("readme.txt")
	if err != nil {
		t.Fatalf("zip create: %v", err)
	}
	if _, err := w.Write([]byte("not a word document")); err != nil {
		t.Fatalf("zip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func TestUploadRejectsAnUnsupportedFileType(t *testing.T) {
	s := openTestStore(t)
	_, _, err := s.UploadDocument(Upload{Name: "pages", Filename: "resume.pages", Data: []byte("x")})
	if !errors.Is(err, ErrInvalidDocument) {
		t.Fatalf("error = %v, want ErrInvalidDocument", err)
	}
	for _, ext := range UploadFormatExtensions() {
		if !strings.Contains(err.Error(), ext) {
			t.Errorf("error %q does not name the accepted extension %q", err, ext)
		}
	}
}

func TestUploadRejectsAFileOverTheSizeLimit(t *testing.T) {
	s := openTestStore(t)
	_, _, err := s.UploadDocument(Upload{
		Name: "huge", Filename: "resume.txt", Data: bytes.Repeat([]byte("a"), int(MaxUploadBytes)+1),
	})
	if !errors.Is(err, ErrInvalidDocument) {
		t.Errorf("error = %v, want ErrInvalidDocument", err)
	}
}

func TestUploadedTextKeepsItsBytesAsTheBody(t *testing.T) {
	s := openTestStore(t)
	body := "VLAD KAYUSHKIN\n\nEXPERIENCE\n  Staff Engineer      2020-2026\n"
	doc, _, err := s.UploadDocument(Upload{
		Name: "resume-txt", Filename: "resume.txt", ContentType: "text/plain", Data: []byte(body),
	})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if doc.Body != body {
		t.Errorf("body = %q, want the uploaded bytes unchanged", doc.Body)
	}
	if doc.Format != DocumentFormatText {
		t.Errorf("format = %q, want %q", doc.Format, DocumentFormatText)
	}
}

// ---- serving the original ----

func TestDocumentFileServesTheOriginalAndTypedInDocumentsHaveNone(t *testing.T) {
	srv, s := newTestServer(t)

	data := minimalDOCX(t, "Vlad Kayushkin")
	uploaded, _, err := s.UploadDocument(Upload{
		Name: "resume-docx", Filename: "Resume.docx", ContentType: "application/docx", Data: data,
	})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	typedIn := newTestDocument(t, s, "resume-typed", DocumentKindResume)

	resp, err := http.Get(srv.URL + "/documents/" + itoa(uploaded.ID) + "/file")
	if err != nil {
		t.Fatalf("get file: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/docx" {
		t.Errorf("content-type = %q, want the stored one", got)
	}
	if got := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(got, "inline") {
		t.Errorf("content-disposition = %q, want it to start with inline", got)
	}
	served := make([]byte, len(data)+1)
	n, _ := resp.Body.Read(served)
	if !bytes.Equal(served[:n], data[:n]) {
		t.Error("the served bytes differ from the uploaded original")
	}

	resp2, err := http.Get(srv.URL + "/documents/" + itoa(typedIn.ID) + "/file")
	if err != nil {
		t.Fatalf("get typed-in file: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d for a typed-in document, want 404", resp2.StatusCode)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestUploadOverHTTPAcceptsMultipartAndReportsExtractionFailure(t *testing.T) {
	srv, _ := newTestServer(t)

	post := func(t *testing.T, name, filename string, data []byte) *http.Response {
		t.Helper()
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		if err := writer.WriteField("name", name); err != nil {
			t.Fatalf("write name: %v", err)
		}
		if err := writer.WriteField("kind", DocumentKindResume); err != nil {
			t.Fatalf("write kind: %v", err)
		}
		part, err := writer.CreateFormFile("file", filename)
		if err != nil {
			t.Fatalf("create form file: %v", err)
		}
		if _, err := part.Write(data); err != nil {
			t.Fatalf("write file: %v", err)
		}
		if err := writer.Close(); err != nil {
			t.Fatalf("close writer: %v", err)
		}
		resp, err := http.Post(srv.URL+"/documents/upload", writer.FormDataContentType(), &body)
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		return resp
	}

	good := post(t, "resume-upload", "Resume.docx", minimalDOCX(t, "Vlad Kayushkin", "Staff Engineer"))
	defer good.Body.Close()
	if good.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", good.StatusCode)
	}
	var doc Document
	if err := json.NewDecoder(good.Body).Decode(&doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(doc.Body, "Vlad Kayushkin") {
		t.Errorf("body = %q, want the extracted text", doc.Body)
	}

	// An unreadable file has to come back as a 400 saying why, not a 201 with an
	// empty body that only shows up when an employer opens the attachment.
	bad := post(t, "broken-upload", "Resume.pdf", []byte("not a pdf"))
	defer bad.Body.Close()
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", bad.StatusCode)
	}
	var errBody struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(bad.Body).Decode(&errBody); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(errBody.Error, "pdftotext") {
		t.Errorf("error %q does not name the cause", errBody.Error)
	}

	// A name of its own is required: falling back to the filename would let two
	// resume.pdf uploads collide and silently overwrite each other.
	nameless := post(t, "", "Resume.docx", minimalDOCX(t, "Vlad"))
	defer nameless.Body.Close()
	if nameless.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d for an upload with no name, want 400", nameless.StatusCode)
	}
}

// ---- rendering ----

func TestRenderMarkdownProducesTheExpectedHTML(t *testing.T) {
	doc := &Document{
		Name:   "resume-general",
		Format: DocumentFormatMarkdown,
		Body: "# Vlad Kayushkin\n\n" +
			"Staff engineer with **fifteen years** of _backend_ experience.\n" +
			"Reach me at [my site](https://kayushkin.com/about_me).\n\n" +
			"## Experience\n\n" +
			"- Built `job-store`\n" +
			"- Ran the payments ledger\n\n" +
			"---\n\n" +
			"1. First\n" +
			"2. Second\n",
	}
	got := RenderDocument(doc)

	for _, want := range []string{
		"<!DOCTYPE html>",
		"<title>resume-general</title>",
		"<h1>Vlad Kayushkin</h1>",
		"<h2>Experience</h2>",
		"<strong>fifteen years</strong>",
		"<em>backend</em>",
		`<a href="https://kayushkin.com/about_me">my site</a>`,
		"<ul>",
		"<li>Built <code>job-store</code></li>",
		"<ol>",
		"<li>First</li>",
		"<hr>",
		"@media print",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered HTML is missing %q", want)
		}
	}
	// An underscore inside a URL is not emphasis.
	if strings.Contains(got, "about<em>me</em>") || strings.Contains(got, "about_me</em>") {
		t.Error("a URL's underscores were treated as emphasis")
	}
}

func TestRenderEscapesUserContent(t *testing.T) {
	doc := &Document{
		Name: "resume", Format: DocumentFormatMarkdown,
		Body: "I once wrote <script>alert(1)</script> & lived.",
	}
	got := RenderDocument(doc)
	if strings.Contains(got, "<script>alert(1)</script>") {
		t.Error("markdown body content was not escaped")
	}
	if !strings.Contains(got, "&lt;script&gt;") || !strings.Contains(got, "&amp;") {
		t.Error("escaping did not produce the expected entities")
	}
}

func TestRenderHTMLBodyPassesThroughTheWrapperUnconverted(t *testing.T) {
	body := `<h1>Vlad</h1><p class="lede">Already <b>markup</b>.</p>`
	got := RenderDocument(&Document{Name: "resume-html", Format: DocumentFormatHTML, Body: body})
	if !strings.Contains(got, body) {
		t.Errorf("an html body was rewritten; want it passed through verbatim.\ngot: %s", got)
	}
	if !strings.Contains(got, "<style>") {
		t.Error("an html body was not wrapped in the print stylesheet")
	}
}

func TestRenderExtractedFormatsCarryABannerPointingAtTheOriginal(t *testing.T) {
	for _, format := range []string{DocumentFormatPDF, DocumentFormatDOCX} {
		doc := &Document{
			ID: 7, Name: "resume-upload", Format: format, SourceFilename: "Resume.pdf",
			Body: "VLAD KAYUSHKIN\n  Staff Engineer",
		}
		got := RenderDocument(doc)
		if !strings.Contains(got, "/documents/7/file") {
			t.Errorf("%s render has no link to the original file", format)
		}
		if !strings.Contains(got, `<pre class="plain">`) {
			t.Errorf("%s render did not preserve the extracted layout as preformatted text", format)
		}
	}
	// A markdown document has no original, so it must not claim one.
	plain := RenderDocument(&Document{ID: 8, Name: "typed", Format: DocumentFormatMarkdown, Body: "# Hi"})
	if strings.Contains(plain, "/documents/8/file") {
		t.Error("a typed-in document rendered a link to an original it does not have")
	}
}

// ---- tailor requests ----

func TestTailorRequestIsQueuedAndValidated(t *testing.T) {
	s := openTestStore(t)
	doc := newTestDocument(t, s, "resume-general", DocumentKindResume)
	listing := newTestListing(t, s, "Acme", "Senior Backend Engineer")

	req, err := s.CreateTailorRequest(doc.ID, listing.ID, "lean on the payments work")
	if err != nil {
		t.Fatalf("create tailor request: %v", err)
	}
	if req.Status != TailorStatusPending {
		t.Errorf("status = %q, want %q — the service queues, it does not run", req.Status, TailorStatusPending)
	}
	if req.ResultDocumentID != 0 {
		t.Error("a fresh request already points at a result document")
	}

	pending, err := s.ListTailorRequests(TailorRequestFilter{Status: TailorStatusPending})
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != req.ID {
		t.Fatalf("pending queue has %d entries, want the one just created", len(pending))
	}
}

func TestTailorRequestIsRefusedWithoutAJobDescription(t *testing.T) {
	s := openTestStore(t)
	doc := newTestDocument(t, s, "resume-general", DocumentKindResume)
	bare, _, err := s.UpsertListing(&Listing{Company: "Acme", Title: "Senior Backend Engineer"})
	if err != nil {
		t.Fatalf("create bare listing: %v", err)
	}

	_, err = s.CreateTailorRequest(doc.ID, bare.ID, "")
	if !errors.Is(err, ErrInvalidTailorRequest) {
		t.Fatalf("error = %v, want ErrInvalidTailorRequest: there is nothing to tailor against", err)
	}
	if !strings.Contains(err.Error(), "description") {
		t.Errorf("error %q does not say the description is what is missing", err)
	}
}

func TestTailorRequestIsRefusedOnUnknownIDs(t *testing.T) {
	s := openTestStore(t)
	doc := newTestDocument(t, s, "resume-general", DocumentKindResume)
	listing := newTestListing(t, s, "Acme", "Senior Backend Engineer")

	if _, err := s.CreateTailorRequest(doc.ID, 987654, ""); !errors.Is(err, ErrInvalidTailorRequest) {
		t.Errorf("unknown listing error = %v, want ErrInvalidTailorRequest", err)
	}
	if _, err := s.CreateTailorRequest(987654, listing.ID, ""); !errors.Is(err, ErrInvalidTailorRequest) {
		t.Errorf("unknown document error = %v, want ErrInvalidTailorRequest", err)
	}
	// A listing the user swept is not a live listing either.
	if err := s.SoftDeleteListing(listing.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	if _, err := s.CreateTailorRequest(doc.ID, listing.ID, ""); !errors.Is(err, ErrInvalidTailorRequest) {
		t.Errorf("swept listing error = %v, want ErrInvalidTailorRequest", err)
	}
}

func TestDispatcherMovesATailorRequestThroughItsStatuses(t *testing.T) {
	s := openTestStore(t)
	doc := newTestDocument(t, s, "resume-general", DocumentKindResume)
	listing := newTestListing(t, s, "Acme", "Senior Backend Engineer")
	req, err := s.CreateTailorRequest(doc.ID, listing.ID, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	running := TailorStatusRunning
	started := now()
	if _, err := s.PatchTailorRequest(req.ID, TailorRequestPatch{Status: &running, StartedAt: &started}); err != nil {
		t.Fatalf("start: %v", err)
	}

	result, _, err := s.UpsertDocument(&Document{
		Name: "resume-acme", Kind: DocumentKindResume, Body: "# tailored",
		DerivedFromDocumentID: doc.ID, ListingID: listing.ID,
	})
	if err != nil {
		t.Fatalf("write result document: %v", err)
	}
	ready := TailorStatusReady
	done, err := s.PatchTailorRequest(req.ID, TailorRequestPatch{
		Status: &ready, ResultDocumentID: &result.ID, FinishedAt: &started,
	})
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	if done.Status != TailorStatusReady || done.ResultDocumentID != result.ID {
		t.Errorf("finished request = %+v, want ready and pointing at %d", done, result.ID)
	}

	bogus := int64(555555)
	if _, err := s.PatchTailorRequest(req.ID, TailorRequestPatch{ResultDocumentID: &bogus}); !errors.Is(err, ErrInvalidTailorRequest) {
		t.Errorf("error = %v, want ErrInvalidTailorRequest for a result id naming no document", err)
	}
	unknown := "finished-ish"
	if _, err := s.PatchTailorRequest(req.ID, TailorRequestPatch{Status: &unknown}); !errors.Is(err, ErrInvalidTailorRequest) {
		t.Errorf("error = %v, want ErrInvalidTailorRequest for an unknown status", err)
	}
}

func TestTailorOverHTTPAnswers202AndRefusesAThinDescriptionWith400(t *testing.T) {
	srv, s := newTestServer(t)
	doc := newTestDocument(t, s, "resume-general", DocumentKindResume)
	listing := newTestListing(t, s, "Acme", "Senior Backend Engineer")
	bare, _, err := s.UpsertListing(&Listing{Company: "Globex", Title: "Backend Engineer"})
	if err != nil {
		t.Fatalf("create bare listing: %v", err)
	}

	resp, err := http.Post(srv.URL+"/documents/"+itoa(doc.ID)+"/tailor", "application/json",
		strings.NewReader(`{"listing_id":`+itoa(listing.ID)+`,"instructions":"lead with Go"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("status = %d, want 202: the request is queued, not done", resp.StatusCode)
	}

	resp2, err := http.Post(srv.URL+"/documents/"+itoa(doc.ID)+"/tailor", "application/json",
		strings.NewReader(`{"listing_id":`+itoa(bare.ID)+`}`))
	if err != nil {
		t.Fatalf("post bare: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a listing with no description", resp2.StatusCode)
	}
}

func TestSubmitApplicationIsNotImplementedAndSaysWhatIsMissing(t *testing.T) {
	srv, s := newTestServer(t)
	listing := newTestListing(t, s, "Acme", "Senior Backend Engineer")
	app, _, err := s.UpsertApplication(&Application{ListingID: listing.ID})
	if err != nil {
		t.Fatalf("create application: %v", err)
	}

	resp, err := http.Post(srv.URL+"/applications/"+itoa(app.ID)+"/submit", "application/json", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", resp.StatusCode)
	}
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(buf.String(), "missing") {
		t.Errorf("body %q does not name what is missing", buf.String())
	}
}
