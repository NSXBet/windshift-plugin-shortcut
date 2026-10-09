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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"shortcut/logic"
)

const (
	apiBase        = "https://api.app.shortcut.com/api/v3"
	fetchTimeoutMs = 3500 // wasm invocation cap is 5s, tick budget 4.2s; live API calls measured 0.2–2.5s
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
	// remainingMs reports the milliseconds left in the tick's wasm budget;
	// nil outside a sync tick (webhook path), meaning the fixed cap applies.
	remainingMs func() int64
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
		TimeoutMs: c.fetchTimeoutMs(),
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

const minFetchTimeoutMs = 600 // below this a fetch is more likely to starve the save than succeed

// fetchTimeoutMs is min(fetchTimeoutMs, remaining tick budget - margin).
// An unclamped 3.5s fetch started late in a tick outlives the 5s wasm cap
// and the module is killed ("module closed with context deadline exceeded").
func (c *shortcutClient) fetchTimeoutMs() int {
	if c.remainingMs == nil {
		return fetchTimeoutMs
	}
	// 900ms margin: state save + tick teardown after the fetch completes.
	r := c.remainingMs() - 900
	if r < minFetchTimeoutMs {
		return minFetchTimeoutMs
	}
	if r < fetchTimeoutMs {
		return int(r)
	}
	return fetchTimeoutMs
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
	OwnerIDs        []string   `json:"owner_ids"`    // Shortcut carries owner_ids (UUID strings); windshift's column takes one
	IterationID     *int64     `json:"iteration_id"` // the story's sprint (workspace-scoped on import)
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

// storyMeta is the frozen per-window slice of a story: everything the pass
// needs to decide skip-vs-work (ShouldSkip compares UpdatedAt) plus the ids
// the comment pass and sweep iterate on. Bodies (name/description) are
// fetched on demand only for stories that actually need an upsert, so a
// resumed window costs no Shortcut traffic beyond real work. Short keys —
// the list lives in the plugin KV state, ~24 bytes per story.
type storyMeta struct {
	ID              int64  `json:"i"`
	UpdatedAt       string `json:"u"`
	EpicID          *int64 `json:"e,omitempty"`
	ProjectID       int64  `json:"p"`
	WorkflowStateID int64  `json:"w"`
}

func (c *shortcutClient) searchStories(start, end time.Time) ([]story, error) {
	body, err := json.Marshal(searchStoriesReq{
		ProjectIDs:     c.projectIDs,
		UpdatedAtStart: start.UTC().Format(time.RFC3339),
		UpdatedAtEnd:   end.UTC().Format(time.RFC3339),
		// Descriptions stay out: the condensed window keeps only id+stamp and
		// syncStory fetches full bodies lazily — carrying every description
		// made the NSX window search ~1.8 MB, which Shortcut serves slow
		// enough to blow the wasm fetch cap.
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

func (c *shortcutClient) getEpic(id int64) (*epic, error) {
	raw, err := c.do("GET", "/epics/"+strconv.FormatInt(id, 10), nil)
	if err != nil {
		return nil, err
	}
	var ep epic
	if err := json.Unmarshal(raw, &ep); err != nil {
		return nil, fmt.Errorf("epic %d: %w", id, err)
	}
	return &ep, nil
}

// shortcutMember is the GET /members/{id} body subset the import needs. The
// email is present only for members who exposed it; a member without one
// imports with the synthetic sync address.
type shortcutMember struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Name     string `json:"name"`
	Email    string `json:"email"`
}

func (c *shortcutClient) getMember(id string) (*shortcutMember, error) {
	raw, err := c.do("GET", "/members/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, err
	}
	var m shortcutMember
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("member %s: %w", id, err)
	}
	return &m, nil
}

// shortcutIteration is the GET /iterations/{id} body subset. Dates are
// Shortcut's YYYY-MM-DD; state is unstarted/started/done.
type shortcutIteration struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	StartDate string `json:"start_date"`
	EndDate   string `json:"end_date"`
	State     string `json:"state"`
}

func (c *shortcutClient) getIteration(id int64) (*shortcutIteration, error) {
	raw, err := c.do("GET", "/iterations/"+strconv.FormatInt(id, 10), nil)
	if err != nil {
		return nil, err
	}
	var it shortcutIteration
	if err := json.Unmarshal(raw, &it); err != nil {
		return nil, fmt.Errorf("iteration %d: %w", id, err)
	}
	return &it, nil
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
	TeamID   *int64 `json:"team_id"` // owning team; null for teamless (solo) projects
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
	Deadline    *string    `json:"deadline"`
	EpicStateID int64      `json:"epic_state_id"`
	TeamID      *int64     `json:"team_id"` // owning team; null for teamless epics
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
	TeamID *int64          `json:"team_id"` // owning Shortcut team; null = unassigned
	States []workflowState `json:"states"`
}

// team is one Shortcut team (GET /teams). The payload embeds the team's
// workflow, so one fetch carries both the team identity and the states its
// board should show.
type team struct {
	ID       int64           `json:"id"`
	Name     string          `json:"name"`
	Archived bool            `json:"archived"`
	Workflow *workflow       `json:"workflow"`
	States   []workflowState `json:"-"` // flattened from Workflow at load (maps cache)
}

// eachList GETs a full-list endpoint and hands each decoded element to fn.
// The NSX org's /epics payload is ~4 MB; a bulk decode of a typed slice
// inside the 64 MiB wasm ceiling exhausts the module (observed as "module
// closed with context deadline exceeded" before the first syncEpics log
// line). Element-at-a-time keeps peak memory flat.
func (c *shortcutClient) eachList(path string, fn func(dec *json.Decoder) error) error {
	raw, err := c.do("GET", path, nil)
	logInfo("eachList " + path + ": fetched " + strconv.Itoa(len(raw)) + " bytes")
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return fmt.Errorf("%s: expected JSON array, got %v", path, tok)
	}
	for dec.More() {
		if err := fn(dec); err != nil {
			return err
		}
	}
	logInfo("eachList " + path + ": decode done")
	return nil
}
func decodeList[T any](c *shortcutClient, path string) ([]T, error) {
	var out []T
	err := c.eachList(path, func(dec *json.Decoder) error {
		var item T
		if err := dec.Decode(&item); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		out = append(out, item)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *shortcutClient) listProjects() ([]project, error) {
	return decodeList[project](c, "/projects")
}
func (c *shortcutClient) listWorkflows() ([]workflow, error) {
	return decodeList[workflow](c, "/workflows")
}

// listTeams fetches every team with its embedded workflow. The org's teams
// are the real navigation unit (projects are unused); each team carries the
// states its workspace board should show.
func (c *shortcutClient) listTeams() ([]team, error) {
	return decodeList[team](c, "/teams")
}

// epicMeta is the catalog-freeze slice of an epic: everything the verify
// pass needs to decide skip-vs-work (ShouldSkip compares UpdatedAt); the
// full body is fetched lazily only for changed or new epics.
type epicMeta struct {
	ID        int64  `json:"i"`
	UpdatedAt string `json:"u"`
}

// collectEpicMetas streams /epics once per call, discarding the processed
// prefix and handing (id, updated_at) for the rest. The caller persists
// the metas across ticks; once the stream is exhausted the catalog runs
// entirely from state.
func (c *shortcutClient) collectEpicMetas(skip int, fn func(id int64, updatedAt string) error) error {
	i := 0
	return c.eachList("/epics", func(dec *json.Decoder) error {
		if i < skip {
			// Already-collected prefix: scan past the token without
			// building the struct.
			var discard json.RawMessage
			if err := dec.Decode(&discard); err != nil {
				return fmt.Errorf("/epics: %w", err)
			}
			i++
			return nil
		}
		// Decode the real stream fields — the condensed epicMeta's i/u
		// tags are the persistence shape, not the wire shape.
		var raw struct {
			ID        int64  `json:"id"`
			UpdatedAt string `json:"updated_at"`
		}
		if err := dec.Decode(&raw); err != nil {
			return fmt.Errorf("/epics: %w", err)
		}
		i++
		return fn(raw.ID, raw.UpdatedAt)
	})
}

// listEpicWorkflows 404s on workspaces with no epic workflows (observed on
// the live API) — an empty list is the right answer, epic status names just
// resolve through the fallback path.
func (c *shortcutClient) listEpicWorkflows() ([]workflow, error) {
	wfs, err := decodeList[workflow](c, "/epic-workflows")
	if err != nil && strings.Contains(err.Error(), "status 404") {
		return nil, nil
	}
	return wfs, err
}
