# windshift-plugin-shortcut

Shortcut → Windshift migration plugin. One-way continuous sync: Shortcut
stories and comments are imported into a Windshift workspace incrementally
(resumable 24 h windows over an advancing watermark), container entities
(epics, projects, workflow states) are synced first as the mapping basis, and
Shortcut-side deletes are propagated as tombstone comments via a signed
webhook plus a per-window deletion sweep. Operators configure and monitor the
sync from an admin-tab panel.

## What ships now

| Surface | How |
|---|---|
| Scheduled sync | `sync_tick` every 5 m (manifest schedule): catalog pass → windowed story import → comment pass → deletion sweep; resumable cursors in per-plugin KV |
| Delete webhook | `POST /webhook/shortcut` — HMAC-SHA256 (`Payload-Signature`) verified; story delete/archive → tombstone comment + KV marker |
| Operator routes | `GET/POST /config` (secrets write-only), `POST /sync/tick`, `POST /sync/reset`, `GET /status` |
| Admin-tab UI | `admin.tab` point, iframe at `/api/plugins/shortcut/assets/index.html` — status panel + config form + tick/reset buttons |
| Persistent state | `kv_get`/`kv_set` host functions — per-plugin KV, survives restarts |
| Release | `task build` → `dist/shortcut.zip` (`manifest.json` + `plugin.wasm` + `assets/`), attached to a GitHub Release by CI |

## Building

Standard Go ≥ 1.24 — no TinyGo, no dependencies. The reactor build mode is
required (`-buildmode=c-shared`): Windshift's Extism runtime skips `_start`
and initializes via `_initialize`.

Tools are pinned via mise (`.mise.toml`): run `mise install` once, then
`go`/`task` resolve to the pinned versions in every shell.

```sh
task verify   # builds dist/shortcut.zip, then runs the wasm against wazero + mock host functions
```

The test harness (`test/`) instantiates the built wasm with the exact stack
Windshift embeds (`github.com/extism/go-sdk` v1.7.1 on wazero) and exercises
every route, including the KV host-function round-trip. It catches the two
failure modes that are invisible at compile time: a missing `handle_request`
export and a command-module build (runtime never initialized).

## Plugin ABI (what this repo encodes)

- **Exports** (standard Go: `//go:wasmexport`): `handle_request` is the only
  required one. `get_metadata`/`get_routes` are optional — routes and the
  extension live in `manifest.json`.
- **Request in**: `{method, path, headers, body, query, params}`
- **Response out**: `{statusCode, headers, body}` (body is a string)
- **Windshift host functions** live in the `extism:host/user` namespace:
  single-pointer ABI — offset of a JSON request in, offset of a JSON response
  out (`0` on failure). `plugin/wasm.go` hand-rolls the minimal
  `extism:host/env` shim (go-pdk's memory package is internal and
  unimportable) plus `kv_get`/`kv_set` imports.

## Install

Drop the release zip's contents into a plugin directory:

```sh
curl -L https://github.com/NSXBet/windshift-plugin-shortcut/releases/latest/download/shortcut.zip -o shortcut.zip
mkdir -p /data/plugins/shortcut
unzip shortcut.zip -d /data/plugins/shortcut
```

Or upload through Windshift's admin panel (Admin → Module Settings →
Plugins), which persists to `/data/plugins` on the data volume.

## Configuring

Open **Integrations → Shortcut Migration** in Windshift's admin, or drive the
same routes directly (`GET/POST /api/plugins/shortcut/config`). Fields:

| Field | Meaning |
|---|---|
| `token` | Shortcut API token (write-only; save with the field blank to keep the stored one) |
| `enabled` | Master switch — a disabled plugin skips ticks |
| `dry_run` | Read-only mode: everything is fetched and counted, nothing is written to Windshift |
| `workspace_id` | Target Windshift workspace id |
| `project_ids` | Shortcut projects to sync (empty = all) |
| `label_mode` | `merge` (add to existing labels) or `replace` |
| `actor_user_id` | Windshift user id that authors imported comments — required unless dry-run |
| `webhook_secret` | Shortcut outgoing-webhook signing secret (write-only, same blank-keeps rule) |
| `backfill_days` | How far before now the first window opens (1–365, default 30) |

