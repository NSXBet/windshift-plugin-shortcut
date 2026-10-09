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
  "title": "Story title",                                    // plugin-sanitized (logic.TitleAndDescription): control runes collapse to spaces, trimmed; >255 runes cut to 255 with the full original prepended to the description as a `# ` markdown heading
  "description": "markdown verbatim",                        // optional, "" allowed; receives the full original title as heading when the title had to be cut
  "status_name": "In Development",                           // optional
  "item_type_name": "Story",                                 // always "Story" for stories (Windshift hierarchy: only level-2 Story may parent under a level-1 Epic; bug/task sit at level 3 and are rejected as epic children)
  "priority_name": "High",                                   // optional
  "project_name": "Web",                                     // optional, find-only
  "label_mode": "merge" | "replace",                         // optional (default "replace"); how "labels" combine with existing item labels
  "due_date": "2026-10-10",                                  // optional YYYY-MM-DD
  "story_points": 3,                                         // optional
  "labels": ["backend", "infra"],                            // optional, find-or-create
  "parent_external_kind": "epic",                            // optional
  "parent_external_id": 999,                                 // optional
  "assignee_external_id": "687a7ce8-ae4b-4528-975d-bfe3fdc8bca8", // optional, Shortcut member UUID (stories carry `owner_ids`, NOT `assignee_ids`); core resolves through `shortcut_sync_users`, imported on demand via `member_upsert`; a missing/deleted member silently drops the assignment rather than failing the story
  "iteration_external_id": 4242                              // optional, Shortcut sprint id; core resolves through `shortcut_sync_iterations` (workspace-scoped, imported on demand via `iteration_upsert`); the iteration's workspace must match the item's
}
```
Response: `{"status":"ok","item_id":"…","item_key":"…","created":true}` or `{"status":"error","error":"…"}`. `item_id` is a decimal string; `item_key` is `KEY-NUMBER`.
- Create path: `ExternalItemReconciliationService.Create` with `ShortcutReconciliationPolicy` (Source `shortcut`, `PublishLiveUpdates=false` — never notifies). `item_type_name` applies only at CREATE (core's update validator rejects item_type changes) — epics MUST set `"Epic"` on their first upsert of a given external id.
- Update path: mapping row must already exist (`external_kind`,`external_id`); otherwise error `not found`. Update fields, refresh `external_updated_at`/`last_synced_at` in the same tx.
- AfterCreate/AfterUpdate hook upserts `shortcut_sync_items`.

### `item_lookup`
Request: `{"external_kind":"story","external_id":123456}`
Response: `{"status":"ok","found":true,"item_id":"…","item_key":"…","external_updated_at":"…","last_synced_at":"…"}` or `{"status":"ok","found":false}`; malformed input → `{"status":"error","error":"…"}`.

### `member_upsert`
Import one Shortcut member as a windshift user so a story's assignee slot points at a real account. Plugin calls it on demand (first story carrying that owner), never as a roster pass. Request:
```json
{
  "external_id": "687a7ce8-ae4b-4528-975d-bfe3fdc8bca8",     // Shortcut member id — UUID STRING (member ids are not numeric)
  "name": "Dev Yuri",                                        // display name; splits into first/last
  "email": "yuri@nsx.dev",                                   // real email when public; else core synthesizes sync-member-<uuid>@shortcut.import
  "username": "dev.yuri"                                     // username hint for the handle
}
```
Response `{"status":"ok","user_id":"…","created":true}` or `{"status":"error","error":"…"}`. Re-upsert is idempotent (mapping keyed by `shortcut_sync_users.external_id` TEXT). Imported users own work but cannot log in until an admin resets credentials.

### `iteration_upsert`
Import a Shortcut sprint scoped to the story's resolved workspace (windshift iterations are per-workspace; UNIQUE(external_id, workspace_id)). Request:
```json
{
  "external_id": 4242,                                       // Shortcut iteration id (numeric)
  "name": "Sprint 42",
  "start_date": "2026-10-06",                                // optional YYYY-MM-DD
  "end_date": "2026-10-20",                                  // optional
  "state": "unstarted",                                      // optional passthrough
  "workspace_id": "51"                                       // decimal string; must match the story's workspace
}
```
Response `{"status":"ok","iteration_id":"…","created":true}` or `{"status":"error","error":"…"}`.

## 2. `http_fetch` (existing core ABI, `internal/plugins/types.go:79-92`)

`HTTPFetchRequest{url,method,headers,body,timeout_ms}`, `HTTPFetchResponse{status,headers,body}` — there is NO `error` field (core `internal/plugins/types.go` + `httpFetchHostFunction`): transport failures arrive as `status 502` (or `400` for a malformed request) with the error text in `body`. JSON-encoded, `[]byte` fields are std base64 (Go `encoding/json` on both sides). Plugin MUST set `timeout_ms ≤ 2500` (total wasm deadline 5s; ≤ 2 sequential fetches per story before re-checking tick budget).

## 3. KV keys (plugin-owned via `kv_get`/`kv_set`/`kv_delete`)
- `shortcut:config` → `{token, enabled, dry_run, workspace_mode:"single"|"team"|"per_project", workspace_id (single/epic-home), project_ids[], label_mode:"merge"|"replace", actor_user_id, webhook_secret}`
- `shortcut:state` → `{phase:"catalog"|"stories", story_window_start, story_window_end (RFC3339), story_idx, catalog_epic_idx, last_window_end (RFC3339; watermark for the next window), comment_idx (index into the frozen comment-pass story list), counts{created,updated,skipped,errors,comments}, last_errors[]}`
- `shortcut:cmt:<shortcut_comment_id>` → windshift comment id (dedup map, create-only)
- `shortcut:ws:<project_id>` (`project_id` = Shortcut project id as decimal) → `{ws_id (decimal string), fallback (bool)}` (per-project workspace map; v0.4.0 keeps resolution in-tick only)
- `shortcut:tomb:<kind>:<story_id>` (`kind` = `story`|`epic`) → `{deleted_at, source:"webhook"|"sweep"}` (skip these ids in every tick)

## 4. Shortcut API v3 usage (verified against developer.shortcut.com v3 docs, 2026-10-05)
Base `https://api.app.shortcut.com`, header `Shortcut-Token`.
- `POST /api/v3/stories/search` body `{project_ids?, updated_at_start?, updated_at_end?, includes_description?}` — response is a PLAIN `[StorySlim…]` array (HTTP 201), NO pagination fields, no offset/limit. Window resume = `story_idx` into the fetched array; window bounds are frozen in state across ticks.
- `GET /api/v3/stories/{id}` → full `Story` (has `description`). `GET /api/v3/stories/{id}/comments` → `[StoryComment…]` with `id, text, author_id, created_at, updated_at, deleted` (soft-deleted comments KEEP a row with `deleted:true` — comment removals are traceable without webhooks; story hard-deletes are NOT traceable → webhook §5).
- `GET /api/v3/projects`, `GET /api/v3/epics` (full-list container GETs; `/epics/paginated` exists but plain `/epics` returns all).
- `GET /api/v3/members/{uuid}` → `{id (UUID string), username, name, email}` — fetched on demand at first assignment; the email is present only for members who exposed it.
- Story `owner_ids` is the assignment field and carries **UUID strings** (`assignee_ids` is not a story field — reading it silently yields zero assignees). Iteration ids are numeric.
- 429 → honor `Retry-After` else sleep 2s (bounded); client budget 180 req/min sliding window.

