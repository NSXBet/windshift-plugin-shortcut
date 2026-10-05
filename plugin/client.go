// Shortcut REST v3 client over the http_fetch host function.
//
// Stateless by design: a fresh WASM instance is created per invocation, so
// the rate limiter only guards bursts within one tick. 429/5xx responses
// abort the tick — state is persisted by the engine and the next scheduled
// tick retries the same window (resumable design, contract §6). There is no
// in-wasm sleep primitive and spinning CPU to honor Retry-After would waste
// the 5s invocation budget.
//
// API facts verified against the published v3 docs (2026-10-05):
//   - POST /stories/search takes filter-only body params (project_ids,
//     updated_at_start/end, includes_description, …) and returns a plain
//     [StorySlim, …] array — no pagination, no cap.
//   - GET /stories/{id}/comments returns [StoryComment, …] with a deleted
//     boolean (comment soft-deletes are traceable without webhooks).
//   - Story public_id is the plain integer id; comment ids are integers.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"shortcut/logic"
)

const (
	apiBase        = "https://api.app.shortcut.com/api/v3"
	fetchTimeoutMs = 2500 // wasm invocation cap is 5s; keep margin for state saves
	rateLimitMax   = 180
	rateLimitWin   = time.Minute
)

// errAPIStatus is a non-2xx response from the Shortcut API.
type errAPIStatus struct {
	Status  int
	Path    string
	Snippet string
}

func (e *errAPIStatus) Error() string {
	return fmt.Sprintf("shortcut %s: status %d: %s", e.Path, e.Status, e.Snippet)
}

// isNotFound reports whether err is a Shortcut 404.
func isNotFound(err error) bool {
	var e *errAPIStatus
	return errors.As(err, &e) && e.Status == 404
}

// shortcutClient issues authenticated calls to the configured organization.
type shortcutClient struct {
	token      string
	projectIDs []int64
	limiter    *logic.RateLimiter
	now        func() time.Time
}

func newShortcutClient(cfg *Config) *shortcutClient {
	return &shortcutClient{
		token:      cfg.Token,
		projectIDs: cfg.ProjectIDs,
		limiter:    logic.NewRateLimiter(rateLimitMax, rateLimitWin),
		now:        time.Now,
	}
}

func (c *shortcutClient) do(method, path string, body []byte) ([]byte, error) {
	if !c.limiter.Allow(c.now()) {
		return nil, fmt.Errorf("shortcut %s: local rate budget exhausted (%d/%s)", path, rateLimitMax, rateLimitWin)
	}
	req := httpFetchRequest{
		Method:    method,
		URL:       apiBase + path,
		Headers:   map[string]string{"Shortcut-Token": c.token, "Content-Type": "application/json"},
		TimeoutMs: fetchTimeoutMs,
	}
	if body != nil {
		req.Body = body
	}
	resp, err := httpFetch(req)
	if err != nil {
		return nil, err
	}
	if resp.Status >= 400 {
		return nil, &errAPIStatus{Status: resp.Status, Path: path, Snippet: truncateSnippet(string(resp.Body), 200)}
	}
	return resp.Body, nil
}

// --- stories ---

type searchStoriesReq struct {
	ProjectIDs          []int64 `json:"project_ids,omitempty"`
	UpdatedAtStart      string  `json:"updated_at_start,omitempty"` // RFC3339 UTC
	UpdatedAtEnd        string  `json:"updated_at_end,omitempty"`
	IncludesDescription bool    `json:"includes_description"`
}

// story is the subset of Shortcut StorySlim/Story the sync maps. Nullable
// Shortcut fields use pointers: epic_id/estimate/deadline are absent-or-null.
type story struct {
	ID              int64      `json:"id"`
	Name            string     `json:"name"`
	Description     string     `json:"description"`
	StoryType       string     `json:"story_type"`
	WorkflowStateID int64      `json:"workflow_state_id"`
	ProjectID       int64      `json:"project_id"`
	EpicID          *int64     `json:"epic_id"`
	Estimate        *float64   `json:"estimate"`
	Deadline        *string    `json:"deadline"`
	Archived        bool       `json:"archived"`
	AppURL          string     `json:"app_url"`
	CreatedAt       string     `json:"created_at"`
	UpdatedAt       string     `json:"updated_at"`
	Labels          []labelRef `json:"labels"`
	ExternalID      string     `json:"external_id"`
}

func (s *story) labelNames() []string {
	names := make([]string, 0, len(s.Labels))
	for _, l := range s.Labels {
		names = append(names, l.Name)
	}
	return names
}

func (c *shortcutClient) searchStories(start, end time.Time) ([]story, error) {
	body, err := json.Marshal(searchStoriesReq{
		ProjectIDs:          c.projectIDs,
		UpdatedAtStart:      start.UTC().Format(time.RFC3339),
		UpdatedAtEnd:        end.UTC().Format(time.RFC3339),
		IncludesDescription: true,
	})
	if err != nil {
		return nil, err
	}
	raw, err := c.do("POST", "/stories/search", body)
	if err != nil {
		return nil, err
	}
	var out []story
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("stories/search: %w", err)
	}
	return out, nil
}

func (c *shortcutClient) getStory(id int64) (*story, error) {
	raw, err := c.do("GET", "/stories/"+strconv.FormatInt(id, 10), nil)
	if err != nil {
		return nil, err
	}
	var s story
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("story %d: %w", id, err)
	}
	return &s, nil
}

// --- comments ---

type storyComment struct {
	ID        int64  `json:"id"`
	StoryID   int64  `json:"story_id"`
	Text      string `json:"text"`
	AuthorID  string `json:"author_id"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	Deleted   bool   `json:"deleted"`
	Position  int    `json:"position"`
}

func (c *shortcutClient) listStoryComments(storyID int64) ([]storyComment, error) {
	raw, err := c.do("GET", "/stories/"+strconv.FormatInt(storyID, 10)+"/comments", nil)
	if err != nil {
		return nil, err
	}
	var out []storyComment
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("comments story %d: %w", storyID, err)
	}
	return out, nil
}

// --- catalog (full lists; container endpoints are unpaged) ---

type labelRef struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type project struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Archived bool   `json:"archived"`
}

type epic struct {
	ID          int64      `json:"id"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Archived    bool       `json:"archived"`
	AppURL      string     `json:"app_url"`
	CreatedAt   string     `json:"created_at"`
	UpdatedAt   string     `json:"updated_at"`
	CompletedAt *string    `json:"completed_at"`
	EpicStateID int64      `json:"epic_state_id"`
	Labels      []labelRef `json:"labels"`
	ExternalID  string     `json:"external_id"`
}

type workflowState struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"` // "backlog" | "unstarted" | "started" | "done"
}

type workflow struct {
	ID     int64           `json:"id"`
	Name   string          `json:"name"`
	States []workflowState `json:"states"`
}

// fetchList GETs a full-list endpoint and decodes a JSON array.
func fetchList[T any](c *shortcutClient, path string) ([]T, error) {
	raw, err := c.do("GET", path, nil)
	if err != nil {
		return nil, err
	}
	var out []T
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return out, nil
}

func (c *shortcutClient) listProjects() ([]project, error) { return fetchList[project](c, "/projects") }
func (c *shortcutClient) listEpics() ([]epic, error)       { return fetchList[epic](c, "/epics") }
func (c *shortcutClient) listWorkflows() ([]workflow, error) {
	return fetchList[workflow](c, "/workflows")
}
func (c *shortcutClient) listEpicWorkflows() ([]workflow, error) {
	return fetchList[workflow](c, "/epic-workflows")
}
