# windshift-plugin-sample

A [Windshift](https://windshift.sh) plugin that stores Shortcut URLs as
bookmarks on work items — and doubles as the reference for building any
Windshift plugin: manifest + WASM + admin-tab UI, zero dependencies beyond the
Extism ABI, released as a GitHub release asset fetched by the Helm chart's
init container.

## What it demonstrates

| Surface | How |
|---|---|
| HTTP routes | `GET/POST/DELETE /api/plugins/shortcuts/...` declared in `plugin/manifest.json`, served by core's catch-all → `handle_request` export |
| Admin-tab UI extension | `admin.tab` point, iframe pointing at `/api/plugins/shortcuts/assets/index.html` (core serves the static assets itself) |
| Persistent state | `kv_get`/`kv_set` host functions — per-plugin KV, survives restarts |
| Release | `make build` → `dist/shortcuts.zip` (`manifest.json` + `plugin.wasm` + `assets/`), attached to a GitHub Release by CI |

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
curl -L https://github.com/NSXBet/windshift-plugin-sample/releases/latest/download/shortcuts.zip -o shortcuts.zip
mkdir -p /data/plugins/shortcuts
unzip shortcuts.zip -d /data/plugins/shortcuts
```

Or upload through Windshift's admin panel (Admin → Module Settings →
Plugins), which persists to `/data/plugins` on the data volume.

## Registry rules

- A plugin directory is any subdir of a plugin dir containing
  `manifest.json`; raw `.zip` files in the dir are skipped.
- Identity = `manifest.json`'s `name`. Routes not declared in the manifest
  or returned by `get_metadata` 404 even though the catch-all mounts.
- Limits per invocation: 5 s timeout, 64 MiB WASM memory.
