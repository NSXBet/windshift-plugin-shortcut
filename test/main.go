// Host-side smoke test: loads dist/plugin.wasm through the same runtime
// stack windshift uses (github.com/extism/go-sdk on wazero) and exercises
// every route with an in-memory mock of the kv_get/kv_set host functions,
// mirroring windshift's extism:host/user pointer ABI (JSON payload at
// stack[0] offset in, JSON response ptr out).
package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	extism "github.com/extism/go-sdk"
)

var kv = map[string]string{}

type kvGetReq struct {
	Key string `json:"key"`
}
type kvGetResp struct {
	Status string `json:"status"`
	Value  string `json:"value,omitempty"`
}
type kvSetReq struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}
type kvSetResp struct {
	Status string `json:"status"`
}

func kvGet(_ context.Context, p *extism.CurrentPlugin, stack []uint64) {
	raw, err := p.ReadBytes(stack[0])
	if err != nil {
		stack[0] = 0
		return
	}
	var req kvGetReq
	if json.Unmarshal(raw, &req) != nil {
		stack[0] = 0
		return
	}
	val, ok := kv[req.Key]
	status := "ok"
	if !ok {
		status = "not_found"
	}
	resp, _ := json.Marshal(kvGetResp{Status: status, Value: val})
	ptr, err := p.WriteBytes(resp)
	if err != nil {
		stack[0] = 0
		return
	}
	stack[0] = ptr
}

func kvSet(_ context.Context, p *extism.CurrentPlugin, stack []uint64) {
	raw, err := p.ReadBytes(stack[0])
	if err != nil {
		stack[0] = 0
		return
	}
	var req kvSetReq
	if json.Unmarshal(raw, &req) != nil {
		stack[0] = 0
		return
	}
	kv[req.Key] = req.Value
	resp, _ := json.Marshal(kvSetResp{Status: "ok"})
	ptr, err := p.WriteBytes(resp)
	if err != nil {
		stack[0] = 0
		return
	}
	stack[0] = ptr
}

// --- functional host mocks ---
//
// The same mocks serve the route checks and the sync_tick E2E:
// http_fetch answers canned Shortcut v3 payloads, item_upsert/item_lookup
// maintain an in-memory external_id → item map, create_comment records
// calls. The recorders are reset by runSyncE2E.

func writeJSON(p *extism.CurrentPlugin, stack []uint64, v any) {
	resp, _ := json.Marshal(v)
	ptr, err := p.WriteBytes(resp)
	if err != nil {
		stack[0] = 0
		return
	}
	stack[0] = ptr
}

// kvDelete acks; no test deletes.
func kvDelete(_ context.Context, p *extism.CurrentPlugin, stack []uint64) {
	writeJSON(p, stack, map[string]string{"status": "ok"})
}

type httpFetchReq struct {
	Method    string            `json:"method"`
	URL       string            `json:"url"`
	Headers   map[string]string `json:"headers,omitempty"`
	Body      []byte            `json:"body,omitempty"`
	TimeoutMs int               `json:"timeout_ms,omitempty"`
}

type httpFetchResp struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    []byte            `json:"body,omitempty"`
}

func httpFetch(_ context.Context, p *extism.CurrentPlugin, stack []uint64) {
	raw, err := p.ReadBytes(stack[0])
	if err != nil {
		stack[0] = 0
		return
	}
	var req httpFetchReq
	if json.Unmarshal(raw, &req) != nil {
		writeJSON(p, stack, httpFetchResp{Status: 400, Body: []byte("bad http_fetch request")})
		return
	}
	writeJSON(p, stack, mockShortcutAPI(req))
}

// Swappable mock bodies. Defaults keep the single-mode fixtures unchanged;
// the per-project E2E installs extra projects/search JSON and restores the
// defaults afterwards.
var (
	defaultProjectsBody  = `[{"id":5,"name":"Demo","archived":false}]`
	defaultSearchBody    = searchStoriesJSON()
	defaultWorkflowsBody = `[{"id":1,"name":"Product","states":[{"id":11,"name":"In Progress","type":"started"}]}]`
	defaultTeamsBody     = `[]`
	projectsBody         = defaultProjectsBody
	searchBody           = defaultSearchBody
	workflowsBody        = defaultWorkflowsBody
	teamsBody            = defaultTeamsBody
	membersBody          = `[]`                // dump's /members list pass
	epicsBody            = `[]`                // dump's /epics list pass
	searchWindowFilter   = false               // /stories/search honors updated_at window + archived pass
	epicComments         = map[string]string{} // /epics/{id}/comments per epic id
)

// perProjectStoriesJSON is a search payload spanning all three workspace
// resolution paths: 101 → project 5 (workspace created), 102 → project 999
// (absent from /projects, fallback workspace), 103 → project 7, whose
// workspace_upsert the mock rejects.
func perProjectStoriesJSON() string {
	at := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
	// Story 101's name is over the core's 255-rune title cap and multi-line —
	// the E2E asserts the sanitized cut title plus the full original preserved
	// as a markdown heading atop the description.
	first := "First\r\n" + strings.Repeat("F", 300)
	return fmt.Sprintf(`[
		{"id":101,"name":%[2]q,"description":"","story_type":"feature","workflow_state_id":11,"project_id":5,
			"app_url":"https://app.shortcut.com/demo/story/101","created_at":%[1]q,"updated_at":%[1]q},
		{"id":102,"name":"Second","description":"","story_type":"bug","workflow_state_id":11,"project_id":999,
			"app_url":"https://app.shortcut.com/demo/story/102","created_at":%[1]q,"updated_at":%[1]q},
		{"id":103,"name":"Third","description":"","story_type":"chore","workflow_state_id":11,"project_id":7,
			"app_url":"https://app.shortcut.com/demo/story/103","created_at":%[1]q,"updated_at":%[1]q},
		{"id":104,"name":"Fourth","description":"","story_type":"feature","workflow_state_id":11,"project_id":5,"epic_id":5001,
			"owner_ids":["687a7ce8-ae4b-4528-975d-bfe3fdc8bca8"],"iteration_id":4242,
			"app_url":"https://app.shortcut.com/demo/story/104","created_at":%[1]q,"updated_at":%[1]q},
		{"id":105,"name":"Fifth","description":"","story_type":"bug","workflow_state_id":11,"project_id":5,"epic_id":6001,
			"app_url":"https://app.shortcut.com/demo/story/105","created_at":%[1]q,"updated_at":%[1]q}
	]`, at, first)
}

// mockShortcutAPI answers the endpoints the sync engine calls. Anything
// unmocked returns 404 so a guest drift becomes a visible error, not silence.
func mockShortcutAPI(req httpFetchReq) httpFetchResp {
	const base = "https://api.app.shortcut.com/api/v3"
	path := strings.TrimPrefix(req.URL, base)
	body := func(s string) httpFetchResp {
		return httpFetchResp{Status: 200, Headers: map[string]string{"Content-Type": "application/json"}, Body: []byte(s)}
	}
	switch {
	case path == "/workflows":
		return body(workflowsBody)
	case path == "/teams":
		return body(teamsBody)
	case path == "/epic-workflows":
		return body(`[{"id":2,"name":"Epic WF","states":[{"id":21,"name":"Prioritized","type":"todo"}]}]`)
	case path == "/projects":
		return body(projectsBody)
	case path == "/members":
		return body(membersBody)
	case path == "/epics":
		if s := os.Getenv("BIG_EPICS"); s != "" {
			n, _ := strconv.Atoi(s)
			return body(bigEpicsJSON(n))
		}
		return body(epicsBody)
	case strings.HasPrefix(path, "/epics/") && strings.HasSuffix(path, "/comments"):
		if b, ok := epicComments[strings.TrimSuffix(strings.TrimPrefix(path, "/epics/"), "/comments")]; ok {
			return body(b)
		}
		return body(`[]`)
	case strings.HasPrefix(path, "/epics/"):
		id := strings.TrimPrefix(path, "/epics/")
		if id == "5001" {
			return body(`{"id":5001,"name":"Parent Epic","description":"","app_url":"https://app.shortcut.com/demo/epic/5001","epic_state_id":21,"created_at":"2026-10-08T09:05:06Z","updated_at":"2026-10-08T09:05:06Z"}`)
		}
		// Big-epics flow (BIG_EPICS env): the catalog freeze collects
		// (id, updated_at) metas from the stream and fetches bodies
		// lazily per id, so the mock serves a body for any numeric epic
		// id in that mode. Without it, unknown epics 404 (the
		// parentless-import case relies on that).
		if s := os.Getenv("BIG_EPICS"); s != "" {
			if n, err := strconv.Atoi(id); err == nil {
				return body(fmt.Sprintf(`{"id":%d,"name":"Epic %d","description":"","app_url":"https://app.shortcut.com/demo/epic/%d","epic_state_id":21,"created_at":"2021-01-01T00:00:00Z","updated_at":"2021-01-02T00:00:00Z"}`, n, n-1000, n))
			}
		}
		return httpFetchResp{Status: 404, Body: []byte(`{"message":"epic not found"}`)}
	case strings.HasPrefix(path, "/members/"):
		if strings.TrimPrefix(path, "/members/") == "687a7ce8-ae4b-4528-975d-bfe3fdc8bca8" {
			// Real member wire shape: identity lives in `profile`
			// (wire-checked 2026-10-09); top-level name/email are absent.
			return body(`{"id":"687a7ce8-ae4b-4528-975d-bfe3fdc8bca8","state":"full","disabled":false,"profile":{"entity_type":"profile","mention_name":"dev.yuri","name":"Dev Yuri","email_address":"yuri@nsx.dev"}}`)
		}
		return httpFetchResp{Status: 404, Body: []byte(`{"message":"member not found"}`)}
	case strings.HasPrefix(path, "/iterations/"):
		if strings.TrimPrefix(path, "/iterations/") == "4242" {
			return body(`{"id":4242,"name":"Sprint 42","start_date":"2026-10-06","end_date":"2026-10-20","state":"unstarted"}`)
		}
		return httpFetchResp{Status: 404, Body: []byte(`{"message":"iteration not found"}`)}
	case path == "/stories/search":
		return body(filterSearchByWindow(searchBody, req))
	case strings.HasPrefix(path, "/stories/") && strings.HasSuffix(path, "/comments"):
		switch strings.TrimSuffix(strings.TrimPrefix(path, "/stories/"), "/comments") {
		case "101":
			return body(`[
				{"id":9001,"story_id":101,"text":"hello","author_id":"77","created_at":"2026-10-01T10:00:00Z","updated_at":"2026-10-01T10:00:00Z","deleted":false,"position":0},
				{"id":9002,"story_id":101,"text":"gone","author_id":"77","created_at":"2026-10-01T11:00:00Z","updated_at":"2026-10-01T11:00:00Z","deleted":true,"position":1}
			]`)
		case "102", "104", "105", "202", "203":
			return body(`[]`)
		case "201":
			return body(`[
				{"id":9101,"story_id":201,"text":"dump comment one","author_id":"77","created_at":"2021-10-05T11:00:00Z","updated_at":"2021-10-05T11:00:00Z","deleted":false,"position":0},
				{"id":9102,"story_id":201,"text":"dump comment two","author_id":"77","created_at":"2021-10-05T12:00:00Z","updated_at":"2021-10-05T12:00:00Z","deleted":false,"position":1},
				{"id":9103,"story_id":201,"text":"deleted dump comment","author_id":"77","created_at":"2021-10-05T13:00:00Z","updated_at":"2021-10-05T13:00:00Z","deleted":true,"position":2}
			]`)
		}
		return httpFetchResp{Status: 404, Body: []byte(`{"message":"story not found"}`)}
	case strings.HasPrefix(path, "/stories/"):
		id := strings.TrimPrefix(path, "/stories/")
		getStoryCalls = append(getStoryCalls, id)
		if deletedStories[id] {
			return httpFetchResp{Status: 404, Body: []byte(`{"message":"story not found"}`)}
		}
		// The lazy body fetch (getStory → upsert) must see the same fields
		// the search window returned, so serve the canonical story out of
		// the active search fixture instead of a fabricated body.
		var stories []map[string]any
		if err := json.Unmarshal([]byte(searchBody), &stories); err == nil {
			for _, s := range stories {
				if fmt.Sprint(s["id"]) == id {
					raw, _ := json.Marshal(s)
					return body(string(raw))
				}
			}
		}
		return httpFetchResp{Status: 404, Body: []byte(`{"message":"story not found"}`)}
	}
	return httpFetchResp{Status: 404, Body: []byte(`{"message":"unmocked: ` + req.URL + `"}`)}
}

