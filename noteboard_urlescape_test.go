package jobstore

import (
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"strings"
	"testing"
)

// urlProbes are the noteboard ids job-store can be handed. noteboard assigns
// them, so job-store does not choose them; only the first is well-formed and
// the rest each carry a byte that means something to a URL path.
//
// Which probe carries which arm is a measurement, not a guess — see the README
// beside this repo's instruments:
//
//   - "item one" cannot witness the defect at all. net/url escapes a raw space
//     to %20 unaided, so it round-trips correctly whether or not the caller
//     escapes. It is here as a PathEscape-vs-QueryEscape control: those two
//     disagree on it (%20 against +) and on "item?type=note" (= against %3D),
//     and on nothing else in this list — and because this pin unescapes the
//     segment before comparing, and PathUnescape reads %3D back to '=', the
//     space is the only one of the two that can change a verdict.
//   - The other five are defect witnesses.
var urlProbes = []string{
	"018f2c1a-4d3b-7e91-a0c5-2b6f8d19e4aa", // well-formed control — must pass before and after
	"item#frag",                            // # truncates the path at a fragment
	"item?type=note",                       // ? truncates the path at a query
	"a/b",                                  // an extra path segment
	"../lists",                             // climbs out of /api/items/ into /api/lists
	"item one",                             // wrong-repair control only; see above
	"item%2Fb",                             // already-encoded; a second escape must not be skipped
}

// assertGetAddressedItem reads the WIRE, not the decoded path. Go's server
// decodes %2F back to a slash before it fills r.URL.Path, so a URL.Path
// assertion reads identically whether or not the client escaped anything — it
// cannot detect this defect in either direction. RequestURI is the raw request
// line.
func assertGetAddressedItem(t *testing.T, requestURI, wantID string) {
	t.Helper()
	u, err := neturl.ParseRequestURI(requestURI)
	if err != nil {
		t.Fatalf("GET request line %q did not parse: %v", requestURI, err)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		t.Fatalf("GET for id %q leaked into the query/fragment: request line was %q", wantID, requestURI)
	}
	segs := strings.Split(strings.Trim(u.EscapedPath(), "/"), "/")
	if len(segs) != 3 || segs[0] != "api" || segs[1] != "items" {
		t.Fatalf("GET for id %q addressed %q, want exactly /api/items/<one segment>", wantID, u.EscapedPath())
	}
	got, err := neturl.PathUnescape(segs[2])
	if err != nil {
		t.Fatalf("GET for id %q wrote an undecodable segment %q: %v", wantID, segs[2], err)
	}
	if got != wantID {
		t.Fatalf("GET addressed item %q, want %q (request line %q)", got, wantID, requestURI)
	}
}

// newNoteboardStubRecording records the raw request line of whatever reaches it
// and answers with well-formed JSON.
//
// Registering "/" rather than the exact item route is deliberate: a request
// that escaped /api/items/<id> must be RECORDED so the assertion can name where
// it went, rather than 404'd into silence. It is also what makes this defect
// worth pinning — noteboard answers /api/lists with valid JSON, and GetItem
// passes those bytes through unchanged, so the caller gets the wrong entity and
// no error at all.
func newNoteboardStubRecording(t *testing.T, requestURI *string, hit *bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*hit = true
		*requestURI = r.RequestURI
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"stub"}`))
	}))
}

func TestGetItemAddressesTheItemNoteboardNamed(t *testing.T) {
	for _, id := range urlProbes {
		t.Run(id, func(t *testing.T) {
			var requestURI string
			var hit bool
			srv := newNoteboardStubRecording(t, &requestURI, &hit)
			defer srv.Close()

			c := NewNoteboardClient(srv.URL)
			if _, err := c.GetItem(id); err != nil {
				t.Fatalf("GetItem(%q): %v", id, err)
			}

			if !hit {
				t.Fatalf("no GET reached noteboard at all for id %q", id)
			}
			assertGetAddressedItem(t, requestURI, id)
		})
	}
}
