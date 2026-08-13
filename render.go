package jobstore

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// RenderDocument converts a document body to a standalone HTML page for preview
// and printing.
//
// The conversion is server-side on purpose: the preview an application agent
// reads through this endpoint and the preview the browser shows are then the same
// bytes, so a resume cannot look right in one and wrong in the other.
//
// Each format is rendered as what it actually is:
//   - markdown is converted;
//   - html passes through the wrapper unconverted, because it is already the
//     markup the user wrote and reformatting it would be a lossy transform;
//   - text, and the extracted text of a pdf or docx, render preformatted —
//     pdftotext -layout encodes the resume's columns in its spacing, and
//     reflowing it as prose would throw that away;
//   - pdf and docx additionally carry a banner pointing at /file, so nobody
//     mistakes the reading copy for the document an employer would receive.
func RenderDocument(d *Document) string {
	var body string
	switch d.Format {
	case DocumentFormatHTML:
		body = d.Body
	case DocumentFormatMarkdown:
		body = markdownToHTML(d.Body)
	default:
		body = `<pre class="plain">` + htmlEscape(d.Body) + `</pre>`
	}
	if DocumentHasOriginalBytes(d.Format) {
		body = originalBytesBanner(d) + body
	}
	return wrapDocumentHTML(d.Name, body)
}

// originalBytesBanner tells the reader that what follows is extracted text and
// where the real file is. Without it a tailoring agent could read this page and
// conclude the formatting was lost, when in fact the original is intact.
func originalBytesBanner(d *Document) string {
	link := "/documents/" + strconv.FormatInt(d.ID, 10) + "/file"
	name := d.SourceFilename
	if name == "" {
		name = "the uploaded file"
	}
	return fmt.Sprintf(
		`<div class="banner">Extracted text from <strong>%s</strong> (%s). `+
			`The original is the document that gets sent — <a href="%s">open it</a>.</div>`,
		htmlEscape(name), htmlEscape(strings.ToUpper(d.Format)), htmlEscape(link))
}

// wrapDocumentHTML puts a rendered body inside a minimal print-friendly page.
// The stylesheet is deliberately small and inline: this page has to print to a
// clean sheet of paper from a browser with no network, so it carries no fonts,
// no CDN and no script.
func wrapDocumentHTML(title, body string) string {
	return `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>` + htmlEscape(title) + `</title>
<style>
  :root { color-scheme: light; }
  * { box-sizing: border-box; }
  body {
    margin: 0;
    background: #f4f4f5;
    color: #18181b;
    font: 16px/1.55 -apple-system, BlinkMacSystemFont, "Segoe UI", Helvetica, Arial, sans-serif;
  }
  .page {
    max-width: 8.5in;
    margin: 24px auto;
    padding: 0.75in;
    background: #fff;
    box-shadow: 0 1px 3px rgba(0,0,0,.14);
  }
  h1, h2, h3, h4, h5, h6 { line-height: 1.25; margin: 1.4em 0 .5em; font-weight: 650; }
  h1 { font-size: 1.75rem; margin-top: 0; }
  h2 { font-size: 1.3rem; border-bottom: 1px solid #e4e4e7; padding-bottom: .2em; }
  h3 { font-size: 1.08rem; }
  p, ul, ol { margin: .6em 0; }
  ul, ol { padding-left: 1.4em; }
  li { margin: .2em 0; }
  a { color: #1d4ed8; }
  hr { border: 0; border-top: 1px solid #e4e4e7; margin: 1.4em 0; }
  code { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; font-size: .92em;
         background: #f4f4f5; padding: .1em .3em; border-radius: 3px; }
  pre.plain { white-space: pre-wrap; word-wrap: break-word; margin: 0;
              font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
              font-size: .88rem; line-height: 1.45; }
  .banner { background: #fef9c3; border: 1px solid #fde047; border-radius: 4px;
            padding: .6em .8em; margin: 0 0 1.2em; font-size: .88rem; }
  @media print {
    body { background: #fff; }
    .page { margin: 0; padding: 0; max-width: none; box-shadow: none; }
    .banner { display: none; }
    a { color: inherit; text-decoration: none; }
  }
</style>
</head>
<body>
<article class="page">
` + body + `
</article>
</body>
</html>
`
}

var htmlEscaper = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	`"`, "&quot;",
)

func htmlEscape(s string) string { return htmlEscaper.Replace(s) }

// ---- a small markdown renderer ----
//
// This is deliberately hand-written rather than a dependency: job-store has one
// module requirement (the SQLite driver) and a resume needs headings, emphasis,
// links, lists, rules and inline code — not the whole CommonMark surface. What is
// here is what a resume and a cover letter use.

var (
	headingPattern = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	// RE2 has no backreferences, so each rule character gets its own alternative
	// rather than "the same character three times".
	horizontalRule     = regexp.MustCompile(`^\s*(?:(?:-\s*){3,}|(?:\*\s*){3,}|(?:_\s*){3,})$`)
	bulletItemPattern  = regexp.MustCompile(`^\s*[-*+]\s+(.*)$`)
	numberedItem       = regexp.MustCompile(`^\s*\d+[.)]\s+(.*)$`)
	linkPattern        = regexp.MustCompile(`\[([^\]]*)\]\(([^)\s]*)\)`)
	strongAsterisks    = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	strongUnderscores  = regexp.MustCompile(`__([^_]+)__`)
	emphasisAsterisk   = regexp.MustCompile(`\*([^*]+)\*`)
	emphasisUnderscore = regexp.MustCompile(`_([^_]+)_`)
)

