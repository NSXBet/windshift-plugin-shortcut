# Plan: Shortcut → Windshift Continuous Sync (plugin-side, idempotent)

Created: 2026-10-04 · Status: implemented (v0.2.0)

## Problem

Windshift's built-in Jira import is a one-shot operator job. Shortcut stories change
constantly; a one-shot import would drift immediately. The sync must be **re-runnable
forever**: know what was already imported, update what changed, never duplicate.

## Constraints (from research)

- **Single change**: implementation lives only in `windshift-plugin-shortcut` (NSXBet-owned).
  No core fork divergence — fork-sync rebase stays clean.
- **Plugin runtime facts** (core v0.8.9 host ABI): schedules (≥1 min interval, 5 s
  invocation cap, resumable-page pattern exists in the repo), `http_fetch` (arbitrary
  outbound HTTP), `kv_get/set/delete` (durable, plugin-namespaced), `log`, `create_comment`.
  No host function can create items → all Windshift writes go through the core REST API
  via `http_fetch` loopback (`http://127.0.0.1:8080`; `--allow-local-connections=true` is
  the default).
- **Shortcut API v3**: `Shortcut-Token` header; 200 req/min, 429 without documented
  headers → budget client-side. Incremental reads via `POST /stories/search` body
  `updated_at_start/_end`. Container entities (projects, epics, workflow states,
  iterations, labels, milestones, members) have full-list endpoints with **no pagination
  and no updated_at filter** → re-fetch + KV-diff each cycle. Story `external_id` is a
  native free-form field → mirror our Windshift item id there. Hard deletes return 404.
  Webhooks fire on story/epic/project create/update/delete with HMAC-SHA256 signature.
- **Core REST surface** (verified): `POST /items` (`items:write`) accepts workspace, title,
  description, status_id, priority_id, item_type_id, parent_id, assignee_id, iteration,
  project, custom_fields, dates. `PUT /items/{id}` (no status change) + `POST
  /items/{id}/transition` (workflow-enforced). Comments: GET list, POST create (author =
  token user, no author override, no created_at override), `PUT /comments/{id}`,
  `DELETE /comments/{id}` (`items:delete`). `GET /users` strips emails for non-admin
  callers → email mapping requires an admin-scoped token (`GET /admin/users`).
- **Core semantics**: no tombstones; items hard-delete; statuses are globally name-unique;
  labels global case-insensitive unique; subtasks need parent to exist (Jira import solved
  with two-phase create→link); `ExternalItemReconciliationService` is source-whitelisted
  (GitHub only) → not usable from a plugin without core change → plugin drives REST only.

## Decision

**One-way continuous sync Shortcut → Windshift, driven by the plugin.** Windshift is the
mirror; Windshift-side edits do not propagate back (explicitly out of scope). Plugin owns
all state in KV; Shortcut and the core REST API are the only sources of truth.

### State layout (KV, plugin-namespaced)

| Key | Content | Purpose |
|---|---|---|
| `config` | `{ shortcut_token, core_base_url, core_token, project→workspace map, epic_workspace, item_type map (feature/bug/chore), priority label map, state-name map, member override map, webhook_secret, sync_enabled }` | Provisioned via admin-tab form (calls a plugin route that writes KV) |
| `cursor` | `{ story_watermark, in_flight_window, per-shard progress }` | Resumable delta position |
| `map/story/{shortcut_id}` | `{ windshift_item_id, last_synced_updated_at }` | Primary story map |
| `map/epic/{id}`, `map/comment/{id}`, `map/label/{id}`, `map/iteration/{id}`, `map/state/{id}` | resolved Windshift ids | Catalog maps |
| `member/{shortcut_id}` | `{ windshift_user_id }` or `{ unmapped: true }` | Member resolution cache |
| `meta/last_run` | stats for admin tab | Observability |

KV loss is recoverable: stories created by the sync carry `external_id: "windshift-item:<id>"`,
so a rebuild sweep (`GET /stories/search` paged by updated windows, read external_id)
reconstructs the story map. Idempotency never depends on KV alone.

### Sync cycle (schedule: every 1 min, budget ~150 requests/invocation, resumable)

1. **Catalog pass** (only when due: every cycle cheap subset, full every 15 min):
   re-fetch containers, diff against KV maps, resolve/create statuses (by name — global
   unique), labels (`EnsureLabel` semantics — find-or-create by name via REST), item types
   (config map), priorities (label-prefix map).
2. **Story delta**: `POST /stories/search {updated_at_start: watermark-overlap}` windowed
   (`updated_at_start`+`updated_at_end` slices, e.g. 24 h shards) to bound result sets;
   one or more shards per invocation; on completion advance cursor past overlap start.
   Overlap (1 h) + upsert = replays are harmless.