Secrets are never echoed back — `GET /config` reports only
`token_set`/`webhook_secret_set` booleans.

## Running the sync

- The manifest schedule ticks every 5 minutes; each tick processes as much as
  its ~4 s budget allows and parks its cursors, so a slow initial backfill
  resumes tick after tick.
- **Tick now** (`POST /sync/tick`) runs one tick inline and returns the
  resulting counts; **Reset state** (`POST /sync/reset`) clears cursors only —
  already-created items stay, and the next tick rebuilds from the watermark.
- Deletes: register `https://<windshift>/api/plugins/shortcut/webhook/shortcut`
  as a Shortcut outgoing webhook. Delete/archive events tombstone the mapped
  item immediately; the end-of-window sweep re-verifies every mapped story of
  a window with a canonical GET and tombstones anything Shortcut has
  hard-deleted without a webhook. Tombstoned items are commented and skipped
  thereafter.

## Registry rules

- A plugin directory is any subdir of a plugin dir containing
  `manifest.json`; raw `.zip` files in the dir are skipped.
- Identity = `manifest.json`'s `name`. Routes not declared in the manifest
  or returned by `get_metadata` 404 even though the catch-all mounts.
- Limits per invocation: 5 s timeout, 64 MiB WASM memory.

## Local dev loop

Full-loop plugin development against the NSXBet fork's public core image
(`ghcr.io/nsxbet/windshift:v0.8.9-p1` — upstream `v0.8.9` + one backported
fix: the CSP change that lets plugin admin-tab JS execute; upstream v0.8.9
serves plugin assets with `default-src 'none'; sandbox`, which breaks any
published tag) on localhost —
no cluster, no image pushes, no release cut per iteration.

Prereqs: Apple `container` runtime + `mise install` (pins go/task).

```sh
task install      # place the built plugin into the container-mounted plugin dir (.dev/plugins)
task dev          # boot on :8080 — the boot scan registers every plugin found on disk
task setup        # first-run: bootstrap admin (dev / windshift-dev-1), cookie in .dev/cookie
```

Plugin registration happens at container boot (disk scan of `/plugins`), so
`install` must precede `dev`. After that, the hot loop never restarts the
container:

```sh
task build install reload   # ~2s: rebuild zip → swap files → POST /reload — same bits, no restart
```

Details worth knowing:

- **Asset edits need no reload**: core reads plugin assets from disk on every
  request — edit `plugin/assets/*`, then just refresh the browser tab. Only
  wasm/manifest changes need `reload`.
- **Test harness pins the version**: `test/main.go` asserts the manifest
  version, so bump it there together with `plugin/manifest.json` or `verify`
  fails.
- **Admin-tab scripts must be external files**: core serves plugin HTML under
  the app CSP (`script-src 'self'` + per-response nonce) — inline `<script>`
  is silently blocked. Keep logic in `assets/*.js`; `verify` asserts every
  file in `plugin/assets/` ships in the zip.
- **Core v0.8.9 limitation**: it serves plugin tab documents with a
  `sandbox` CSP, so tab JavaScript does not execute there (platform posture,
  since removed in core main). Routes, KV ABI, reload, and tab registration
  all work against the pinned image.
- The container runs as root (`compose.yaml`) because the image's default uid
  65534 cannot create the SQLite file in the root-owned data volume. Dev
  only; the prod chart sets its own security context.

Pristine reset — removes the container, the SQLite volume, and all build
output:

```sh
task clean
```

## Releasing

1. Bump `plugin/manifest.json`'s `version`.
2. Tag the same version and push: `git tag v0.2.0 && git push --tags`.
3. CI builds, runtime-tests, and attaches `shortcut.zip` + `checksums.txt`
   to the release. The tag must match the manifest version.