// filterSearchByWindow applies the dump's window + archived-pass filters to
// the fixture stories, mirroring the real POST /stories/search contract
// (inclusive start, exclusive end). Off by default: the legacy fixtures
// stamp stories at realNow-2h and their tests assert on the full body
// regardless of any window.
func filterSearchByWindow(bodyJSON string, req httpFetchReq) string {
	if !searchWindowFilter {
		return bodyJSON
	}
	var q struct {
		UpdatedAtStart string `json:"updated_at_start"`
		UpdatedAtEnd   string `json:"updated_at_end"`
		Archived       bool   `json:"archived"`
	}
	if err := json.Unmarshal([]byte(req.Body), &q); err != nil || q.UpdatedAtStart == "" || q.UpdatedAtEnd == "" {
		return bodyJSON
	}
	start, err1 := time.Parse(time.RFC3339, q.UpdatedAtStart)
	end, err2 := time.Parse(time.RFC3339, q.UpdatedAtEnd)
	if err1 != nil || err2 != nil {
		return bodyJSON
	}
	var all []map[string]any
	if err := json.Unmarshal([]byte(bodyJSON), &all); err != nil {
		return bodyJSON
	}
	out := make([]map[string]any, 0, len(all))
	for _, s := range all {
		archived, _ := s["archived"].(bool)
		if archived != q.Archived {
			continue
		}
		ts, _ := s["updated_at"].(string)
		t, err := time.Parse(time.RFC3339, ts)
		if err != nil || t.Before(start) || !t.Before(end) {
			continue
		}
		out = append(out, s)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return bodyJSON
	}
	return string(raw)
}

// dumpStoriesJSON is the dump E2E's search fixture: stories stamped inside
// the guest-clock range the dump tiles ([2000-01-01, 2021-12-05)), so the
// window-aware mock serves them on exactly one window. 201 carries the full
// mapping surface (epic, sprint, assignee, labels, estimate, deadline);
// 202 is bare; 203 is archived and only served on the archived pass.
func dumpStoriesJSON() string {
	return `[
		{"id":201,"name":"Dump Alpha","description":"alpha body","story_type":"feature","workflow_state_id":11,"project_id":5,"epic_id":6001,
		 "owner_ids":["687a7ce8-ae4b-4528-975d-bfe3fdc8bca8"],"iteration_id":4242,"estimate":3,"deadline":"2021-11-01T00:00:00Z",
		 "labels":[{"name":"core","id":51}],"archived":false,
		 "app_url":"https://app.shortcut.com/demo/story/201","created_at":"2021-10-05T09:00:00Z","updated_at":"2021-10-05T10:00:00Z"},
		{"id":202,"name":"Dump Beta","description":"","story_type":"bug","workflow_state_id":11,"project_id":5,"archived":false,
		 "app_url":"https://app.shortcut.com/demo/story/202","created_at":"2021-10-05T09:30:00Z","updated_at":"2021-10-05T10:00:00Z"},
		{"id":203,"name":"Dump Archived","description":"archived body","story_type":"chore","workflow_state_id":11,"project_id":5,"epic_id":6001,
		 "archived":true,"app_url":"https://app.shortcut.com/demo/story/203","created_at":"2021-10-05T09:45:00Z","updated_at":"2021-10-05T10:00:00Z"}
	]`
}

