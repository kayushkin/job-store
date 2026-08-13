package jobstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// ErrNoteboard marks a failure to reach or read noteboard, which is neither the
// caller's mistake (400) nor a bug in this service (500) — it is a dependency
// being down, and the HTTP layer answers 502 so it reads as one.
var ErrNoteboard = errors.New("noteboard")

// DefaultNoteboardURL is where noteboard listens. Overridable with NOTEBOARD_URL,
// which is what a test injects to point the client at a stub — or at nothing, to
// prove an unreachable noteboard is reported rather than swallowed.
const DefaultNoteboardURL = "http://localhost:8191"

// NoteboardClient reads and writes todos in noteboard, which owns them. job-store
// stores their ids and never their content, so every call here is a read-through
// or a create — there is no cache to go stale.
type NoteboardClient struct {
	BaseURL string
	HTTP    *http.Client
}

// NewNoteboardClient builds a client for baseURL, falling back to NOTEBOARD_URL
// and then to DefaultNoteboardURL.
//
// The timeout is short on purpose: expanding tasks happens inside a GET a person
// is waiting on, and a noteboard that is not answering has to be reported
// quickly rather than hold the request open.
func NewNoteboardClient(baseURL string) *NoteboardClient {
	if baseURL == "" {
		baseURL = os.Getenv("NOTEBOARD_URL")
	}
	if baseURL == "" {
		baseURL = DefaultNoteboardURL
	}
	return &NoteboardClient{
		BaseURL: strings.TrimSuffix(baseURL, "/"),
		HTTP:    &http.Client{Timeout: 5 * time.Second},
	}
}

// GetItem reads one noteboard item and returns it exactly as noteboard served
// it. The bytes are passed through unchanged: this layer is transparent, and
// decoding into a local struct would make job-store a second, narrower schema for
// something noteboard owns.
func (c *NoteboardClient) GetItem(noteboardID string) (json.RawMessage, error) {
	url := c.BaseURL + "/api/items/" + noteboardID
	resp, err := c.HTTP.Get(url)
	if err != nil {
		return nil, fmt.Errorf("%w: GET %s: %v", ErrNoteboard, url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%w: reading GET %s: %v", ErrNoteboard, url, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: GET %s answered %d: %s",
			ErrNoteboard, url, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if !json.Valid(body) {
		return nil, fmt.Errorf("%w: GET %s answered %d bytes that are not JSON", ErrNoteboard, url, len(body))
	}
	return json.RawMessage(body), nil
}

// OpenTodoIDs returns the id of every todo noteboard currently reports as open,
// in ONE request, so a list of applications can be summarized without a call per
// linked task.
//
// Held todos are included. A hold is work parked behind a gate, not work that has
// gone away, and leaving them out would quietly undercount what is outstanding.
func (c *NoteboardClient) OpenTodoIDs() (map[string]bool, error) {
	// No limit parameter: noteboard applies a limit only when one is greater than
	// zero, so omitting it returns every open todo rather than a truncated page
	// that would silently undercount.
	url := c.BaseURL + "/api/items?type=todo&status=open&include_held=true"
	resp, err := c.HTTP.Get(url)
	if err != nil {
		return nil, fmt.Errorf("%w: GET %s: %v", ErrNoteboard, url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%w: reading GET %s: %v", ErrNoteboard, url, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: GET %s answered %d: %s",
			ErrNoteboard, url, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var items []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &items); err != nil {
		return nil, fmt.Errorf("%w: GET %s answered unreadable JSON: %v", ErrNoteboard, url, err)
	}
	open := make(map[string]bool, len(items))
	for _, item := range items {
		open[item.ID] = true
	}
	return open, nil
}

// NoteboardTodo is the todo job-store asks noteboard to create. It is a request
// shape, not a copy of noteboard's item: nothing here is ever read back or
// stored — the only thing kept from a create is the id noteboard assigns.
type NoteboardTodo struct {
	Type  string   `json:"type"`
	Title string   `json:"title"`
	Body  string   `json:"body,omitempty"`
	Tags  []string `json:"tags,omitempty"`
	DueAt string   `json:"due_at,omitempty"`
}

// CreateTodo creates a todo in noteboard and returns the id noteboard assigned.
//
// The id comes back from the owning store and is stored as-is. A local id
// invented here would point at nothing, and joining on the title instead would
// pick the wrong row the first time two applications share a company.
func (c *NoteboardClient) CreateTodo(todo NoteboardTodo) (string, error) {
	if todo.Type == "" {
		todo.Type = "todo"
	}
	payload, err := json.Marshal(todo)
	if err != nil {
		return "", fmt.Errorf("%w: encoding todo %q: %v", ErrNoteboard, todo.Title, err)
	}
	url := c.BaseURL + "/api/items"
	resp, err := c.HTTP.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("%w: POST %s: %v", ErrNoteboard, url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("%w: reading POST %s: %v", ErrNoteboard, url, err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("%w: POST %s answered %d: %s",
			ErrNoteboard, url, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		return "", fmt.Errorf("%w: POST %s answered unreadable JSON: %v", ErrNoteboard, url, err)
	}
	if created.ID == "" {
		// Without the owning store's id there is nothing to join on, and inventing
		// one here would be a reference to a row that does not exist.
		return "", fmt.Errorf("%w: POST %s created a todo with no id in the response", ErrNoteboard, url)
	}
	return created.ID, nil
}

// StandardApplicationTask is one of the follow-ups POST
// /applications/{id}/tasks/standard creates. The set is small on purpose: these
// are the three things that actually get forgotten, and a longer list would make
// the todo queue useless rather than more thorough.
type StandardApplicationTask struct {
	// TitleFormat takes the company and then the role title.
	TitleFormat string
	// DueInDays is when the todo comes due, counted from creation.
	DueInDays int
}

// StandardApplicationTasks is the set created for a new application. Exported so
// a caller — and a test — can see exactly what will land in the todo queue.
var StandardApplicationTasks = []StandardApplicationTask{
	{TitleFormat: "Follow up with %s about the %s application if no reply in 7 days", DueInDays: 7},
	{TitleFormat: "Research %s and prepare questions for the %s interview", DueInDays: 3},
	{TitleFormat: "Record the outcome of the %s %s application, or mark it ghosted", DueInDays: 30},
}

// StandardApplicationTaskTags is what every standard todo is tagged with, so the
// reminder surfaces treat them as personal work rather than coding work.
var StandardApplicationTaskTags = []string{"jobs", "personal"}
