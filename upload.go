package jobstore

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// blobDirName is the directory under the data dir holding original uploaded
// bytes, content-addressed: <data dir>/blobs/<sha256[:2]>/<sha256>.
//
// Content addressing means re-uploading the same file costs nothing and two
// documents that happen to share a filename cannot clobber each other's bytes.
const blobDirName = "blobs"

// MaxUploadBytes caps an upload. A resume is a few hundred kilobytes; ten
// megabytes is generous for a design-heavy PDF and small enough that a runaway
// or hostile upload cannot fill the disk before anyone notices.
const MaxUploadBytes int64 = 10 << 20

// pdftotextTimeout bounds the extraction subprocess. pdftotext on a resume takes
// milliseconds; a minute means it is stuck on a malformed file, and hanging the
// request forever is not a better answer than failing.
const pdftotextTimeout = 60 * time.Second

// ErrExtractionFailed marks a failure to get text out of an uploaded file. It is
// deliberately its own sentinel: this is the error that must never be swallowed,
// because the alternative — storing an empty body — is discovered much later by
// an agent submitting a blank application.
var ErrExtractionFailed = errors.New("extraction failed")

// Upload is one file arriving at POST /documents/upload.
type Upload struct {
	Name        string
	Kind        string
	Filename    string
	ContentType string
	Data        []byte
	Notes       string
	IsDefault   bool
}

// uploadFormats maps an accepted file extension to the document format it
// becomes. A file whose extension is not here is refused: guessing at the type
// of a resume is how a .pages file becomes an unreadable blob nobody notices.
var uploadFormats = map[string]string{
	".pdf":  DocumentFormatPDF,
	".docx": DocumentFormatDOCX,
	".txt":  DocumentFormatText,
	".md":   DocumentFormatMarkdown,
	".html": DocumentFormatHTML,
	".htm":  DocumentFormatHTML,
}

// UploadFormatExtensions lists the accepted extensions, so an error message can
// name them rather than leaving a caller to guess.
func UploadFormatExtensions() []string {
	return []string{".pdf", ".docx", ".txt", ".md", ".html", ".htm"}
}

// FormatForFilename resolves an uploaded filename to a document format.
func FormatForFilename(filename string) (string, error) {
	ext := strings.ToLower(filepath.Ext(strings.TrimSpace(filename)))
	if ext == "" {
		return "", fmt.Errorf("%w: %q has no file extension, so its type is unknown: use one of %s",
			ErrInvalidDocument, filename, strings.Join(UploadFormatExtensions(), ", "))
	}
	format, ok := uploadFormats[ext]
	if !ok {
		return "", fmt.Errorf("%w: unsupported file type %q: use one of %s",
			ErrInvalidDocument, ext, strings.Join(UploadFormatExtensions(), ", "))
	}
	return format, nil
}

// UploadDocument stores the original bytes and the text extracted from them as
// one document, upserting by name. It returns the row and whether it was created.
//
// Extraction runs BEFORE anything is written. A file whose text cannot be read
// is refused outright rather than stored with an empty body: a resume that
// silently became blank is worse than a refused upload, because the refusal is
// seen now and the blank is seen by an employer.
func (s *Store) UploadDocument(u Upload) (*Document, bool, error) {
	if strings.TrimSpace(u.Name) == "" {
		return nil, false, fmt.Errorf("%w: name required", ErrInvalidDocument)
	}
	if len(u.Data) == 0 {
		return nil, false, fmt.Errorf("%w: uploaded file is empty", ErrInvalidDocument)
	}
	if int64(len(u.Data)) > MaxUploadBytes {
		return nil, false, fmt.Errorf("%w: file is %d bytes, over the %d byte limit",
			ErrInvalidDocument, len(u.Data), MaxUploadBytes)
	}
	if u.Kind == "" {
		u.Kind = DocumentKindResume
	}
	if !ValidDocumentKind(u.Kind) {
		return nil, false, fmt.Errorf("%w: unknown kind %q: use one of %s",
			ErrInvalidDocument, u.Kind, strings.Join(DocumentKinds, ", "))
	}
	format, err := FormatForFilename(u.Filename)
	if err != nil {
		return nil, false, err
	}

	body, err := ExtractText(format, u.Data)
	if err != nil {
		return nil, false, err
	}

	sha, size, err := s.storeBlob(u.Data)
	if err != nil {
		return nil, false, err
	}

	ts := now()
	doc := &Document{
		Name:           u.Name,
		Kind:           u.Kind,
		Format:         format,
		Body:           body,
		SourceFilename: filepath.Base(u.Filename),
		ContentType:    u.ContentType,
		BlobSHA256:     sha,
		ByteSize:       size,
		ExtractedAt:    ts,
		IsDefault:      u.IsDefault,
		Notes:          u.Notes,
	}
	return s.UpsertDocument(doc)
}

// BlobPath is where the bytes with this sha256 live.
func (s *Store) BlobPath(sha string) string {
	return filepath.Join(s.dataDir, blobDirName, sha[:2], sha)
}