// bigEpicsJSON emits n realistic epic objects (~2KB each) for reproducing
// the live NSX org's ~4MB /epics payload in harness runs.
func bigEpicsJSON(n int) string {
	var b strings.Builder
	b.WriteByte('[')
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"id":%d,"name":"Epic %d","description":%q,"archived":false,
		"app_url":"https://app.shortcut.com/demo/epic/%d","created_at":"2021-01-01T00:00:00Z",
		"updated_at":"2021-01-02T00:00:00Z","epic_state_id":%d,"team_id":null,
		"labels":[],"external_id":""}`,
			1000+i, i, strings.Repeat("lorem ipsum dolor sit amet ", 70), 1000+i, 100+i)
	}
	b.WriteByte(']')
	return b.String()
}

// searchStoriesJSON returns two stories updated two hours ago — inside any
// backfill window the engine opens: one with comments, one without.
func searchStoriesJSON() string {
	at := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
	return fmt.Sprintf(`[
		{"id":101,"name":"First","description":"first story","story_type":"feature","workflow_state_id":11,"project_id":5,
		 "app_url":"https://app.shortcut.com/demo/story/101","created_at":%[1]q,"updated_at":%[1]q,"labels":[],"external_id":""},
		{"id":102,"name":"Second","description":"","story_type":"bug","workflow_state_id":11,"project_id":5,
		 "app_url":"https://app.shortcut.com/demo/story/102","created_at":%[1]q,"updated_at":%[1]q,"labels":[],"external_id":""}
	]`, at)
}

// deletedStories and getStoryCalls back the end-of-window deletion sweep
// test (tk-thf): ids placed here 404 on canonical GET, every sweep GET is
// recorded so the test can assert tombstoned ids stop being fetched.
var (
	deletedStories = map[string]bool{}
	getStoryCalls  []string
)

func countStoryGets(id string) int {
	n := 0
	for _, g := range getStoryCalls {
		if g == id {
			n++
		}
	}
	return n
}

// --- item_upsert / item_lookup / create_comment wire (contract §1) ---

type itemUpsertReq struct {
	ExternalKind        string `json:"external_kind"`
	ExternalID          int64  `json:"external_id"`
	ExternalURL         string `json:"external_url,omitempty"`
	ExternalUpdatedAt   string `json:"external_updated_at,omitempty"`
	WorkspaceID         string `json:"workspace_id"`
	Title               string `json:"title"`
	Description         string `json:"description"`
	ParentExternalKind  string `json:"parent_external_kind,omitempty"`
	ParentExternalID    int64  `json:"parent_external_id,omitempty"`
	AssigneeExternalID  string `json:"assignee_external_id,omitempty"`
	IterationExternalID int64  `json:"iteration_external_id,omitempty"`
	StatusName          string `json:"status_name"`
	ItemTypeName        string `json:"item_type_name"`
}

type itemUpsertResp struct {
	Status  string `json:"status"`
	ItemID  string `json:"item_id,omitempty"`
	ItemKey string `json:"item_key,omitempty"`
	Created bool   `json:"created,omitempty"`
	Error   string `json:"error,omitempty"`
}

type itemLookupReq struct {
	ExternalKind string `json:"external_kind"`
	ExternalID   int64  `json:"external_id"`
}

type itemLookupResp struct {
	Status            string `json:"status"`
	Found             bool   `json:"found,omitempty"`
	ItemID            string `json:"item_id,omitempty"`
	ItemKey           string `json:"item_key,omitempty"`
	ExternalUpdatedAt string `json:"external_updated_at,omitempty"`
	LastSyncedAt      string `json:"last_synced_at,omitempty"`
	Error             string `json:"error,omitempty"`
}

type createCommentReq struct {
	ItemID                int    `json:"item_id"`
	AuthorID              int    `json:"author_id"`
	Content               string `json:"content"`
	SuppressNotifications bool   `json:"suppress_notifications"`
}

type createCommentResp struct {
	Status    string `json:"status"`
	CommentID int    `json:"comment_id,omitempty"`
	Error     string `json:"error,omitempty"`
}

type mockItem struct {
	req itemUpsertReq
	id  int
}

var (
	items        = map[string]mockItem{} // "story/101" → last upserted item
	itemSeq      int
	upsertCalls  []itemUpsertReq
	commentCalls []createCommentReq
	commentSeq   int
)

func itemUpsert(_ context.Context, p *extism.CurrentPlugin, stack []uint64) {
	raw, err := p.ReadBytes(stack[0])
	if err != nil {
		stack[0] = 0
		return
	}
	var req itemUpsertReq
	if json.Unmarshal(raw, &req) != nil {
		writeJSON(p, stack, itemUpsertResp{Status: "error", Error: "bad item_upsert request"})
		return
	}
	upsertCalls = append(upsertCalls, req)
	itemSeq++
	items[req.ExternalKind+"/"+strconv.FormatInt(req.ExternalID, 10)] = mockItem{req: req, id: itemSeq}
	writeJSON(p, stack, itemUpsertResp{Status: "ok", ItemID: strconv.Itoa(itemSeq), ItemKey: fmt.Sprintf("WI-%d", itemSeq), Created: true})
}

func itemLookup(_ context.Context, p *extism.CurrentPlugin, stack []uint64) {
	raw, err := p.ReadBytes(stack[0])
	if err != nil {
		stack[0] = 0
		return
	}
	var req itemLookupReq
	if json.Unmarshal(raw, &req) != nil {
		writeJSON(p, stack, itemLookupResp{Status: "error", Error: "bad item_lookup request"})
		return
	}
	it, ok := items[req.ExternalKind+"/"+strconv.FormatInt(req.ExternalID, 10)]
	if !ok {
		writeJSON(p, stack, itemLookupResp{Status: "ok"})
		return
	}
	writeJSON(p, stack, itemLookupResp{
		Status:            "ok",
		Found:             true,
		ItemID:            strconv.Itoa(it.id),
		ItemKey:           fmt.Sprintf("WI-%d", it.id),
		ExternalUpdatedAt: it.req.ExternalUpdatedAt,
		LastSyncedAt:      it.req.ExternalUpdatedAt,
	})
}

func createComment(_ context.Context, p *extism.CurrentPlugin, stack []uint64) {
	raw, err := p.ReadBytes(stack[0])
	if err != nil {
		stack[0] = 0
		return
	}
	var req createCommentReq
	if json.Unmarshal(raw, &req) != nil {
		writeJSON(p, stack, createCommentResp{Status: "error", Error: "bad create_comment request"})
		return
	}
	commentSeq++
	commentCalls = append(commentCalls, req)
	writeJSON(p, stack, createCommentResp{Status: "ok", CommentID: commentSeq})
}

// --- workspace_upsert wire (contract §1) ---

type workspaceUpsertState struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type workspaceUpsertReq struct {
	ExternalID   int64                  `json:"external_id"`
	ExternalKind string                 `json:"external_kind"`
	Name         string                 `json:"name"`
	CreatorID    int                    `json:"creator_id,omitempty"`
	States       []workspaceUpsertState `json:"states,omitempty"`
}

type workspaceUpsertResp struct {
	Status      string `json:"status"`
	WorkspaceID string `json:"workspace_id,omitempty"`
	Created     bool   `json:"created,omitempty"`
	Error       string `json:"error,omitempty"`
}

type mockWorkspace struct {
	wsID    string
	created bool
}

var (
	wsByProject = map[int64]mockWorkspace{} // project id → workspace
	wsCalls     []workspaceUpsertReq
	wsCreated   int
)

// workspaceUpsert mirrors the core host function: find-or-create by
// external_id. Project 7 ("Owned") is always rejected — the strict
// per-project case asserts the engine treats the rejection as an item-level
// error.
func workspaceUpsert(_ context.Context, p *extism.CurrentPlugin, stack []uint64) {
	raw, err := p.ReadBytes(stack[0])
	if err != nil {
		stack[0] = 0
		return
	}
	var req workspaceUpsertReq
	resp := workspaceUpsertResp{}
	if json.Unmarshal(raw, &req) != nil {
		resp = workspaceUpsertResp{Status: "error", Error: "bad workspace_upsert request"}
	} else {
		wsCalls = append(wsCalls, req)
		if req.ExternalID == 7 {
			resp = workspaceUpsertResp{Status: "error", Error: "project 7 rejected by core"}
		} else if m, ok := wsByProject[req.ExternalID]; ok {
			resp = workspaceUpsertResp{Status: "ok", WorkspaceID: m.wsID, Created: m.created}
		} else {
			wsCreated++
			m := mockWorkspace{wsID: fmt.Sprintf("ws-%d", req.ExternalID), created: true}
			wsByProject[req.ExternalID] = m
			resp = workspaceUpsertResp{Status: "ok", WorkspaceID: m.wsID, Created: m.created}
		}
	}
	writeJSON(p, stack, resp)
}

// purgeSync mirrors the core PurgeSync: every mapped item is deleted and the
// plugin's KV state (cursors, comment dedup, tombstones) is cleared. The
// config survives — the replay reuses the operator's settings.
func purgeSync(_ context.Context, p *extism.CurrentPlugin, stack []uint64) {
	deleted := len(items)
	items = map[string]mockItem{}
	itemSeq, commentSeq = 0, 0
	for k := range kv {
		if k == "shortcut:config" {
			continue
		}
		if strings.HasPrefix(k, "shortcut:") {
			delete(kv, k)
		}
	}
	wsByProject = map[int64]mockWorkspace{}
	writeJSON(p, stack, map[string]any{"status": "ok", "items": deleted, "kv": 0})
}

// --- member_upsert / iteration_upsert wire (contract §1) ---

// memberCalls/iterationCalls capture the on-demand import calls for the
// user/iteration assertions.
var (
	memberCalls    []memberUpsertReq
	iterationCalls []iterationUpsertReq
)

type memberUpsertReq struct {
	ExternalID string `json:"external_id"`
	Name       string `json:"name,omitempty"`
	Email      string `json:"email,omitempty"`
	Username   string `json:"username,omitempty"`
}

type memberUpsertResp struct {
	Status  string `json:"status"`
	UserID  string `json:"user_id,omitempty"`
	Created bool   `json:"created,omitempty"`
	Error   string `json:"error,omitempty"`
}

type iterationUpsertReq struct {
	ExternalID int64  `json:"external_id"`
	Name       string `json:"name"`
	StartDate  string `json:"start_date,omitempty"`
	EndDate    string `json:"end_date,omitempty"`
	Status     string `json:"status,omitempty"`
	Workspace  string `json:"workspace"`
}

type iterationUpsertResp struct {
	Status      string `json:"status"`
	IterationID string `json:"iteration_id,omitempty"`
	Created     bool   `json:"created,omitempty"`
	Error       string `json:"error,omitempty"`
}

func memberUpsert(_ context.Context, p *extism.CurrentPlugin, stack []uint64) {
	raw, err := p.ReadBytes(stack[0])
	if err != nil {
		stack[0] = 0
		return
	}
	var req memberUpsertReq
	resp := memberUpsertResp{Status: "error", Error: "bad member_upsert request"}
	if json.Unmarshal(raw, &req) == nil {
		memberCalls = append(memberCalls, req)
		resp = memberUpsertResp{Status: "ok", UserID: "9901", Created: true}
	}
	writeJSON(p, stack, resp)
}

func iterationUpsert(_ context.Context, p *extism.CurrentPlugin, stack []uint64) {
	raw, err := p.ReadBytes(stack[0])
	if err != nil {
		stack[0] = 0
		return
	}
	var req iterationUpsertReq
	resp := iterationUpsertResp{Status: "error", Error: "bad iteration_upsert request"}
	if json.Unmarshal(raw, &req) == nil {
		iterationCalls = append(iterationCalls, req)
		resp = iterationUpsertResp{Status: "ok", IterationID: fmt.Sprintf("%d", req.ExternalID+10), Created: true}
	}
	writeJSON(p, stack, resp)
}

type wsRequest struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
	Query   map[string]string `json:"query"`
}
type wsResponse struct {
	StatusCode int               `json:"statusCode"`
	Headers    map[string]string `json:"headers"`
	Body       string            `json:"body"`
}

var fails int

func check(name string, got wsResponse, wantCode int, wantBody string) {
	ok := got.StatusCode == wantCode
	if wantBody != "" && got.Body != wantBody {
		ok = false
	}
	if ok {
		fmt.Printf("PASS %-38s %d %s\n", name, got.StatusCode, wantBody)
		return
	}
	fails++
	fmt.Printf("FAIL %-38s code=%d body=%q (want %d %q)\n", name, got.StatusCode, got.Body, wantCode, wantBody)
}

func call(ctx context.Context, plugin *extism.Plugin, method, path string, query map[string]string, body string) wsResponse {
	in, _ := json.Marshal(wsRequest{
		Method:  method,
		Path:    path,
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    body,
		Query:   query,
	})
	exit, out, err := plugin.Call("handle_request", in)
	if err != nil {
		fails++
		fmt.Printf("FAIL call %s %s: %v\n", method, path, err)
		return wsResponse{}
	}
	if exit != 0 {
		fails++
		fmt.Printf("FAIL call %s %s: plugin exit code %d\n", method, path, exit)
		return wsResponse{}
	}
	var resp wsResponse
	if json.Unmarshal(out, &resp) != nil {
		fails++
		fmt.Printf("FAIL unmarshal response for %s %s: %v\n", method, path, err)
		return wsResponse{}
	}
	return resp
}

func main() {
	wasmPath := os.Args[1]
	ctx := context.Background()
	// Fixed guest clock (windshift core pins the same var): far enough in
	// the past that "2h ago" stories and all windows land at deterministic
	// 2021 timestamps the tests can assert on.
	manifest := extism.Manifest{
		Wasm:   []extism.Wasm{extism.WasmFile{Path: wasmPath}},
		Config: map[string]string{"NOW": "2021-12-05T00:00:00Z"},
		// Mirror the live core's constraints (manager.go): 64 MiB memory
		// ceiling and the 5s per-invocation timeout. The 4 MB /epics
		// payload only reproduces the live "module closed" death under
		// both — without them the harness passed while live died.
		Memory:  &extism.ManifestMemory{MaxPages: 1024}, // 64 MiB / 64 KiB pages
		Timeout: 5000,
	}
	// Surface guest log_info output (the SDK gates it behind a global level
	// that defaults to LogLevelOff) — mirrors the core manager patch.
	extism.SetLogLevel(extism.LogLevelInfo)
	plugin, err := extism.NewPlugin(ctx, manifest, extism.PluginConfig{EnableWasi: true},
		[]extism.HostFunction{
			extism.NewHostFunctionWithStack("kv_get", kvGet, []extism.ValueType{extism.ValueTypeI64}, []extism.ValueType{extism.ValueTypeI64}),
			extism.NewHostFunctionWithStack("kv_set", kvSet, []extism.ValueType{extism.ValueTypeI64}, []extism.ValueType{extism.ValueTypeI64}),
			// Stubs for ABI added by the core patch (contracts §1/§2): they
			// satisfy instantiation and fail loudly so an accidental real
			// call during route tests is visible. The full E2E against the
			// patched core is the ship-task gate (tk-ntl).
			extism.NewHostFunctionWithStack("kv_delete", kvDelete, []extism.ValueType{extism.ValueTypeI64}, []extism.ValueType{extism.ValueTypeI64}),
			extism.NewHostFunctionWithStack("http_fetch", httpFetch, []extism.ValueType{extism.ValueTypeI64}, []extism.ValueType{extism.ValueTypeI64}),
			extism.NewHostFunctionWithStack("item_upsert", itemUpsert, []extism.ValueType{extism.ValueTypeI64}, []extism.ValueType{extism.ValueTypeI64}),
			extism.NewHostFunctionWithStack("item_lookup", itemLookup, []extism.ValueType{extism.ValueTypeI64}, []extism.ValueType{extism.ValueTypeI64}),
			extism.NewHostFunctionWithStack("create_comment", createComment, []extism.ValueType{extism.ValueTypeI64}, []extism.ValueType{extism.ValueTypeI64}),
			extism.NewHostFunctionWithStack("workspace_upsert", workspaceUpsert, []extism.ValueType{extism.ValueTypeI64}, []extism.ValueType{extism.ValueTypeI64}),
			extism.NewHostFunctionWithStack("purge_sync", purgeSync, []extism.ValueType{extism.ValueTypeI64}, []extism.ValueType{extism.ValueTypeI64}),
			extism.NewHostFunctionWithStack("member_upsert", memberUpsert, []extism.ValueType{extism.ValueTypeI64}, []extism.ValueType{extism.ValueTypeI64}),
			extism.NewHostFunctionWithStack("iteration_upsert", iterationUpsert, []extism.ValueType{extism.ValueTypeI64}, []extism.ValueType{extism.ValueTypeI64}),
		})
	if err != nil {
		fmt.Printf("FAIL instantiate: %v\n", err)
		os.Exit(1)
	}
	defer plugin.Close(ctx)

	check("GET /status", call(ctx, plugin, "GET", "/status", nil, ""), 200, `{"kv":true,"plugin":"shortcut","version":"0.5.0"}`)
	check("GET /unmatched route", call(ctx, plugin, "GET", "/unmatched", nil, ""), 404, `{"error":"route not found: GET /unmatched"}`)
	runSyncE2E(ctx, plugin)
	runPerProjectE2E(ctx, plugin)
	runTeamE2E(ctx, plugin)
	runDumpRestoreE2E(ctx, plugin)
	runBackfillSweepSkipE2E(ctx, plugin)
	runBigEpicsCondenseE2E(ctx, plugin)
	runWebhookTests(ctx, plugin)
	runConfigTests(ctx, plugin)

	if fails > 0 {
		fmt.Printf("\n%d FAILURES\n", fails)
		os.Exit(1)
	}
	fmt.Println("\nAll checks passed — wasm verified against the windshift runtime stack.")
}

// --- sync_tick E2E ---
//
// Drives the real sync_tick export against the mocks: tick 1 runs the
// catalog pass and opens the backfill window; tick 2 searches, upserts the
// stories, and imports comments. Then replays the same window with the
// updated_at-stable mocks to prove idempotent resume.

type testState struct {
	Phase            string `json:"phase"`
	StoryWindowStart string `json:"story_window_start"`
	StoryWindowEnd   string `json:"story_window_end"`
	StoryIdx         int    `json:"story_idx"`
	CommentIdx       int    `json:"comment_idx"`
	StorySweepIdx    int    `json:"story_sweep_idx"`
	LastWindowEnd    string `json:"last_window_end,omitempty"`
	CatalogEpics     string `json:"catalog_epics,omitempty"`
	CatalogCondensed bool   `json:"catalog_condensed,omitempty"`
	CatalogEpicIdx   int    `json:"catalog_epic_idx,omitempty"`
	Counts           struct {
		Created  int `json:"created"`
		Updated  int `json:"updated"`
		Skipped  int `json:"skipped"`
		Errors   int `json:"errors"`
		Comments int `json:"comments"`
	} `json:"counts"`
	LastErrors []string `json:"last_errors,omitempty"`
}

func want(cond bool, name, detail string) {
	if !cond {
		fails++
		fmt.Printf("FAIL %s: %s\n", name, detail)
	}
}

// testConfigJSON is the shared E2E/webhook config; webhook_secret matches
// the HMAC key the webhook tests sign with ("whsec").
const testConfigJSON = `{"token":"tok","enabled":true,"dry_run":false,"workspace_id":"demo","project_ids":[5],"actor_user_id":7,"label_mode":"merge","webhook_secret":"whsec"}`

func runSyncE2E(ctx context.Context, plugin *extism.Plugin) {
	resetE2E()
	kv["shortcut:config"] = testConfigJSON
	delete(kv, "shortcut:state")
	delete(kv, "shortcut:cmt:9001")

	tick(ctx, plugin, "catalog")

	tick(ctx, plugin, "stories+comments")

	st := state()
	want(st != nil, "state persisted", "kv[shortcut:state] missing")
	if st == nil {
		return
	}
	want(st.Counts.Created == 2, "stories created", fmt.Sprintf("created=%d errors=%v", st.Counts.Created, st.LastErrors))
	want(st.Counts.Errors == 0, "no errors", fmt.Sprintf("%v", st.LastErrors))
	want(st.Counts.Comments == 1, "one comment imported", fmt.Sprintf("comments=%d errors=%v", st.Counts.Comments, st.LastErrors))
	want(st.StoryIdx == 0 && st.StoryWindowStart == "", "window parked", fmt.Sprintf("idx=%d cmt=%d sweep=%d window=%q..%q", st.StoryIdx, st.CommentIdx, st.StorySweepIdx, st.StoryWindowStart, st.StoryWindowEnd))
	want(st.LastWindowEnd != "", "watermark advanced", fmt.Sprintf("last_window_end=%q", st.LastWindowEnd))
	want(st.Phase == "stories", "phase parked (stories)", fmt.Sprintf("phase=%q", st.Phase))

	if len(upsertCalls) == 2 {
		u := upsertCalls[0]
		want(u.ExternalKind == "story" && u.ExternalID == 101, "story upsert ref", fmt.Sprintf("%s/%d", u.ExternalKind, u.ExternalID))
		want(u.Title == "First" && u.StatusName == "In Progress", "story title/status", fmt.Sprintf("title=%q status=%q", u.Title, u.StatusName))
		want(u.ItemTypeName == "Story", "item_type_name", fmt.Sprintf("%q", u.ItemTypeName))
		want(u.WorkspaceID == "demo", "workspace id", fmt.Sprintf("%q", u.WorkspaceID))
	}
	want(len(upsertCalls) == 2, "two upserts", fmt.Sprintf("upserts=%d", len(upsertCalls)))
	want(len(commentCalls) == 1, "one create_comment", fmt.Sprintf("comments=%d", len(commentCalls)))
	if len(commentCalls) == 1 {
		c := commentCalls[0]
		want(c.ItemID == 1 && c.AuthorID == 7, "comment item/author", fmt.Sprintf("item=%d author=%d", c.ItemID, c.AuthorID))
		want(c.Content == "hello", "comment content", fmt.Sprintf("%q", c.Content))
		want(c.SuppressNotifications, "notifications suppressed", "suppress_notifications=false")
	}
	want(kv["shortcut:cmt:9001"] == "1", "comment dedup key", fmt.Sprintf("kv=%q", kv["shortcut:cmt:9001"]))

	// Replay: same window contents again. item_lookup now reports the synced
	// items with their last_synced_at — the engine must skip the stories and
	// the already-imported comment, with zero new host writes.
	beforeUpserts, beforeComments := len(upsertCalls), len(commentCalls)
	tick(ctx, plugin, "replay")
	st = state()
	if st != nil {
		want(st.Counts.Created == 2, "replay: no re-creates", fmt.Sprintf("created=%d", st.Counts.Created))
		want(st.Counts.Skipped >= 3, "replay: stories+comment skipped", fmt.Sprintf("skipped=%d", st.Counts.Skipped))
		want(st.Counts.Errors == 0, "replay: no errors", fmt.Sprintf("%v", st.LastErrors))
	}
	want(len(upsertCalls) == beforeUpserts, "replay: no new upserts", fmt.Sprintf("upserts=%d→%d", beforeUpserts, len(upsertCalls)))
	want(len(commentCalls) == beforeComments, "replay: no new comments", fmt.Sprintf("comments=%d→%d", beforeComments, len(commentCalls)))

	// Deletion sweep (tk-thf AC 4): hard-delete story 102 in the mock, then
	// let the next window run. The sweep re-verifies each mapped window
	// story with a canonical GET; 102 now 404s → tombstone comment on the
	// mapped item + KV marker. Window contents are range-independent in the
	// mock, so the fresh window still contains both stories.
	deletedStories["102"] = true
	beforeComments = len(commentCalls)
	tick(ctx, plugin, "sweep")
	st = state()
	if st != nil {
		want(st.Counts.Errors == 0, "sweep: no errors", fmt.Sprintf("%v", st.LastErrors))
	}
	want(kv["shortcut:tomb:story:102"] != "", "sweep: tombstone marker", fmt.Sprintf("kv=%q", kv["shortcut:tomb:story:102"]))
	var marker struct {
		DeletedAt string `json:"deleted_at"`
		Source    string `json:"source"`
	}
	if raw := kv["shortcut:tomb:story:102"]; raw != "" {
		if json.Unmarshal([]byte(raw), &marker) != nil {
			fails++
			fmt.Println("FAIL sweep: tombstone marker not valid JSON: " + raw)
		} else {
			want(marker.Source == "sweep", "sweep: marker source", fmt.Sprintf("source=%q", marker.Source))
			want(marker.DeletedAt != "", "sweep: marker timestamp", "deleted_at empty")
		}
	}
	want(len(commentCalls) == beforeComments+1, "sweep: one tombstone comment", fmt.Sprintf("comments=%d→%d", beforeComments, len(commentCalls)))
	want(len(upsertCalls) == beforeUpserts, "sweep: no new upserts", fmt.Sprintf("upserts=%d→%d", beforeUpserts, len(upsertCalls)))

	// Next window: the tombstoned story must be skipped everywhere — no
	// re-fetch, no new comment, marker untouched.
	markerJSON := kv["shortcut:tomb:story:102"]
	beforeComments = len(commentCalls)
	before102Gets := countStoryGets("102")
	tick(ctx, plugin, "post-sweep")
	want(countStoryGets("102") == before102Gets, "post-sweep: 102 never re-fetched", fmt.Sprintf("gets=%d", countStoryGets("102")-before102Gets))
	want(len(commentCalls) == beforeComments, "post-sweep: no new comments", fmt.Sprintf("comments=%d→%d", beforeComments, len(commentCalls)))
	want(kv["shortcut:tomb:story:102"] == markerJSON, "post-sweep: marker stable", "")

	fmt.Printf("\nsync_tick E2E done — upserts=%d comments=%d gets=%d\n", len(upsertCalls), len(commentCalls), len(getStoryCalls))
}

// runPerProjectE2E exercises workspace_mode=per_project: per-project
// workspace_upsert, unknown-project fallback, and rejected-project handling.
func runPerProjectE2E(ctx context.Context, plugin *extism.Plugin) {
	fmt.Printf("\n--- per_project workspace mode ---\n")
	resetE2E()
	projectsBody = `[{"id":5,"name":"Demo","archived":false},{"id":7,"name":"Owned","archived":false}]`
	searchBody = perProjectStoriesJSON()
	kv["shortcut:config"] = `{"token":"tok","enabled":true,"dry_run":false,"workspace_mode":"per_project","workspace_id":"demo","actor_user_id":7,"label_mode":"merge"}`
	delete(kv, "shortcut:state")

	tick(ctx, plugin, "per_project catalog")
	tick(ctx, plugin, "per_project stories+comments")
	st := state()
	if st == nil {
		fails++
		fmt.Println("FAIL per_project: no state")
		return
	}
	want(st.Counts.Created == 5, "per_project: 4 stories created + on-demand parent epic", fmt.Sprintf("created=%d errors=%v", st.Counts.Created, st.LastErrors))
	want(st.Counts.Errors == 1, "per_project: rejected project recorded once", fmt.Sprintf("errors=%d %v", st.Counts.Errors, st.LastErrors))
	want(len(wsCalls) == 2, "per_project: two workspace_upsert calls", fmt.Sprintf("calls=%d", len(wsCalls)))
	want(wsCalls[0].ExternalKind == "project", "per_project: upsert carries kind=project", fmt.Sprintf("kind=%q", wsCalls[0].ExternalKind))
	want(wsByProject[5].wsID == "ws-5", "per_project: project 5 workspace", fmt.Sprintf("%v", wsByProject))
	want(len(upsertCalls) == 5, "per_project: five upserts (4 stories + imported parent epic)", fmt.Sprintf("upserts=%d", len(upsertCalls)))
	epicIdx, story104Idx := -1, -1
	for i, u := range upsertCalls {
		switch u.ExternalID {
		case 101:
			want(u.WorkspaceID == "ws-5", "per_project: story 101 → project 5 workspace", fmt.Sprintf("ws=%q", u.WorkspaceID))
			// Story 101's fixture name is 300+ runes with a newline: the
			// title must be the cut single-line form and the full original
			// (control runes collapsed — an H1 is a single line) preserved
			// as a markdown heading atop the description.
			full := "First " + strings.Repeat("F", 300)
			want(u.Title == "First "+strings.Repeat("F", 249), "title sanitized to 255 single-line runes", fmt.Sprintf("len=%d", len([]rune(u.Title))))
			want(u.Description == "# "+full+"\n\n", "long title preserved as description heading", fmt.Sprintf("desc len=%d head=%q", len(u.Description), u.Description[:20]))
		case 102:
			want(u.WorkspaceID == "demo", "per_project: unknown-project story 102 → fallback", fmt.Sprintf("ws=%q", u.WorkspaceID))
		case 104:
			want(u.ParentExternalKind == "epic" && u.ParentExternalID == 5001, "per_project: story 104 carries epic parent", fmt.Sprintf("parent=%q/%d", u.ParentExternalKind, u.ParentExternalID))
			want(u.AssigneeExternalID == "687a7ce8-ae4b-4528-975d-bfe3fdc8bca8", "per_project: story 104 carries first assignee", fmt.Sprintf("assignee=%q", u.AssigneeExternalID))
			want(u.IterationExternalID == 4242, "per_project: story 104 carries iteration", fmt.Sprintf("iteration=%d", u.IterationExternalID))
		case 105:
			want(u.ParentExternalKind == "" && u.ParentExternalID == 0, "per_project: story 105 imports parentless on 404 epic", fmt.Sprintf("parent=%q/%d", u.ParentExternalKind, u.ParentExternalID))
		case 5001:
			want(u.ExternalKind == "epic" && u.ItemTypeName == "Epic", "per_project: on-demand parent epic upserted as Epic", fmt.Sprintf("kind=%q type=%q", u.ExternalKind, u.ItemTypeName))
			if u.ExternalKind == "epic" && u.ExternalID == 5001 {
				epicIdx = i
			}
		}
		if u.ExternalID == 104 {
			story104Idx = i
		}
	}
	want(epicIdx >= 0 && story104Idx > epicIdx, "per_project: parent epic imported before its story", fmt.Sprintf("epic@%d story104@%d", epicIdx, story104Idx))
	// The assignee and sprint import on demand right before the story that
	// references them, with the data Shortcut serves on the one-off GETs.
	if len(memberCalls) != 1 || memberCalls[0].ExternalID != "687a7ce8-ae4b-4528-975d-bfe3fdc8bca8" || memberCalls[0].Name != "Dev Yuri" ||
		memberCalls[0].Email != "yuri@nsx.dev" || memberCalls[0].Username != "dev.yuri" {
		want(false, "per_project: member imported on demand", fmt.Sprintf("%+v", memberCalls))
	} else {
		want(true, "per_project: member imported on demand", memberCalls[0].Name)
	}
	if len(iterationCalls) != 1 || iterationCalls[0].ExternalID != 4242 || iterationCalls[0].Name != "Sprint 42" ||
		iterationCalls[0].StartDate != "2026-10-06" || iterationCalls[0].EndDate != "2026-10-20" ||
		iterationCalls[0].Status != "unstarted" || iterationCalls[0].Workspace != "ws-5" {
		want(false, "per_project: iteration imported into story workspace", fmt.Sprintf("%+v", iterationCalls))
	} else {
		want(true, "per_project: iteration imported into story workspace", iterationCalls[0].Name)
	}
	want(items["story/104"].id != 0 && items["story/105"].id != 0 && items["epic/5001"].id != 0, "per_project: parent-epic stories and epic stored", fmt.Sprintf("104=%d 105=%d epic=%d", items["story/104"].id, items["story/105"].id, items["epic/5001"].id))
	want(items["story/103"].id == 0, "per_project: rejected story 103 not upserted", fmt.Sprintf("id=%d", items["story/103"].id))

	// Replay: the fresh window contains the same stories; 101/102 skip via
	// item_lookup, but 103's project rejection is NOT cached — item-level
	// errors retry every window, so workspace_upsert fires once more.
	beforeUpserts, beforeWSCalls := len(upsertCalls), len(wsCalls)
	tick(ctx, plugin, "per_project replay")
	st = state()
	if st != nil {
		want(st.Counts.Created == 5, "per_project replay: no re-creates", fmt.Sprintf("created=%d", st.Counts.Created))
	}
	want(len(upsertCalls) == beforeUpserts, "per_project replay: no new upserts", fmt.Sprintf("upserts=%d→%d", beforeUpserts, len(upsertCalls)))
	want(len(wsCalls) == beforeWSCalls+1, "per_project replay: rejection retried once", fmt.Sprintf("ws calls=%d→%d", beforeWSCalls, len(wsCalls)))

	projectsBody, searchBody = defaultProjectsBody, defaultSearchBody
	kv["shortcut:config"] = testConfigJSON
	fmt.Printf("\nper_project E2E done — wsCreated=%d upserts=%d\n", wsCreated, len(upsertCalls))
}

// teamSearchStoriesJSON: story 201 sits in a Tech Development state (team
// 9001), 202 in an SRE state (team 9002), 203 in an unknown state (fallback).
func teamSearchStoriesJSON() string {
	at := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
	return fmt.Sprintf(`[
		{"id":201,"name":"TD story","description":"","story_type":"feature","workflow_state_id":11,"project_id":5,
			"app_url":"https://app.shortcut.com/demo/story/201","created_at":%[1]q,"updated_at":%[1]q},
		{"id":202,"name":"SRE story","description":"","story_type":"bug","workflow_state_id":12,"project_id":5,
			"app_url":"https://app.shortcut.com/demo/story/202","created_at":%[1]q,"updated_at":%[1]q},
		{"id":203,"name":"Orphan story","description":"","story_type":"chore","workflow_state_id":999,"project_id":5,
			"app_url":"https://app.shortcut.com/demo/story/203","created_at":%[1]q,"updated_at":%[1]q}
	]`, at)
}

// runTeamE2E covers workspace_mode:"team": the catalog creates one workspace
// per Shortcut team with the team's workflow bound (states ride the
// workspace_upsert payload), stories are placed by their workflow state's
// team, unknown states fall back to the configured workspace, and
// POST /sync/purge wipes every synced artifact for a clean re-import.
func runTeamE2E(ctx context.Context, plugin *extism.Plugin) {
	fmt.Printf("\n--- team workspace mode ---\n")
	resetE2E()
	workflowsBody = `[{"id":1,"name":"Tech Development","team_id":9001,"states":[{"id":11,"name":"In Progress","type":"started"}]},
		{"id":2,"name":"SRE — Simple","team_id":9002,"states":[{"id":12,"name":"To Do","type":"unstarted"},{"id":13,"name":"Done","type":"done"}]}]`
	teamsBody = `[
		{"id":9001,"name":"Tech Development","archived":false,"workflow":{"id":1,"name":"Tech Development","states":[{"id":11,"name":"In Progress","type":"started"}]}},
		{"id":9002,"name":"SRE","archived":false,"workflow":{"id":2,"name":"SRE — Simple","states":[{"id":12,"name":"To Do","type":"unstarted"},{"id":13,"name":"Done","type":"done"}]}}
	]`
	searchBody = teamSearchStoriesJSON()
	kv["shortcut:config"] = `{"token":"tok","enabled":true,"dry_run":false,"workspace_mode":"team","workspace_id":"demo","actor_user_id":7,"label_mode":"merge"}`
	delete(kv, "shortcut:state")

	tick(ctx, plugin, "team catalog")
	tick(ctx, plugin, "team stories+comments")
	st := state()
	if st == nil {
		fails++
		fmt.Println("FAIL team: no state")
		return
	}
	want(st.Counts.Created == 3, "team: three stories created", fmt.Sprintf("created=%d errors=%v", st.Counts.Created, st.LastErrors))
	want(st.Counts.Errors == 0, "team: no errors", fmt.Sprintf("errors=%d %v", st.Counts.Errors, st.LastErrors))
	// Per-tick workspace cache: the catalog upserts both teams, and the
	// story tick re-upserts each team once before its cache warms — the
	// core is find-or-create, so the repeat is a no-op there.
	want(len(wsCalls) == 4, "team: both teams upserted in catalog and story tick", fmt.Sprintf("calls=%d", len(wsCalls)))
	want(wsCalls[0].ExternalKind == "team" && wsCalls[0].ExternalID == 9001, "team: upsert kind=team", fmt.Sprintf("%+v", wsCalls[0]))
	want(len(wsCalls[0].States) == 1 && wsCalls[0].States[0].Name == "In Progress", "team: workflow states ride the upsert", fmt.Sprintf("states=%v", wsCalls[0].States))
	want(len(wsCalls[1].States) == 2, "team: second team carries its states", fmt.Sprintf("states=%v", wsCalls[1].States))
	byStory := map[int64]string{}
	for _, u := range upsertCalls {
		if u.ExternalKind == "story" {
			byStory[u.ExternalID] = u.WorkspaceID
		}
	}
	want(byStory[201] == "ws-9001", "team: story 201 → Tech Development workspace", fmt.Sprintf("ws=%q", byStory[201]))
	want(byStory[202] == "ws-9002", "team: story 202 → SRE workspace", fmt.Sprintf("ws=%q", byStory[202]))
	want(byStory[203] == "demo", "team: unknown state → fallback workspace", fmt.Sprintf("ws=%q", byStory[203]))
	want(len(items) == 3, "team: three items stored", fmt.Sprintf("items=%d", len(items)))

	// Purge: items, mappings, and plugin KV all go; config survives.
	before := len(items)
	purgeResp := call(ctx, plugin, "POST", "/sync/purge", nil, "")
	check("purge: 200 with report", purgeResp, 200, "")
	var purgeReport struct {
		Status string `json:"status"`
		Items  int    `json:"items"`
	}
	_ = json.Unmarshal([]byte(purgeResp.Body), &purgeReport)
	want(purgeReport.Status == "ok" && purgeReport.Items == 3, "purge: reports wiped items", fmt.Sprintf("body=%s", purgeResp.Body))
	want(len(items) == 0 && before == 3, "purge: every mapped item deleted", fmt.Sprintf("before=%d after=%d", before, len(items)))
	_, stateAlive := kv["shortcut:state"]
	want(!stateAlive, "purge: sync state cleared", fmt.Sprintf("alive=%v", stateAlive))
	want(kv["shortcut:config"] != "", "purge: config survives", "")

	// Post-purge ticks start a fresh catalog: team workspaces re-upsert
	// (find-or-create on a wiped mapping table), then stories re-import.
	tick(ctx, plugin, "team re-catalog after purge")
	tick(ctx, plugin, "team re-import after purge")
	st = state()
	want(st != nil && st.Phase == "stories", "purge: fresh catalog reached stories", func() string {
		if st == nil {
			return "no state"
		}
		return st.Phase
	}())
	want(len(wsCalls) == 8, "purge: team workspaces recreated", fmt.Sprintf("ws calls=%d", len(wsCalls)))
	want(len(items) == 3, "purge: stories re-imported", fmt.Sprintf("items=%d", len(items)))
}

// runBackfillSweepSkipE2E covers initial-import mode (backfill_days > 365):
// the end-of-window sweep is skipped — every story in the window was created
// or canonically re-verified by the story pass moments earlier — while the
// operational value (≤365) sweeps mapped stories again.
func runBackfillSweepSkipE2E(ctx context.Context, plugin *extism.Plugin) {
	fmt.Printf("\n--- backfill sweep skip ---\n")
	resetE2E()
	kv["shortcut:config"] = `{"token":"tok","enabled":true,"dry_run":false,"workspace_id":"demo","actor_user_id":7,"label_mode":"merge","backfill_days":400}`
	tick(ctx, plugin, "sweep-skip catalog+window")
	tick(ctx, plugin, "sweep-skip stories")
	afterStories := len(getStoryCalls)
	tick(ctx, plugin, "sweep-skip window close")
	want(len(getStoryCalls) == afterStories, "sweep skipped in initial-import mode",
		fmt.Sprintf("extra gets=%d", len(getStoryCalls)-afterStories))

	// Operational mode: the sweep re-verifies both mapped stories.
	kv["shortcut:config"] = testConfigJSON
	tick(ctx, plugin, "operational sweep")
	want(len(getStoryCalls) == afterStories+2, "operational sweep re-verifies mapped stories",
		fmt.Sprintf("gets=%d want %d", len(getStoryCalls), afterStories+2))
}

// runDumpRestoreE2E covers the full-account dump + restore round-trip: the
// dump streams a window-filtered fixture into NDJSON pieces, the harness
// reassembles them like the CLI would, and POST /restore/chunk replays the
// file into a purged workspace. A second identical restore proves
// idempotence — everything must skip via ShouldSkip and comment dedup with
// zero new writes. The fixture also pins the ordering invariants restore
// depends on: the iteration line precedes its story (story→sprint resolves
// at upsert time) and the member line precedes it (assignee resolution).
func runDumpRestoreE2E(ctx context.Context, plugin *extism.Plugin) {
	fmt.Printf("\n--- dump + restore ---\n")
	resetE2E()
	kv["shortcut:config"] = testConfigJSON
	check("purge before dump E2E", call(ctx, plugin, "POST", "/sync/purge", nil, ""), 200, "")

	// Install the dump fixture: window-stamped stories so the window-aware
	// mock serves them on exactly one of the ~8000 tiled windows.
	searchBody = dumpStoriesJSON()
	membersBody = `[{"id":"687a7ce8-ae4b-4528-975d-bfe3fdc8bca8","profile":{"name":"Dev Yuri","email_address":"yuri@nsx.dev","mention_name":"dev.yuri"}}]`
	epicsBody = `[{"id":6001,"name":"Dump Epic","description":"","app_url":"https://app.shortcut.com/demo/epic/6001","epic_state_id":21,"created_at":"2021-10-01T00:00:00Z","updated_at":"2021-10-02T00:00:00Z"}]`
	epicComments["6001"] = `[{"id":8101,"epic_id":6001,"text":"epic note","author_id":"77","created_at":"2021-10-05T08:00:00Z","updated_at":"2021-10-05T08:00:00Z","deleted":false,"position":0}]`
	searchWindowFilter = true

	// --- dump: start, tick to done, assemble the pieces ---
	check("dump start", call(ctx, plugin, "POST", "/dump/start", nil, `{"force":false}`), 200, "")
	// The rate limiter caps each tick at ~180 fetches, so the ~8000-window
	// scan needs ~50 ticks; every tick persists its cursor and resumes.
	dumpDone := false
	for i := 0; i < 100 && !dumpDone; i++ {
		resp := call(ctx, plugin, "POST", "/dump/tick", nil, "")
		var st struct {
			Phase  string `json:"phase"`
			Error  string `json:"error"`
			Counts struct {
				Stories       int `json:"stories"`
				Comments      int `json:"comments"`
				Iterations    int `json:"iterations"`
				Epics         int `json:"epics"`
				EpicComments  int `json:"epic_comments"`
				Members       int `json:"members"`
			} `json:"counts"`
		}
		if resp.StatusCode != 200 || json.Unmarshal([]byte(resp.Body), &st) != nil {
			want(false, "dump tick", resp.Body)
			return
		}
		want(st.Error == "", "dump tick error", st.Error)
		dumpDone = st.Phase == "done"
		if st.Phase != "done" {
			continue
		}
		want(st.Counts.Stories == 3 && st.Counts.Comments == 3 && st.Counts.Iterations == 1 &&
			st.Counts.Epics == 1 && st.Counts.EpicComments == 1 && st.Counts.Members == 1,
			"dump counts", fmt.Sprintf("%+v", st.Counts))
	}
	want(dumpDone, "dump completes within tick cap", "")

	// Assemble the file exactly like the CLI: raw NDJSON pieces concatenated.
	var file string
	for n := 0; ; n++ {
		resp := call(ctx, plugin, "GET", "/dump/piece", map[string]string{"n": strconv.Itoa(n)}, "")
		if resp.StatusCode != 200 {
			break
		}
		file += resp.Body
	}
	lines := strings.Split(strings.TrimSuffix(file, "\n"), "\n")
	typeIdx := func(t string) int {
		for i, ln := range lines {
			if strings.HasPrefix(ln, `{"type":"`+t+`"`) {
				return i
			}
		}
		return -1
	}
	want(len(lines) > 10, "dump file has all lines", fmt.Sprintf("lines=%d", len(lines)))
	want(typeIdx("meta") == 0, "file opens with meta", lines[0][:min(80, len(lines[0]))])
	// Ordering invariants: catalog lines precede their stories.
	itIdx, storyIdx := typeIdx("iterations"), -1
	for i, ln := range lines {
		if strings.Contains(ln, "Dump Alpha") { // story lines pass the body through raw
			storyIdx = i
			break
		}
	}
	want(itIdx != -1 && storyIdx != -1 && itIdx < storyIdx, "iteration line precedes story 201",
		fmt.Sprintf("it=%d story=%d", itIdx, storyIdx))
	memIdx := typeIdx("members")
	want(memIdx != -1 && memIdx < storyIdx, "member line precedes story 201",
		fmt.Sprintf("mem=%d story=%d", memIdx, storyIdx))
	epIdx, epCmtIdx := typeIdx("epics"), typeIdx("epic_comments")
	want(epIdx != -1 && epCmtIdx != -1 && epIdx < epCmtIdx, "epic precedes its comments",
		fmt.Sprintf("epic=%d cmt=%d", epIdx, epCmtIdx))
	want(typeIdx("end") == len(lines)-1, "file ends with end line", fmt.Sprintf("n=%d", len(lines)))

	// --- restore: meta-first rejection, wrong-seq rejection, full replay ---
	check("restore discard (clean slate)", call(ctx, plugin, "POST", "/restore/discard", nil, ""), 204, "")
	check("restore rejects non-meta first chunk", call(ctx, plugin, "POST", "/restore/chunk", map[string]string{"seq": "0"}, strings.Join(lines[1:], "\n")+"\n"), 400, "")

	// Chunked replay: split the file into 2 chunks mid-way through the
	// stories so the story/sprint/assignee resolution crosses a chunk
	// boundary (buffered state must survive).
	half := storyIdx + 1
	done := false
	done = postRestoreChunks(ctx, plugin, 0, lines[:half])
	check("restore rejects out-of-order seq", call(ctx, plugin, "POST", "/restore/chunk", map[string]string{"seq": "3"}, lines[0]+"\n"), 409, "")
	done = postRestoreChunks(ctx, plugin, 1, lines[half:])
	want(done, "restore completes", "")

	// First restore: two new stories + one archived + epic + sprint + member
	// assignment + 2 story comments + 1 epic comment; zero errors.
	var st struct {
		Counts struct {
			Created  int      `json:"created"`
			Updated  int      `json:"updated"`
			Skipped  int      `json:"skipped"`
			Errors   int      `json:"errors"`
			Comments int      `json:"comments"`
		} `json:"counts"`
		TypeCounts map[string]int `json:"type_counts"`
	}
	resp := call(ctx, plugin, "GET", "/restore/status", nil, "")
	_ = json.Unmarshal([]byte(resp.Body), &st)
	want(st.Counts.Created == 4 && st.Counts.Comments == 3 && st.Counts.Errors == 0,
		"restore counts", fmt.Sprintf("%+v type=%v", st.Counts, st.TypeCounts))
	if m := items["story/201"]; m.req.Title != "Dump Alpha" || m.req.IterationExternalID != 4242 ||
		m.req.AssigneeExternalID != "687a7ce8-ae4b-4528-975d-bfe3fdc8bca8" ||
		m.req.ParentExternalID != 6001 || m.req.ParentExternalKind != "epic" {
		want(false, "restore: story 201 mapping surface", fmt.Sprintf("%+v", m.req))
	}
	if m := items["story/203"]; m.req.Title != "Dump Archived" {
		want(false, "restore: archived story imported", fmt.Sprintf("%+v", m.req))
	} else {
		want(m.req.ParentExternalID == 6001, "restore: archived story epic link", fmt.Sprintf("%+v", m.req))
	}
	// The offline ensure* paths upsert straight from the buffered lines —
	// exactly one call each, carrying the dump's data.
	if len(memberCalls) != 1 || memberCalls[0].Name != "Dev Yuri" ||
		memberCalls[0].Email != "yuri@nsx.dev" || memberCalls[0].Username != "dev.yuri" {
		want(false, "restore: member upserted from buffered line", fmt.Sprintf("%+v", memberCalls))
	}
	if len(iterationCalls) != 1 || iterationCalls[0].Name != "Sprint 42" || iterationCalls[0].Workspace != "demo" {
		want(false, "restore: iteration upserted from buffered line", fmt.Sprintf("%+v", iterationCalls))
	}
	beforeUpserts := len(upsertCalls)
	beforeComments := len(commentCalls)

	// --- second restore: byte-identical file replays as pure skips ---
	check("restore discard (replay)", call(ctx, plugin, "POST", "/restore/discard", nil, ""), 204, "")
	postRestoreChunks(ctx, plugin, 0, lines[:half])
	done = postRestoreChunks(ctx, plugin, 1, lines[half:])
	want(done, "replay completes", "")
	resp = call(ctx, plugin, "GET", "/restore/status", nil, "")
	_ = json.Unmarshal([]byte(resp.Body), &st)
	want(st.Counts.Created == 0 && st.Counts.Updated == 0 && st.Counts.Comments == 0 && st.Counts.Errors == 0 &&
		st.Counts.Skipped == 7, // 4 stories + epic + 2 story comments + 1 epic comment
		"replay: everything skipped, nothing re-written",
		fmt.Sprintf("%+v type=%v", st.Counts, st.TypeCounts))
	want(len(upsertCalls) == beforeUpserts, "replay: zero upsert calls", fmt.Sprintf("%d→%d", beforeUpserts, len(upsertCalls)))
	want(len(commentCalls) == beforeComments, "replay: zero comment calls",
		fmt.Sprintf("%d→%d", beforeComments, len(commentCalls)))

	// --- restore after done: idempotent re-posts return the status ---
	check("re-post chunk after done", call(ctx, plugin, "POST", "/restore/chunk", map[string]string{"seq": "0"}, lines[0]+"\n"), 200, "")

	// Leave the canonical fixtures for later sections.
	resetE2E()
	kv["shortcut:config"] = testConfigJSON
	check("purge after dump E2E", call(ctx, plugin, "POST", "/sync/purge", nil, ""), 200, "")
}

// postRestoreChunks re-posts one line-aligned chunk until chunk_done, the
// way the CLI does across budget pauses. Returns the response's done flag.
func postRestoreChunks(ctx context.Context, plugin *extism.Plugin, seq int, body []string) bool {
	done := false
	payload := strings.Join(body, "\n") + "\n"
	for post := 0; post < 5 && !done; post++ {
		resp := call(ctx, plugin, "POST", "/restore/chunk", map[string]string{"seq": strconv.Itoa(seq)}, payload)
		var cr struct {
			ChunkDone bool `json:"chunk_done"`
			Done      bool `json:"done"`
		}
		if resp.StatusCode != 200 || json.Unmarshal([]byte(resp.Body), &cr) != nil {
			want(false, fmt.Sprintf("restore chunk %d", seq), fmt.Sprintf("code=%d %s", resp.StatusCode, resp.Body))
			return done
		}
		done = cr.Done
		if cr.ChunkDone {
			break
		}
	}
	return done
}

// runBigEpicsCondenseE2E exercises the catalog condense against a realistic
// /epics stream (real-shaped id/updated_at objects, ~2KB each) inside the
// default pass. The zero-epic default scenarios can't see a wire-shape or
// struct-tag regression in collectEpicMetas — the live stack can (it froze
// 1825 zero-id metas while the harness stayed green until this existed).
func runBigEpicsCondenseE2E(ctx context.Context, plugin *extism.Plugin) {
	fmt.Printf("\n--- big epics condense ---\n")
	resetE2E()
	kv = map[string]string{}
	kv["shortcut:config"] = testConfigJSON
	os.Setenv("BIG_EPICS", "80")
	defer os.Unsetenv("BIG_EPICS")

	// Tick 1 condenses the stream and lazily imports every epic at its
	// frozen position before opening the story window.
	tick(ctx, plugin, "big-epics catalog")

	st := state()
	if st == nil {
		want(false, "big-epics: state saved", "no state")
		return
	}
	if !st.CatalogCondensed || st.CatalogEpics == "" {
		want(false, "big-epics: catalog condensed",
			fmt.Sprintf("condensed=%v len=%d", st.CatalogCondensed, len(st.CatalogEpics)))
		return
	}
	var metas []struct {
		I int64  `json:"i"`
		U string `json:"u"`
	}
	if err := json.Unmarshal([]byte(st.CatalogEpics), &metas); err != nil {
		want(false, "big-epics: condensed metas parse", err.Error())
		return
	}
	zero := 0
	for _, m := range metas {
		if m.I == 0 {
			zero++
		}
	}
	want(len(metas) == 80 && zero == 0, "big-epics: 80 epics condensed with real ids",
		fmt.Sprintf("metas=%d zero=%d", len(metas), zero))

	epics := 0
	for _, u := range upsertCalls {
		if u.ExternalKind == "epic" && u.ExternalID >= 1000 && u.ExternalID < 1080 {
			epics++
		}
	}
	want(epics == 80, "big-epics: every epic upserted at its frozen position",
		fmt.Sprintf("epics=%d idx=%d counts=%+v", epics, st.CatalogEpicIdx, st.Counts))
}

func resetE2E() {
	items = map[string]mockItem{}
	itemSeq, commentSeq = 0, 0
	upsertCalls, commentCalls = nil, nil
	wsByProject, wsCalls, wsCreated = map[int64]mockWorkspace{}, nil, 0
	memberCalls, iterationCalls = nil, nil
	projectsBody, searchBody = defaultProjectsBody, defaultSearchBody
	workflowsBody, teamsBody = defaultWorkflowsBody, defaultTeamsBody
	membersBody, epicsBody = `[]`, `[]`
	searchWindowFilter, epicComments = false, map[string]string{}
	deletedStories, getStoryCalls = map[string]bool{}, nil
	// Tombstone markers and comment-dedup records leak between E2E runs
	// (the sweep tests leave both behind): a stale tomb:story:102 silently
	// skips the story in the per-team run.
	for k := range kv {
		if strings.HasPrefix(k, "shortcut:tomb:") || strings.HasPrefix(k, "shortcut:cmt:") {
			delete(kv, k)
		}
	}
}

func tick(ctx context.Context, plugin *extism.Plugin, name string) {
	_, _, err := plugin.Call("sync_tick", nil)
	if err != nil {
		fails++
		fmt.Printf("FAIL sync_tick (%s): %v\n", name, err)
	}
}

func state() *testState {
	raw, ok := kv["shortcut:state"]
	if !ok {
		return nil
	}
	var st testState
	if json.Unmarshal([]byte(raw), &st) != nil {
		return nil
	}
	return &st
}

// runWebhookTests covers POST /webhook/shortcut (tk-thf): signature
// verification drives 401/400, valid deletions drive 204 + tombstone. Uses
// its own KV reset (saves config + tombstones; state is irrelevant here).
func runWebhookTests(ctx context.Context, plugin *extism.Plugin) {
	// Own baseline: the team E2E purged every mapping, so rebuild one here
	// (a fresh tick maps stories 101/102 for the tombstone-comment case).
	resetE2E()
	tick(ctx, plugin, "webhook baseline")
	kv = map[string]string{}
	kv["shortcut:config"] = testConfigJSON
	delete(kv, "shortcut:tomb:story:999")
	deletedStories["999"] = true
	commentSeq = 0
	commentCalls = nil

	body := `{"actions":[{"action":"story_delete","entity_type":"story","id":999}]}`

	// Unset secret in config → feature disabled → 401 even with a sig.
	kv["shortcut:config"] = `{"token":"tok","workspace_id":"demo"}`
	check("webhook: secret unset → 401", callSigned(ctx, plugin, body, sign(body)), 401, "")
	kv["shortcut:config"] = testConfigJSON

	check("webhook: bad signature → 401", callSigned(ctx, plugin, body, "deadbeef"), 401, "")
	check("webhook: missing signature → 401", callSigned(ctx, plugin, body, ""), 401, "")
	check("webhook: malformed body → 400", callSigned(ctx, plugin, "{not json", sign("{not json")), 400, "")
	check("webhook: no actions → 400", callSigned(ctx, plugin, `{"actions":[]}`, sign(`{"actions":[]}`)), 400, "")
	nonDeletion := `{"actions":[{"action":"story_update","entity_type":"story","id":999}]}`
	check("webhook: non-deletion → 204", callSigned(ctx, plugin, nonDeletion, sign(nonDeletion)), 204, "")
	check("webhook: valid delete → 204", callSigned(ctx, plugin, body, sign(body)), 204, "")

	// Tombstone side effects: KV marker (item 999 unmapped → no comment).
	want(kv["shortcut:tomb:story:999"] != "", "webhook: tombstone marker", fmt.Sprintf("kv=%q", kv["shortcut:tomb:story:999"]))
	var marker struct {
		Source string `json:"source"`
	}
	if raw := kv["shortcut:tomb:story:999"]; raw != "" && json.Unmarshal([]byte(raw), &marker) == nil {
		want(marker.Source == "webhook", "webhook: marker source", fmt.Sprintf("source=%q", marker.Source))
	}
	want(len(commentCalls) == 0, "webhook: unmapped story → no comment", fmt.Sprintf("comments=%d", len(commentCalls)))

	// Mapped story: tombstone comment lands on the mapped item (item 1 was
	// created by the sync E2E; item_lookup here answers from the live map).
	deletedStories["101"] = true
	mappedBody := `{"actions":[{"action":"story_archive","entity_type":"story","id":101}]}`
	check("webhook: archive mapped story → 204", callSigned(ctx, plugin, mappedBody, sign(mappedBody)), 204, "")
	want(len(commentCalls) == 1, "webhook: mapped story → comment", fmt.Sprintf("comments=%d", len(commentCalls)))
	if len(commentCalls) == 1 {
		want(commentCalls[0].ItemID == 1 && commentCalls[0].AuthorID == 7, "webhook: comment item/author", fmt.Sprintf("%+v", commentCalls[0]))
	}
}

// sign is the HMAC-SHA256 hex digest Shortcut's webhook signer would send
// for body under the test secret ("whsec" in testConfigJSON).
func sign(body string) string {
	mac := hmac.New(sha256.New, []byte("whsec"))
	mac.Write([]byte(body))
	return hex.EncodeToString(mac.Sum(nil))
}

// callSigned posts a webhook payload with only the signature header — the
// handler must not depend on Content-Type.
func callSigned(ctx context.Context, plugin *extism.Plugin, body, sig string) wsResponse {
	in, _ := json.Marshal(wsRequest{
		Method:  "POST",
		Path:    "/webhook/shortcut",
		Headers: map[string]string{"Payload-Signature": sig},
		Body:    body,
	})
	exit, out, err := plugin.Call("handle_request", in)
	if err != nil {
		fails++
		fmt.Printf("FAIL call webhook: %v\n", err)
		return wsResponse{}
	}
	if exit != 0 {
		fails++
		fmt.Printf("FAIL call webhook: plugin exit code %d\n", exit)
		return wsResponse{}
	}
	var resp wsResponse
	if json.Unmarshal(out, &resp) != nil {
		fails++
		fmt.Printf("FAIL unmarshal webhook response: %v\n", err)
		return wsResponse{}
	}
	return resp
}

// runConfigTests covers the operator control routes (tk-swm): config
// round-trip with nil-keep secrets, blank-secret rejection, the manual
// tick, and state reset — including that an empty KV value (what
// handleResetState writes) behaves exactly like an absent key.
func runConfigTests(ctx context.Context, plugin *extism.Plugin) {
	fmt.Printf("\n--- admin config/control routes (tk-swm) ---\n")

	// Unconfigured: empty KV value behaves like absent.
	kv["shortcut:config"] = ""
	check("GET /config unconfigured", call(ctx, plugin, "GET", "/config", nil, ""), 200, `{}`)
	check("tick unconfigured → 400", call(ctx, plugin, "POST", "/sync/tick", nil, ""), 400, `{"error":"not configured — save the connection form first"}`)

	// Full save round-trip.
	save := `{"token":"tok2","enabled":true,"dry_run":true,"workspace_id":"demo","project_ids":[5],"label_mode":"merge","webhook_secret":"whsec2"}`
	check("POST /config valid", call(ctx, plugin, "POST", "/config", nil, save), 204, "")

	// An explicitly blank token is rejected rather than erasing the stored one.
	check("POST /config blank token → 400", call(ctx, plugin, "POST", "/config", nil, `{"token":"","enabled":true,"dry_run":true,"workspace_id":"demo"}`), 400, `{"error":"token is required"}`)

	// Omitted token/webhook_secret are nil-keep (the panel can't re-send
	// secrets it never sees).
	keep := `{"enabled":true,"dry_run":false,"actor_user_id":7,"workspace_id":"demo","project_ids":[5],"label_mode":"replace"}`
	check("POST /config keep secrets", call(ctx, plugin, "POST", "/config", nil, keep), 204, "")
	resp := call(ctx, plugin, "GET", "/config", nil, "")
	want(resp.StatusCode == 200, "GET /config after keep-save", fmt.Sprintf("code=%d", resp.StatusCode))
	want(strings.Contains(resp.Body, `"token_set":true`), "config: token kept", resp.Body)
	want(strings.Contains(resp.Body, `"webhook_secret_set":true`), "config: webhook secret kept", resp.Body)
	want(strings.Contains(resp.Body, `"label_mode":"replace"`), "config: label_mode updated", resp.Body)
	want(!strings.Contains(resp.Body, "tok2"), "config: token never echoed", resp.Body)

	// Manual tick ("Tick now" button).
	resp = call(ctx, plugin, "POST", "/sync/tick", nil, "")
	want(resp.StatusCode == 200, "POST /sync/tick", fmt.Sprintf("code=%d %s", resp.StatusCode, resp.Body))
	want(strings.Contains(resp.Body, `"counts"`), "tick: counts in body", resp.Body)

	// Reset clears state only; the empty KV value must read back as absent,
	// not corrupt — the next tick rebuilds from the watermark.
	check("POST /sync/reset", call(ctx, plugin, "POST", "/sync/reset", nil, ""), 204, "")
	resp = call(ctx, plugin, "GET", "/config", nil, "")
	want(!strings.Contains(resp.Body, `"state"`), "reset: state gone", resp.Body)
	resp = call(ctx, plugin, "POST", "/sync/tick", nil, "")
	want(resp.StatusCode == 200, "tick after reset (empty state = absent)", fmt.Sprintf("code=%d %s", resp.StatusCode, resp.Body))

	// Leave the canonical test config for any later sections.
	kv["shortcut:config"] = testConfigJSON
}
