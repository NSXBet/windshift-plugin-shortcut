# windshift-plugin-shortcut

Shortcut → Windshift migration plugin. **Skeleton**: validates the
plugin-deployment path (build → GitHub release asset → Helm init container →
Windshift loads it). The actual Shortcut migration logic lands later; only
the liveness/KV-probe surface exists today.

## What ships now

| Surface | How |
|---|---|
| HTTP routes | `GET /api/plugins/shortcut/status` declared in `plugin/manifest.json`, served by core's catch-all → `handle_request` export |
| Admin-tab UI extension | `admin.tab` point, iframe pointing at `/api/plugins/shortcut/assets/index.html` (core serves the static assets itself) |
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