// markdownToHTML converts a markdown body to an HTML fragment.
func markdownToHTML(md string) string {
	lines := strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n")
	var out []string

	// One tiny state machine: at most one block is open at a time, and closeBlock
	// finishes whichever it is. Tracking it this way is what keeps a list from
	// swallowing the paragraph after it.
	var listTag string // "ul", "ol" or "" when no list is open
	var paragraph []string
	inFence := false
	var fence []string

	closeBlock := func() {
		if len(paragraph) > 0 {
			out = append(out, "<p>"+strings.Join(paragraph, "\n")+"</p>")
			paragraph = nil
		}
		if listTag != "" {
			out = append(out, "</"+listTag+">")
			listTag = ""
		}
	}
	openList := func(tag string) {
		if listTag == tag {
			return
		}
		if len(paragraph) > 0 {
			out = append(out, "<p>"+strings.Join(paragraph, "\n")+"</p>")
			paragraph = nil
		}
		if listTag != "" {
			out = append(out, "</"+listTag+">")
		}
		out = append(out, "<"+tag+">")
		listTag = tag
	}

	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			if inFence {
				out = append(out, "<pre><code>"+htmlEscape(strings.Join(fence, "\n"))+"</code></pre>")
				fence = nil
				inFence = false
			} else {
				closeBlock()
				inFence = true
			}
			continue
		}
		if inFence {
			fence = append(fence, line)
			continue
		}
		if strings.TrimSpace(line) == "" {
			closeBlock()
			continue
		}
		if horizontalRule.MatchString(line) {
			closeBlock()
			out = append(out, "<hr>")
			continue
		}
		if m := headingPattern.FindStringSubmatch(line); m != nil {
			closeBlock()
			level := strconv.Itoa(len(m[1]))
			out = append(out, "<h"+level+">"+renderInline(m[2])+"</h"+level+">")
			continue
		}
		if m := bulletItemPattern.FindStringSubmatch(line); m != nil {
			openList("ul")
			out = append(out, "<li>"+renderInline(m[1])+"</li>")
			continue
		}
		if m := numberedItem.FindStringSubmatch(line); m != nil {
			openList("ol")
			out = append(out, "<li>"+renderInline(m[1])+"</li>")
			continue
		}
		if listTag != "" {
			out = append(out, "</"+listTag+">")
			listTag = ""
		}
		paragraph = append(paragraph, renderInline(strings.TrimSpace(line)))
	}
	if inFence {
		// An unterminated fence is still content; drop it on the floor and it would
		// be a resume section that vanished.
		out = append(out, "<pre><code>"+htmlEscape(strings.Join(fence, "\n"))+"</code></pre>")
	}
	closeBlock()
	return strings.Join(out, "\n")
}

// renderInline applies the inline markup to one line of text.
//
// Order matters and is the reason this is not four chained ReplaceAll calls:
// code spans are taken out first so nothing inside backticks is reformatted, and
// links are lifted into placeholders before emphasis runs, so an underscore in a
// URL cannot be mistaken for italics.
func renderInline(s string) string {
	s = strings.ReplaceAll(s, "\x00", "")
	segments := strings.Split(s, "`")
	// An odd number of backticks means they are not delimiters at all; treat the
	// whole line as text rather than inventing a code span that runs to the end.
	if len(segments)%2 == 0 {
		return renderInlineText(s)
	}
	var b strings.Builder
	for i, segment := range segments {
		if i%2 == 1 {
			b.WriteString("<code>" + htmlEscape(segment) + "</code>")
			continue
		}
		b.WriteString(renderInlineText(segment))
	}
	return b.String()
}

func renderInlineText(s string) string {
	escaped := htmlEscape(s)

	var anchors []string
	withPlaceholders := linkPattern.ReplaceAllStringFunc(escaped, func(match string) string {
		parts := linkPattern.FindStringSubmatch(match)
		anchors = append(anchors, `<a href="`+parts[2]+`">`+applyEmphasis(parts[1])+`</a>`)
		return "\x00" + strconv.Itoa(len(anchors)-1) + "\x00"
	})

	rendered := applyEmphasis(withPlaceholders)
	for i, anchor := range anchors {
		rendered = strings.ReplaceAll(rendered, "\x00"+strconv.Itoa(i)+"\x00", anchor)
	}
	return rendered
}

func applyEmphasis(s string) string {
	s = strongAsterisks.ReplaceAllString(s, "<strong>$1</strong>")
	s = strongUnderscores.ReplaceAllString(s, "<strong>$1</strong>")
	s = emphasisAsterisk.ReplaceAllString(s, "<em>$1</em>")
	s = emphasisUnderscore.ReplaceAllString(s, "<em>$1</em>")
	return s
}