## 5. Webhook
Route `POST /webhook/shortcut` exposed by core as `https://<core-host>/api/plugins/shortcut/webhook/shortcut` (transport unauthenticated; security = HMAC).
- `X-Signature` = HMAC-SHA256 hex over raw body with `config.webhook_secret`; constant-time compare; secret unset → feature disabled (401).
- Valid → 204, bad sig → 401, malformed → 400.
- story delete/archive event → tombstone: comment + KV record + skip in all future ticks.

## 6. Sync semantics (one-way, Shortcut → Windshift)
- Idempotency: `item_lookup` before every upsert; skip when `found && external_updated_at >= story.updated_at`.
- Guest clocks: the core instantiates every plugin with wazero `WithSysWalltime().WithSysNanotime()` — without it wazero serves a fake wall clock anchored at 2022-01-01 and a fake monotonic clock (1ms per reading), which silently disables the plugin's tick budget (`Remaining()` compares monotonic readings) and resumable cursors.
- Cursor advances only after the whole window commits; mid-window failure keeps cursor; next tick resumes same window.
- Tick budget ~4.2s of the 5s wasm deadline; exit cleanly, persist page position within window. The open window's search result is frozen into state as condensed story metas (`window_stories`), the `/epics` stream is frozen the same way (`catalog_epics`, condensed once per catalog epoch) and container maps are cached (`maps_json`), so each tick spends its budget on processing, not on re-fetching — a module killed by the wasm cap can no longer lose a freshly opened window. Epic bodies are fetched lazily (`GET /epics/{id}`) at their frozen position, mirroring the story pass.
- Catalog cursor duality: `catalog_epic_idx` means "stream elements consumed" while condensing and "frozen metas processed" while syncing. The condense drain (stream EOF) MUST reset it to 0 — otherwise the sync pass starts at the array's end, syncs zero epics, and the catalog reports done with an empty epic import (the live stack froze 1825 zero-id metas from this exact bug before the harness gained a default-pass condense scenario).
- Dry-run: count/log only, zero `item_upsert`/`create_comment`.
- Comments: create-only, dedup by KV map, author = `config.actor_user_id`, notifications suppressed.
- Epics: `item_type_name:"Epic"`; if core rejects, record error in state and continue.
- Assignee + sprint: before the story upsert, `owner_ids[0]` → `ensureMember` (on-demand `GET /members/{uuid}` + `member_upsert`) and `iteration_id` → `ensureIteration` (on-demand `GET /iterations/{id}` + `iteration_upsert` into the story's resolved workspace); failures of either are item-level and abort the story at its cursor (transport) or record an error (rejection), never the window.
- Deletion sweep: end-of-window fallback; initial-import mode (`backfill_days > 365`) skips it — every story in the window was created or canonically re-verified by the story pass moments earlier, so re-`GET`ing it doubles API cost for zero new information. Sweeps resume when the operator drops `backfill_days` to the operational value (≤365) after the migration.
- Parent epics: before a story upsert with `epic_id`, the plugin `item_lookup`s the parent; on a mapping miss (epic created after the catalog pass ran) it fetches `GET /epics/:id` and upserts it through the same catalog path — deleted-in-Shortcut epics (404/tombstone) import the child parentless instead of failing it forever.

## 7. Workspace mode (v0.4.0 / v0.6.0)
- `workspace_mode:"single"` (default) → every item upserts into `config.workspace_id` exactly as v0.2.0.
- `workspace_mode:"team"` (v0.6.0) → the org's real navigation unit. The catalog first upserts every Shortcut team (`GET /teams`, team → `workspace_upsert` with the team's workflow states). Story placement follows the story's workflow state → `workflow.team_id` → the team's workspace; unknown states fall back to `config.workspace_id`. Epics (org-level, team-less) always upsert into `config.workspace_id`; stories parent cross-workspace (core attaches the permission checker, the actor is an admin). This is the mode that makes the app look like Shortcut: Tech Development tasks sit in a "Tech Development" workspace whose board columns are the Tech Development workflow's states.
- `workspace_mode:"per_project"` → resolution per story: workspace = story's Shortcut project (`workspace_upsert` find-or-create by project id, cached per tick). Epics are org-level and always upsert into `config.workspace_id`. Unknown project (missing from `/projects`) falls back to `config.workspace_id`. Rejected by `workspace_upsert` → item-level error, state continues.
- Live-data note (nsx-bet, superseded): per_project was built on the assumption projects partition the org — they don't (no one uses Projects; 6 archived projects, projectless fallback dominated). Team mode replaces it as the intended mode.
### `workspace_upsert` (v0.4.0 / v0.6.0)
Request:
```json
{
  "external_id": 2196,          // Shortcut team or project id
  "external_kind": "team",      // "team" | "project" (core keys mappings by kind → SC-<KIND>-<id> workspace keys)
  "name": "Tech Development",   // team/project name, becomes the workspace name
  "creator_id": 7,              // optional; config.actor_user_id for created workspaces
  "states": [                   // v0.6.0, kind "team": the team's workflow states in board order
    {"name": "To Do", "type": "unstarted"},
    {"name": "In Progress", "type": "started"}
  ]
}
```
Response: `{"status":"ok","workspace_id":"42","created":true}` or `{"status":"error","error":"…"}`. `workspace_id` is a decimal string of the windshift workspace PK (or the fallback workspace id). Core: `ShortcutSyncService.UpsertWorkspace` — find-or-create by `(external_kind, external_id)` inside one tx (`external_kind` omitted = "team", back-compat with v0.3 payloads), `creator_id` only used on create. With `states` on kind "team": one-time `bindTeamWorkflow` — find-or-create a windshift workflow named after the team, find-or-create one status per state (category = state type: unstarted/backlog → to_do, started → in_progress, done → done) plus its transitions, then bind it to the workspace via a `configuration_sets` row + `workspace_configuration_sets` — so the workspace's board columns are the team's states (`WorkspaceService.GetStatuses` follows that binding). Already-bound workspaces no-op.

### `purge_sync` (v0.6.0)
Request `{}` (empty). Response `{"status":"ok","items":N,"kv_entries":N,"workspaces":N,"workflows":N,"statuses":N,"configuration_sets":N}` or `{"status":"error",...}`. Core: `ShortcutSyncService.PurgeSync` — one tx: delete `shortcut_sync_items` (items cascade), plugin KV state (`shortcut:state`/`cmt:*`/`tomb:*`; config survives), `SC-*` workspaces (fallback workspace survives), team workflows with no remaining workspace binding + their configuration sets + orphaned statuses. Purpose: full re-import from zero without operator DB surgery. Plugin route `POST /sync/purge` (operator, admin UI "Purge sync" button with confirm).