// storeBlob writes bytes to their content-addressed path and reports the digest
// and size. Re-storing identical bytes is a no-op by construction.
func (s *Store) storeBlob(data []byte) (string, int64, error) {
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	path := s.BlobPath(sha)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", 0, fmt.Errorf("mkdir blob dir: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", 0, fmt.Errorf("write blob: %w", err)
	}
	return sha, int64(len(data)), nil
}

// ReadBlob returns the original bytes of a document. A document with no blob —
// one typed in rather than uploaded — reads as ErrNotFound, because there is no
// original to serve and pretending otherwise would hand back the extracted text
// under the original's content type.
func (s *Store) ReadBlob(sha string) ([]byte, error) {
	if sha == "" {
		return nil, ErrNotFound
	}
	data, err := os.ReadFile(s.BlobPath(sha))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}

// ExtractText pulls readable text out of uploaded bytes according to format.
//
// Every path here either returns real text or an error. None of them returns an
// empty string with a nil error: a blank body written silently is the one
// failure mode this whole feature has to rule out.
func ExtractText(format string, data []byte) (string, error) {
	switch format {
	case DocumentFormatPDF:
		return extractPDF(data)
	case DocumentFormatDOCX:
		return extractDOCX(data)
	case DocumentFormatText, DocumentFormatMarkdown, DocumentFormatHTML:
		text := string(data)
		if strings.TrimSpace(text) == "" {
			return "", fmt.Errorf("%w: the file holds no text, only whitespace", ErrExtractionFailed)
		}
		return text, nil
	default:
		return "", fmt.Errorf("%w: no extractor for format %q", ErrExtractionFailed, format)
	}
}

// extractPDF shells out to pdftotext. -layout preserves the column structure a
// resume depends on: reflowed into a single column, a two-column resume
// interleaves its sidebar into the body and reads as nonsense.
//
// pdftotext missing, exiting non-zero, or producing only whitespace are all
// errors that name themselves. The last is the important one — a scanned or
// image-only PDF exits 0 and yields nothing, which is exactly the silent blank
// that must not reach the database.
func extractPDF(data []byte) (string, error) {
	binary, err := exec.LookPath("pdftotext")
	if err != nil {
		return "", fmt.Errorf(
			"%w: pdftotext is not on PATH, so no PDF can be read: install poppler-utils (%v)",
			ErrExtractionFailed, err)
	}

	tmp, err := os.CreateTemp("", "job-store-upload-*.pdf")
	if err != nil {
		return "", fmt.Errorf("%w: create temp file: %v", ErrExtractionFailed, err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", fmt.Errorf("%w: write temp file: %v", ErrExtractionFailed, err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("%w: close temp file: %v", ErrExtractionFailed, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), pdftotextTimeout)
	defer cancel()

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, binary, "-layout", "-enc", "UTF-8", tmp.Name(), "-")
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("%w: pdftotext could not read this PDF: %s", ErrExtractionFailed, detail)
	}
	text := stdout.String()
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf(
			"%w: pdftotext read the PDF but found no text — it is probably a scan or image-only export, so upload a text PDF or paste the text instead",
			ErrExtractionFailed)
	}
	return text, nil
}

// docxParagraph mirrors the fragment of WordprocessingML that carries text: a
// paragraph (w:p) holds runs, and a run holds text nodes (w:t). Everything else
// in the file — styles, numbering, drawing — is layout we do not need in a
// reading copy.
type docxParagraph struct {
	Texts []string `xml:"r>t"`
}

// extractDOCX reads the text of a .docx with the standard library only: a docx
// is a zip, and word/document.xml holds the body. Paragraphs become lines.
//
// Doing this with archive/zip and encoding/xml rather than a docx module is what
// keeps job-store on its single dependency.
func extractDOCX(data []byte) (string, error) {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("%w: this is not a readable .docx (a docx is a zip archive): %v",
			ErrExtractionFailed, err)
	}
	var documentXML *zip.File
	for _, f := range reader.File {
		if f.Name == "word/document.xml" {
			documentXML = f
			break
		}
	}
	if documentXML == nil {
		return "", fmt.Errorf("%w: the archive has no word/document.xml, so it is not a Word document",
			ErrExtractionFailed)
	}
	rc, err := documentXML.Open()
	if err != nil {
		return "", fmt.Errorf("%w: open word/document.xml: %v", ErrExtractionFailed, err)
	}
	defer rc.Close()
	raw, err := io.ReadAll(rc)
	if err != nil {
		return "", fmt.Errorf("%w: read word/document.xml: %v", ErrExtractionFailed, err)
	}

	decoder := xml.NewDecoder(bytes.NewReader(raw))
	var lines []string
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("%w: word/document.xml is malformed: %v", ErrExtractionFailed, err)
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Local != "p" {
			continue
		}
		var p docxParagraph
		if err := decoder.DecodeElement(&p, &start); err != nil {
			return "", fmt.Errorf("%w: word/document.xml is malformed: %v", ErrExtractionFailed, err)
		}
		lines = append(lines, strings.Join(p.Texts, ""))
	}
	text := strings.Join(lines, "\n")
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf(
			"%w: the .docx has no text in word/document.xml — if the content is inside images or text boxes, paste the text instead",
			ErrExtractionFailed)
	}
	return text, nil
}
