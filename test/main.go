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
		return body(`[{"id":1,"name":"Product","states":[{"id":11,"name":"In Progress","type":"started"}]}]`)
	case path == "/epic-workflows":
		return body(`[{"id":2,"name":"Epic WF","states":[{"id":21,"name":"Prioritized","type":"todo"}]}]`)
	case path == "/projects":
		return body(`[{"id":5,"name":"Demo","archived":false}]`)
	case path == "/epics":
		return body(`[]`)
	case path == "/stories/search":
		return body(searchStoriesJSON())
	case strings.HasPrefix(path, "/stories/") && strings.HasSuffix(path, "/comments"):
		switch strings.TrimSuffix(strings.TrimPrefix(path, "/stories/"), "/comments") {
		case "101":
			return body(`[
				{"id":9001,"story_id":101,"text":"hello","author_id":"77","created_at":"2026-10-01T10:00:00Z","updated_at":"2026-10-01T10:00:00Z","deleted":false,"position":0},
				{"id":9002,"story_id":101,"text":"gone","author_id":"77","created_at":"2026-10-01T11:00:00Z","updated_at":"2026-10-01T11:00:00Z","deleted":true,"position":1}
			]`)
		case "102":
			return body(`[]`)
		}
		return httpFetchResp{Status: 404, Body: []byte(`{"message":"story not found"}`)}
	case strings.HasPrefix(path, "/stories/"):
		id := strings.TrimPrefix(path, "/stories/")
		getStoryCalls = append(getStoryCalls, id)
		if deletedStories[id] {
			return httpFetchResp{Status: 404, Body: []byte(`{"message":"story not found"}`)}
		}
		return body(storyJSON(id))
	}
	return httpFetchResp{Status: 404, Body: []byte(`{"message":"unmocked: ` + req.URL + `"}`)}
}

// storyJSON returns a canonical GET /stories/:id body; the engine only
// inspects the status code in the sweep, but getStory parses the body.
func storyJSON(id string) string {
	at := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
	return fmt.Sprintf(`{"id":%s,"name":"S%s","description":"","story_type":"feature","workflow_state_id":11,"project_id":5,
		"app_url":"https://app.shortcut.com/demo/story/%s","created_at":%q,"updated_at":%q,"labels":[],"external_id":""}`,
		id, id, id, at, at)
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
	ExternalKind      string   `json:"external_kind"`
	ExternalID        int64    `json:"external_id"`
	ExternalURL       string   `json:"external_url,omitempty"`
	ExternalUpdatedAt string   `json:"external_updated_at,omitempty"`
	WorkspaceID       string   `json:"workspace_id"`
	Title             string   `json:"title"`
	StatusName        string   `json:"status_name"`
	ItemTypeName      string   `json:"item_type_name"`
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
	}
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
		})
	if err != nil {
		fmt.Printf("FAIL instantiate: %v\n", err)
		os.Exit(1)
	}
	defer plugin.Close(ctx)

	check("GET /status", call(ctx, plugin, "GET", "/status", nil, ""), 200, `{"kv":true,"plugin":"shortcut","version":"0.2.0"}`)
	check("GET /unmatched route", call(ctx, plugin, "GET", "/unmatched", nil, ""), 404, `{"error":"route not found: GET /unmatched"}`)

	runSyncE2E(ctx, plugin)
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
	Phase            string   `json:"phase"`
	StoryWindowStart string   `json:"story_window_start"`
	StoryWindowEnd   string   `json:"story_window_end"`
	StoryIdx         int      `json:"story_idx"`
	CommentIdx       int      `json:"comment_idx"`
	StorySweepIdx    int      `json:"story_sweep_idx"`
	LastWindowEnd    string   `json:"last_window_end,omitempty"`
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
		want(u.ItemTypeName == "Feature", "item_type_name", fmt.Sprintf("%q", u.ItemTypeName))
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

func resetE2E() {
	items = map[string]mockItem{}
	itemSeq, commentSeq = 0, 0
	upsertCalls, commentCalls = nil, nil
	deletedStories, getStoryCalls = map[string]bool{}, nil
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