3. **Upsert per story** (in two passes inside the cycle):
   a. resolve target workspace (project map), status (state map), priority (label map),
      iteration, epic (epic map; epics synced as items in `epic_workspace`), type
      (story_type map), assignee (member map; unmapped → null + flagged).
   b. KV map hit + `last_synced_updated_at == story.updated_at` → skip (no-op guard).
   c. Map hit → `PUT /items/{id}` (+ transition when status changed); on 404 (item was
      hard-deleted Windshift-side) → recreate.
   d. No map → `POST /items` create, then set Shortcut `external_id = windshift-item:<id>`
      (one PUT /stories/{id}), then write KV map. Create-first order means a crash between
      create and external_id write is healed next cycle (duplicate detection below).
   e. Two-phase hierarchy: create stories unparented; parent/subtask linking pass runs
      after the upsert pass, keyed on story map (Jira-import pattern).
4. **Comments sub-sync**: per story, `GET /stories/{id}/comments` diff against `map/comment/*`
   (Shortcut comment ids in the fetch). Create (as bot author, text prefixed
   `[Shortcut] <name>: `), update via `PUT /comments/{id}`, soft-deletes: Shortcut
   `deleted: true` → `DELETE /comments/{id}` (`items:delete` scope).
5. **Deletes**: preferred — webhook receiver plugin route (`GET /api/plugins/shortcut/webhook`,
   HMAC-SHA256 verified with `webhook_secret`, 202-then-resync semantics → sets a
   "dirty" KV flag the next schedule tick acts on; deleted stories → `DELETE /items/{id}`
   guarded by map). Fallback when webhooks unreachable: weekly reconciliation sweep diffs
   the full story-id set against KV maps; stories absent from Shortcut → delete Windshift item.
6. **Rate budget**: in-memory token bucket, ≤150 Shortcut requests + ≤50 core requests per
   invocation; anything unprocessed stays in the cursor → next tick.

### Idempotency invariants

- Every write keyed on `(scope, shortcut_id)`; never key on title or key.
- Overlap-window watermark → replays converge to upsert.
- No-op guard: skip when story `updated_at` ≤ `last_synced_updated_at`.
- Duplicate guard on create: Shortcut search `external_id: "windshift-item:<id>"` before
  creating (covers the crash-window case); post-create external_id write covers the rest.
- Comment map + Shortcut comment list = complete state; a lost comment row re-creates
  (prefixed text is stable).
- Status/label/type resolution is find-or-create — no reliance on prior runs.

### Admin tab (`Shortcut Migration`, existing extension point)

- Connection form (tokens, base URL, webhook secret) → plugin route → KV.
- Mapping config: project→workspace picker, epic workspace, type/priority/state maps.
- Status panel: last run, cursor position, per-cycle stats, unmapped members list,
  delete-sweep state, "dry run" toggle (fetch+diff without writes) for first enablement.

### Risks / tradeoffs (accepted)

- Tokens live plaintext in plugin KV (only channel for plugin secrets). Admin-scoped core
  token needed for `/admin/users` email mapping; manual member override map as fallback.
- Comments authored by sync bot, not original member (REST cannot override author or
  created_at). Core-side host function would lift this — future core change, not now.
- Story links beyond parent/subtask (blocked-by, relates) have no Windshift equivalent →
  skipped v1; rendered as a trailing `[Shortcut links]` note in description only if
  configured.
- Windshift-side edits are overwritten by the next delta (mirror semantics) —
  documented; per-item "protected" config map (never overwrite list) as an escape hatch.
- 5 s invocation cap → big first import takes many ticks (rate-bound anyway); the
  resumable-page pattern in the repo is exactly for this.

## Phases

1. **Foundation** — config schema, KV wrappers, http_fetch client with rate budget +
   429 backoff, admin-tab connection form + route.
2. **Catalog sync** — container full-list diff, maps, find-or-create statuses/labels.
3. **Story sync** — delta cursor + windowing, upsert, external_id mirroring, two-phase
   parenting, no-op guards, dry-run mode.
4. **Comments** — create/update/soft-delete with map, author-prefixed text.
5. **Deletes** — webhook route (HMAC) + weekly reconciliation sweep fallback.
6. **UI + tests** — status panel, unmapped-member surfacing; integration tests exercising
   idempotency: run twice → zero duplicates; mutate → update; delete → tombstone.

## Verification

- Integration tests: two full cycles against a fixture → item count stable; story edit →
  update applied; story delete → item gone; comment edit → updated; restart-from-empty-KV
  → rebuild sweep reconstructs maps without duplicating items.
- Live check against a test Shortcut workspace + local core (compose stack).

## Out of scope

- Windshift → Shortcut back-sync (link/comment creation on Shortcut side).
- Attachments (Shortcut file attachments → Windshift upload API) — later phase candidate.
- Core changes of any kind (no fork divergence in this plan).
