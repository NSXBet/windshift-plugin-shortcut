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
| Release | `make build` → `dist/shortcut.zip` (`manifest.json` + `plugin.wasm` + `assets/index.html`), attached to a GitHub Release by CI |

## Building

Standard Go ≥ 1.24 — no TinyGo, no dependencies. The reactor build mode is
required (`-buildmode=c-shared`): Windshift's Extism runtime skips `_start`
and initializes via `_initialize`.

```sh
make verify   # builds, then runs the wasm against wazero + mock host functions
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

Full-loop plugin development against a real Windshift instance on localhost —
no cluster, no image pushes, no release cut per iteration.

```sh
make dev           # one-time: builds core (frontend+binary), boots :7777, seeds admin, installs plugin
make dev-reload    # the hot loop: rebuild zip → reinstall → POST reload → probe   (~3.5s)
make dev-clean     # stop server, remove .dev/
```

`make dev` clones `Windshiftapp/core` to `../windshift-core` if absent, builds
its frontend + binary (pinned toolchain via mise: node 24.18.0 / npm 11.16.0 /
go 1.27.0 — matching core's engines and go.mod), runs it with SQLite on
`:7777`, completes first-run setup automatically, and logs in as
`dev` / `windshift-dev-1`. Plugin assets are installed from `dist/` into
`.dev/plugins/shortcut/`; the server loads them from there via `PLUGIN_DIRS`.

Details worth knowing:

- **Asset edits need no reload**: core reads plugin assets from disk on every
  request — edit `plugin/assets/*`, then just refresh the browser tab.
  Wasm/manifest changes need `make dev-reload`.
- **Test harness pins the version**: `test/main.go` asserts the manifest
  version, so bump it there together with `plugin/manifest.json` or
  `dev-reload`'s `verify` gate rejects the build.
- **Admin-tab scripts must be external files**: core serves plugin HTML under
  the app CSP (`script-src 'self'` + per-response nonce) — inline `<script>`
  is silently blocked. Keep logic in `assets/*.js`; the Makefile verify gate
  asserts every file in `plugin/assets/` ships in the zip.
- `dev.sh` clears `GOROOT` when invoking go: a stale global `GOROOT` (e.g.
  mise go 1.26) poisons even a 1.27 binary's std-lib resolution.

## Releasing

1. Bump `plugin/manifest.json`'s `version`.
2. Tag the same version and push: `git tag v0.2.0 && git push --tags`.
3. CI builds, runtime-tests, and attaches `shortcut.zip` + `checksums.txt`
   to the release. The tag must match the manifest version.
