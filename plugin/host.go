// Host ABI surface for the Windshift plugin: every extism:host/user import
// (kv_get/kv_set/kv_delete, http_fetch, item_upsert/item_lookup), the JSON
// wire types exchanged with the Windshift core plugin manager
// (internal/plugins/types.go + host_functions.go, see
// .agents/plans/shortcut-v1-contracts.md), and thin typed wrappers.
//
// Wire rules (must match core exactly):
//   - single UTF-8 JSON payload in, JSON response out; a nil return pointer
//     means the host failed before producing a response
//   - http_fetch transport/parse failures surface as status 400 (bad
//     request) or 502 (network) with the error text in body; every other
//     status (including 404) is passed through to the caller
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

// --- extism:host/user imports ---

//go:wasmimport extism:host/user kv_get
func hostKVGet(req extismPointer) extismPointer

//go:wasmimport extism:host/user kv_set
func hostKVSet(req extismPointer) extismPointer

//go:wasmimport extism:host/user kv_delete
func hostKVDelete(req extismPointer) extismPointer

//go:wasmimport extism:host/user http_fetch
func hostHTTPFetch(req extismPointer) extismPointer

//go:wasmimport extism:host/user item_upsert
func hostItemUpsert(req extismPointer) extismPointer

//go:wasmimport extism:host/user item_lookup
func hostItemLookup(req extismPointer) extismPointer

//go:wasmimport extism:host/user create_comment
func hostCreateComment(req extismPointer) extismPointer

//go:wasmimport extism:host/user workspace_upsert
func hostWorkspaceUpsert(req extismPointer) extismPointer

//go:wasmimport extism:host/user member_upsert
func hostMemberUpsert(req extismPointer) extismPointer

//go:wasmimport extism:host/user iteration_upsert
func hostIterationUpsert(req extismPointer) extismPointer

//go:wasmimport extism:host/user purge_sync
func hostPurgeSync(req extismPointer) extismPointer

// --- KV wire (core: internal/plugins/host_functions.go) ---

type kvGetRequest struct {
	Key string `json:"key"`
}

type kvGetResponse struct {
	Status string `json:"status"` // "ok" | "not_found"
	Value  string `json:"value,omitempty"`
	Error  string `json:"error,omitempty"`
}

type kvSetRequest struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type kvSetResponse struct {
	Status string `json:"status"` // "ok" | "error"
	Error  string `json:"error,omitempty"`
}

type kvDeleteRequest struct {
	Key string `json:"key"`
}

type kvDeleteResponse struct {
	Status string `json:"status"` // "ok" | "error"
	Error  string `json:"error,omitempty"`
}

// callKV invokes a single-payload Windshift host function with req as the
// JSON request and returns the JSON response (nil on host-side failure).
func callKV(fn func(extismPointer) extismPointer, req []byte) []byte {
	reqPtr := allocBytes(req)
	defer freeBytes(reqPtr)
	respPtr := fn(reqPtr)
	if respPtr == 0 {
		return nil
	}
	defer freeBytes(respPtr)
	return readBytes(respPtr)
}

// kvGetString returns (value, found, err).
func kvGetString(key string) (string, bool, error) {
	raw := callKV(hostKVGet, mustJSON(kvGetRequest{Key: key}))
	if raw == nil {
		return "", false, fmt.Errorf("kv_get %q: host returned no payload", key)
	}
	var resp kvGetResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", false, fmt.Errorf("kv_get %q: %w", key, err)
	}
	switch resp.Status {
	case "ok":
		return resp.Value, true, nil
	case "not_found":
		return "", false, nil
	default:
		return "", false, fmt.Errorf("kv_get %q: %s", key, resp.Error)
	}
}

func kvSetString(key, value string) error {
	raw := callKV(hostKVSet, mustJSON(kvSetRequest{Key: key, Value: value}))
	if raw == nil {
		return fmt.Errorf("kv_set %q: host returned no payload", key)
	}
	var resp kvSetResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("kv_set %q: %w", key, err)
	}
	if resp.Status != "ok" {
		return fmt.Errorf("kv_set %q: %s", key, resp.Error)
	}
	return nil
}

func kvDelete(key string) error {
	raw := callKV(hostKVDelete, mustJSON(kvDeleteRequest{Key: key}))
	if raw == nil {
		return fmt.Errorf("kv_delete %q: host returned no payload", key)
	}
	var resp kvDeleteResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("kv_delete %q: %w", key, err)
	}
	if resp.Status != "ok" {
		return fmt.Errorf("kv_delete %q: %s", key, resp.Error)
	}
	return nil
}

