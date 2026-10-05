# Shortcut → Windshift Sync v1 — ABI & Wire Contracts

Shared contract between the core patch (repo `/Users/yuri/Workdir/Nsx/windshift-core-internal`, branch `internal/v0.8.9`) and the Shortcut plugin (this repo, `plugin/`). Both sides MUST match these wire formats exactly. Parent plan: `shortcut-import-sync.md`.

## 1. New host functions (core, namespace `extism:host/user` — same default namespace as `kv_get`/`kv_set`/`http_fetch`/`kv_delete`; `extism.NewHostFunctionWithStack` without options lands there)

Wire = UTF-8 JSON strings over extism host-function input/output (same as existing `create_comment` in `internal/plugins/host_functions.go`).

### `item_upsert`
Request:
```json
{
  "external_kind": "story",                                  // "story" | "epic"
  "external_id": 123456,                                     // Shortcut id, number
  "external_url": "https://app.shortcut.com/<ws>/story/123456",
  "external_updated_at": "2026-10-05T01:02:03Z",             // RFC3339 UTC
  "workspace_id": "1",                                       // decimal string of the workspace id (workspaces are int-PK; core parses, no uuid)
  "title": "Story title",
  "description": "markdown verbatim",                        // optional, "" allowed
  "status_name": "In Development",                           // optional
  "item_type_name": "Feature",                               // optional
  "priority_name": "High",                                   // optional
  "project_name": "Web",                                     // optional, find-only
  "label_mode": "merge" | "replace",                         // optional (default "replace"); how "labels" combine with existing item labels
  "due_date": "2026-10-10",                                  // optional YYYY-MM-DD
  "story_points": 3,                                         // optional
  "labels": ["backend", "infra"],                            // optional, find-or-create
  "parent_external_kind": "epic",                            // optional
  "parent_external_id": 999                                  // optional
}
```
Response: `{"status":"ok","item_id":"…","item_key":"…","created":true}` or `{"status":"error","error":"…"}`. `item_id` is a decimal string; `item_key` is `KEY-NUMBER`.
- Create path: `ExternalItemReconciliationService.Create` with `ShortcutReconciliationPolicy` (Source `shortcut`, `PublishLiveUpdates=false` — never notifies). `item_type_name` applies only at CREATE (core's update validator rejects item_type changes) — epics MUST set `"Epic"` on their first upsert of a given external id.
- Update path: mapping row must already exist (`external_kind`,`external_id`); otherwise error `not found`. Update fields, refresh `external_updated_at`/`last_synced_at` in the same tx.
- AfterCreate/AfterUpdate hook upserts `shortcut_sync_items`.

### `item_lookup`
Request: `{"external_kind":"story","external_id":123456}`
Response: `{"status":"ok","found":true,"item_id":"…","item_key":"…","external_updated_at":"…","last_synced_at":"…"}` or `{"status":"ok","found":false}`; malformed input → `{"status":"error","error":"…"}`.

## 2. `http_fetch` (existing core ABI, `internal/plugins/types.go:79-92`)

`HTTPFetchRequest{url,method,headers,body,timeout_ms}`, `HTTPFetchResponse{status,headers,body}` — there is NO `error` field (core `internal/plugins/types.go` + `httpFetchHostFunction`): transport failures arrive as `status 502` (or `400` for a malformed request) with the error text in `body`. JSON-encoded, `[]byte` fields are std base64 (Go `encoding/json` on both sides). Plugin MUST set `timeout_ms ≤ 2500` (total wasm deadline 5s; ≤ 2 sequential fetches per story before re-checking tick budget).

## 3. KV keys (plugin-owned via `kv_get`/`kv_set`/`kv_delete`)
- `shortcut:config` → `{token, enabled, dry_run, workspace_id, project_ids[], label_mode:"merge"|"replace", actor_user_id, webhook_secret}`
- `shortcut:state` → `{phase:"catalog"|"stories", story_window_start, story_window_end (RFC3339), story_idx, catalog_epic_idx, last_window_end (RFC3339; watermark for the next window), comment_idx (index into the frozen comment-pass story list), counts{created,updated,skipped,errors,comments}, last_errors[]}`
- `shortcut:cmt:<shortcut_comment_id>` → windshift comment id (dedup map, create-only)
- `shortcut:tomb:<kind>:<story_id>` (`kind` = `story`|`epic`) → `{deleted_at, source:"webhook"|"sweep"}` (skip these ids in every tick)

## 4. Shortcut API v3 usage (verified against developer.shortcut.com v3 docs, 2026-10-05)
Base `https://api.app.shortcut.com`, header `Shortcut-Token`.
- `POST /api/v3/stories/search` body `{project_ids?, updated_at_start?, updated_at_end?, includes_description?}` — response is a PLAIN `[StorySlim…]` array (HTTP 201), NO pagination fields, no offset/limit. Window resume = `story_idx` into the fetched array; window bounds are frozen in state across ticks.
- `GET /api/v3/stories/{id}` → full `Story` (has `description`). `GET /api/v3/stories/{id}/comments` → `[StoryComment…]` with `id, text, author_id, created_at, updated_at, deleted` (soft-deleted comments KEEP a row with `deleted:true` — comment removals are traceable without webhooks; story hard-deletes are NOT traceable → webhook §5).
- `GET /api/v3/projects`, `GET /api/v3/epics` (full-list container GETs; `/epics/paginated` exists but plain `/epics` returns all).
- 429 → honor `Retry-After` else sleep 2s (bounded); client budget 180 req/min sliding window.

## 5. Webhook
Route `POST /webhook/shortcut` exposed by core as `https://<core-host>/api/plugins/shortcut/webhook/shortcut` (transport unauthenticated; security = HMAC).
- `X-Signature` = HMAC-SHA256 hex over raw body with `config.webhook_secret`; constant-time compare; secret unset → feature disabled (401).
- Valid → 204, bad sig → 401, malformed → 400.
- story delete/archive event → tombstone: comment + KV record + skip in all future ticks.

## 6. Sync semantics (one-way, Shortcut → Windshift)
- Idempotency: `item_lookup` before every upsert; skip when `found && external_updated_at >= story.updated_at`.
- Cursor advances only after the whole window commits; mid-window failure keeps cursor; next tick resumes same window.
- Tick budget ~4.2s of the 5s wasm deadline; exit cleanly, persist page position within window.
- Dry-run: count/log only, zero `item_upsert`/`create_comment`.
- Comments: create-only, dedup by KV map, author = `config.actor_user_id`, notifications suppressed.
- Epics: `item_type_name:"Epic"`; if core rejects, record error in state and continue.