// --- http_fetch wire (core: internal/plugins/types.go:79-92) ---

type httpFetchRequest struct {
	Method    string            `json:"method"`
	URL       string            `json:"url"`
	Headers   map[string]string `json:"headers,omitempty"`
	Body      []byte            `json:"body,omitempty"`
	TimeoutMs int               `json:"timeout_ms,omitempty"`
}

type httpFetchResponse struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    []byte            `json:"body,omitempty"`
}

// httpFetch performs one outbound HTTP call via the core host function.
// timeoutMs caps the whole call; the wasm invocation deadline is 5s, so
// callers keep this <= 2500ms (contract §2).
func httpFetch(req httpFetchRequest) (httpFetchResponse, error) {
	raw := callKV(hostHTTPFetch, mustJSON(req))
	if raw == nil {
		return httpFetchResponse{}, fmt.Errorf("http_fetch %s %s: host returned no payload", req.Method, req.URL)
	}
	var resp httpFetchResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return resp, fmt.Errorf("http_fetch %s %s: %w", req.Method, req.URL, err)
	}
	if resp.Status == 400 || resp.Status == 502 {
		return resp, fmt.Errorf("http_fetch %s %s: %s", req.Method, req.URL, truncateSnippet(string(resp.Body), 300))
	}
	return resp, nil
}

// --- item_upsert / item_lookup wire (contract §1) ---

type itemUpsertRequest struct {
	ExternalKind       string   `json:"external_kind"` // "story" | "epic"
	ExternalID         int64    `json:"external_id"`
	ExternalURL        string   `json:"external_url,omitempty"`
	ExternalUpdatedAt  string   `json:"external_updated_at,omitempty"` // RFC3339 UTC
	WorkspaceID        string   `json:"workspace_id"`
	Title              string   `json:"title"`
	Description        string   `json:"description,omitempty"`
	StatusName         string   `json:"status_name,omitempty"`
	ItemTypeName       string   `json:"item_type_name,omitempty"`
	PriorityName       string   `json:"priority_name,omitempty"`
	ProjectName        string   `json:"project_name,omitempty"` // find-only
	DueDate            string   `json:"due_date,omitempty"`     // YYYY-MM-DD
	StoryPoints        float64  `json:"story_points,omitempty"`
	Labels             []string `json:"labels,omitempty"`     // find-or-create
	LabelMode          string   `json:"label_mode,omitempty"` // "merge" | "replace" (default replace)
	ParentExternalKind string   `json:"parent_external_kind,omitempty"`
	ParentExternalID   int64    `json:"parent_external_id,omitempty"`
	// AssigneeExternalID maps to items.assignee_id (first Shortcut owner
	// wins; the column carries one; UUID string). IterationExternalID maps
	// to items.iteration_id — the core imports the member/iteration on demand.
	AssigneeExternalID  string `json:"assignee_external_id,omitempty"`
	IterationExternalID int64  `json:"iteration_external_id,omitempty"`
}

type itemUpsertResponse struct {
	Status  string `json:"status"`
	ItemID  string `json:"item_id,omitempty"`
	ItemKey string `json:"item_key,omitempty"`
	Created bool   `json:"created,omitempty"`
	Error   string `json:"error,omitempty"`
}

type itemLookupRequest struct {
	ExternalKind string `json:"external_kind"`
	ExternalID   int64  `json:"external_id"`
}

type itemLookupResponse struct {
	Status            string `json:"status"`
	Found             bool   `json:"found,omitempty"`
	ItemID            string `json:"item_id,omitempty"`
	ItemKey           string `json:"item_key,omitempty"`
	ExternalUpdatedAt string `json:"external_updated_at,omitempty"`
	LastSyncedAt      string `json:"last_synced_at,omitempty"`
	Error             string `json:"error,omitempty"`
}

func itemUpsert(req itemUpsertRequest) (itemUpsertResponse, error) {
	raw := callKV(hostItemUpsert, mustJSON(req))
	if raw == nil {
		return itemUpsertResponse{}, fmt.Errorf("item_upsert %s/%d: host returned no payload", req.ExternalKind, req.ExternalID)
	}
	var resp itemUpsertResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return resp, fmt.Errorf("item_upsert %s/%d: %w", req.ExternalKind, req.ExternalID, err)
	}
	if resp.Status != "ok" {
		return resp, &errItemRejected{Fn: "item_upsert", Ref: req.ExternalKind + "/" + strconv.FormatInt(req.ExternalID, 10), Msg: resp.Error}
	}
	return resp, nil
}

func itemLookup(kind string, id int64) (itemLookupResponse, error) {
	raw := callKV(hostItemLookup, mustJSON(itemLookupRequest{ExternalKind: kind, ExternalID: id}))
	if raw == nil {
		return itemLookupResponse{}, fmt.Errorf("item_lookup %s/%d: host returned no payload", kind, id)
	}
	var resp itemLookupResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return resp, fmt.Errorf("item_lookup %s/%d: %w", kind, id, err)
	}
	if resp.Status != "ok" {
		return resp, &errItemRejected{Fn: "item_lookup", Ref: kind + "/" + strconv.FormatInt(id, 10), Msg: resp.Error}
	}
	return resp, nil
}

// errItemRejected is an item-level host-function rejection: the host ran,
// parsed the request, and answered {"status":"error",...}. These are recorded
// and skipped (contract §6) instead of aborting the tick — e.g. an epic whose
// status_name core rejects must not livelock the schedule. Host/transport
// failures (nil payload, unparseable response) abort.
type errItemRejected struct {
	Fn  string
	Ref string
	Msg string
}

func (e *errItemRejected) Error() string { return e.Fn + " " + e.Ref + ": " + e.Msg }

// isRejected reports whether err carries an item-level host rejection.
func isRejected(err error) bool {
	var r *errItemRejected
	return errors.As(err, &r)
}

// --- create_comment wire (core: internal/plugins/types.go:189-201,
// pre-existing host function — no core patch dependency) ---

type createCommentRequest struct {
	ItemID                int    `json:"item_id"`
	AuthorID              int    `json:"author_id"`
	Content               string `json:"content"`
	SuppressNotifications bool   `json:"suppress_notifications"`
}

type createCommentResponse struct {
	Status    string `json:"status"`
	CommentID int    `json:"comment_id,omitempty"`
	Error     string `json:"error,omitempty"`
}

func createComment(req createCommentRequest) (createCommentResponse, error) {
	raw := callKV(hostCreateComment, mustJSON(req))
	if raw == nil {
		return createCommentResponse{}, fmt.Errorf("create_comment item %d: host returned no payload", req.ItemID)
	}
	var resp createCommentResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return resp, fmt.Errorf("create_comment item %d: %w", req.ItemID, err)
	}
	if resp.Status != "ok" {
		return resp, &errItemRejected{Fn: "create_comment", Ref: strconv.Itoa(req.ItemID), Msg: resp.Error}
	}
	return resp, nil
}

// --- workspace_upsert wire (core: internal/plugins/types.go WorkspaceUpsert*,
// contract §1) — find-or-create the target workspace for a Shortcut project ---

type workspaceUpsertState struct {
	Name string `json:"name"` // Shortcut state name, board column
	Type string `json:"type"` // "backlog"|"unstarted"|"started"|"done" → status category
}

type workspaceUpsertRequest struct {
	ExternalID   int64                  `json:"external_id"`          // Shortcut project id
	ExternalKind string                 `json:"external_kind"`        // "project" (core keys mappings by kind)
	Name         string                 `json:"name"`                 // project name, becomes the workspace name
	CreatorID    int                    `json:"creator_id,omitempty"` // Windshift user granted workspace admin
	States       []workspaceUpsertState `json:"states,omitempty"`     // team workflow states; kind "team" binds them as the workspace's workflow
}

type workspaceUpsertResponse struct {
	Status      string `json:"status"`
	WorkspaceID string `json:"workspace_id,omitempty"` // decimal string of the workspace id
	Created     bool   `json:"created,omitempty"`
	Error       string `json:"error,omitempty"`
}

func workspaceUpsert(req workspaceUpsertRequest) (workspaceUpsertResponse, error) {
	raw := callKV(hostWorkspaceUpsert, mustJSON(req))
	if raw == nil {
		return workspaceUpsertResponse{}, fmt.Errorf("workspace_upsert project %d: host returned no payload", req.ExternalID)
	}
	var resp workspaceUpsertResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return resp, fmt.Errorf("workspace_upsert project %d: %w", req.ExternalID, err)
	}
	if resp.Status != "ok" {
		return resp, &errItemRejected{Fn: "workspace_upsert", Ref: "project/" + strconv.FormatInt(req.ExternalID, 10), Msg: resp.Error}
	}
	return resp, nil
}

// --- purge wire (core: PurgeSync via internal/services/shortcut_sync_service.go) ---

type purgeSyncResponse struct {
	Status            string `json:"status"`
	Items             int    `json:"items,omitempty"`
	KVEntries         int64  `json:"kv_entries,omitempty"`
	Workspaces        int    `json:"workspaces,omitempty"`
	Statuses          int    `json:"statuses,omitempty"`
	Workflows         int    `json:"workflows,omitempty"`
	ConfigurationSets int    `json:"configuration_sets,omitempty"`
	Error             string `json:"error,omitempty"`
}

// purgeSync wipes every artifact the sync ever created in the core: item
// mappings (items cascade), plugin KV state, SC-* workspaces, team
// workflows/statuses/configuration sets. Operator-only (tk-migration).
func purgeSync() (purgeSyncResponse, error) {
	raw := callKV(hostPurgeSync, []byte(`{}`))
	if raw == nil {
		return purgeSyncResponse{}, fmt.Errorf("purge_sync: host returned no payload")
	}
	var resp purgeSyncResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return resp, fmt.Errorf("purge_sync: %w", err)
	}
	if resp.Status != "ok" {
		return resp, &errItemRejected{Fn: "purge_sync", Ref: pluginName, Msg: resp.Error}
	}
	return resp, nil
}

// --- member wire (core: UpsertMember, on-demand import at first assignment) ---

type memberUpsertRequest struct {
	ExternalID string `json:"external_id"` // Shortcut member id (UUID string)
	Name       string `json:"name,omitempty"`
	Email      string `json:"email,omitempty"` // real email when the member exposes it
	Username   string `json:"username,omitempty"`
}

type memberUpsertResponse struct {
	Status  string `json:"status"`
	UserID  string `json:"user_id,omitempty"`
	Created bool   `json:"created,omitempty"`
	Error   string `json:"error,omitempty"`
}

// memberUpsert imports one Shortcut member as an inactive-login windshift
// user. Item-level failures (schema mismatch) reject instead of aborting —
// a broken member profile must not wedge the story that referenced it.
func memberUpsert(req memberUpsertRequest) (memberUpsertResponse, error) {
	raw := callKV(hostMemberUpsert, mustJSON(req))
	if raw == nil {
		return memberUpsertResponse{}, fmt.Errorf("member_upsert %s: host returned no payload", req.ExternalID)
	}
	var resp memberUpsertResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return resp, fmt.Errorf("member_upsert %s: %w", req.ExternalID, err)
	}
	if resp.Status != "ok" {
		return resp, &errItemRejected{Fn: "member_upsert", Ref: req.ExternalID, Msg: resp.Error}
	}
	return resp, nil
}

// --- iteration wire (core: UpsertIteration, on-demand sprint import) ---

type iterationUpsertRequest struct {
	ExternalID int64  `json:"external_id"`
	Name       string `json:"name"`
	StartDate  string `json:"start_date,omitempty"`
	EndDate    string `json:"end_date,omitempty"`
	Status     string `json:"status,omitempty"`
	Workspace  string `json:"workspace"`
}

type iterationUpsertResponse struct {
	Status      string `json:"status"`
	IterationID string `json:"iteration_id,omitempty"`
	Created     bool   `json:"created,omitempty"`
	Error       string `json:"error,omitempty"`
}

// iterationUpsert imports one Shortcut iteration scoped to the story's
// workspace (windshift iterations are workspace-local). Item-level
// failures reject like members.
func iterationUpsert(req iterationUpsertRequest) (iterationUpsertResponse, error) {
	raw := callKV(hostIterationUpsert, mustJSON(req))
	if raw == nil {
		return iterationUpsertResponse{}, fmt.Errorf("iteration_upsert %d: host returned no payload", req.ExternalID)
	}
	var resp iterationUpsertResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return resp, fmt.Errorf("iteration_upsert %d: %w", req.ExternalID, err)
	}
	if resp.Status != "ok" {
		return resp, &errItemRejected{Fn: "iteration_upsert", Ref: strconv.FormatInt(req.ExternalID, 10), Msg: resp.Error}
	}
	return resp, nil
}

// itemIDInt parses an item_lookup/upsert item_id ("42") into the int the
// create_comment wire requires.
func itemIDInt(itemID string) (int, error) {
	n, err := strconv.Atoi(itemID)
	if err != nil {
		return 0, fmt.Errorf("item id %q: %w", itemID, err)
	}
	return n, nil
}

// truncateSnippet clamps error text embedded in wrapped errors so log lines
// stay bounded.
func truncateSnippet(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
