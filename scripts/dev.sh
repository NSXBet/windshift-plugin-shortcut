#!/usr/bin/env bash
# Local plugin dev loop: run a real windshift core (built from source) against
# the locally built shortcut plugin. SQLite only — no containers, no cluster.
#
#   make dev          # first run: clones core, builds frontend + binary, starts :7777
#   make dev          # later runs: reuses builds, re-installs plugin, starts :7777
#   make dev REBUILD=1  # force core frontend/binary rebuild after pulling core
#
# While it's running: `make dev-reload` rebuilds the plugin and hot-reloads it
# with no server restart. State lives in .dev/ (gitignored): DB, binary, mounts.
#
# Overrides (env): WINDSHIFT_CORE_DIR, WINDSHIFT_DEV_PORT, WINDSHIFT_DEV_EMAIL,
# WINDSHIFT_DEV_USERNAME, WINDSHIFT_DEV_PASSWORD.
set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEV_DIR="$REPO_DIR/.dev"
CORE_DIR="${WINDSHIFT_CORE_DIR:-$REPO_DIR/../windshift-core}"
PORT="${WINDSHIFT_DEV_PORT:-7777}"
PLUGIN_NAME="shortcut"
PLUGIN_MOUNT="$DEV_DIR/plugins"
DB_PATH="$DEV_DIR/windshift.db"
EMAIL="${WINDSHIFT_DEV_EMAIL:-dev@windshift.local}"
USERNAME="${WINDSHIFT_DEV_USERNAME:-dev}"
PASSWORD="${WINDSHIFT_DEV_PASSWORD:-windshift-dev-1}"
BASE="http://localhost:$PORT"

[ -f "$REPO_DIR/dist/$PLUGIN_NAME.zip" ] || { echo "dist/$PLUGIN_NAME.zip missing — run: make build" >&2; exit 1; }

cd "$REPO_DIR"
mkdir -p "$DEV_DIR"
REBUILD="${REBUILD:-0}"

# --- core: clone once, build only what's missing --------------------------------
if [ ! -d "$CORE_DIR" ]; then
  echo "==> cloning windshift core into $CORE_DIR"
  git clone https://github.com/Windshiftapp/core.git "$CORE_DIR"
fi

NEED_FRONTEND=0 NEED_GO=0
if [ "$REBUILD" = 1 ]; then
  NEED_FRONTEND=1 NEED_GO=1
fi
[ -f "$DEV_DIR/windshift" ] || NEED_GO=1
if [ "$NEED_GO" = 1 ] && [ "$NEED_FRONTEND" = 0 ]; then
  # go:embed requires frontend/dist at build time; the binary embeds whatever is there.
  [ -d "$CORE_DIR/frontend/dist" ] && [ -n "$(ls -A "$CORE_DIR/frontend/dist" 2>/dev/null)" ] || NEED_FRONTEND=1
fi

# Core pins its toolchain (package.json engines: node 24.18.0, npm 11.16.0;
# .nvmrc mirrors node). `mise exec` swaps its tool dirs in place in PATH when
# they already exist mid-path, so in bare (non-activated) shells an earlier
# /opt/homebrew/bin node shadows the pin. Build PATH manually instead.
NODE_PIN=24.18.0
NPM_PIN=11.16.0
GO_PIN=1.27.0
if command -v mise >/dev/null 2>&1; then
  NODE_BIN="$(mise where "node@$NODE_PIN")/bin"
  NPM_BIN="$(mise where "npm@$NPM_PIN")/package/bin"
  mise install "go@$GO_PIN" >/dev/null
  GO_BIN="$(mise where "go@$GO_PIN")/bin"
  tool_npm() { PATH="$NPM_BIN:$NODE_BIN:$PATH" "$@"; }
  # A global GOROOT env (mise go 1.26.6) poisons even the 1.27 binary's std
  # resolution ("uuid is not in std" — uuid landed in 1.27 std). Go derives
  # GOROOT from its binary path when the env is unset.
  tool_go() { PATH="$GO_BIN:$PATH" GOROOT= "$@"; }
else
  tool_npm() { "$@"; }
  tool_go() { "$@"; }
fi

if [ "$NEED_FRONTEND" = 1 ]; then
  echo "==> building core frontend (one-time, ~1min)"
  (cd "$CORE_DIR/frontend" && tool_npm npm ci --no-audit --no-fund && tool_npm npm run build)
fi
if [ "$NEED_GO" = 1 ]; then
  [ -d "$CORE_DIR/frontend/dist" ] && [ -n "$(ls -A "$CORE_DIR/frontend/dist" 2>/dev/null)" ] \
    || { echo "core frontend/dist is empty — run: make dev REBUILD=1" >&2; exit 1; }
  echo "==> building core binary"
  (cd "$CORE_DIR" && tool_go go build -o "$DEV_DIR/windshift" .)
fi

# --- plugin: install the freshly built zip --------------------------------------
# layout: $PLUGIN_MOUNT/$PLUGIN_NAME/{manifest.json,plugin.wasm,assets/}
stage="$(mktemp -d)"
trap 'rm -rf "$stage"' EXIT
unzip -q "dist/$PLUGIN_NAME.zip" -d "$stage"
mkdir -p "$PLUGIN_MOUNT"
rm -rf "$PLUGIN_MOUNT/$PLUGIN_NAME"
mv "$stage" "$PLUGIN_MOUNT/$PLUGIN_NAME"

# --- server ---------------------------------------------------------------------
if [ -f "$DEV_DIR/server.pid" ]; then
  pid="$(cat "$DEV_DIR/server.pid" 2>/dev/null || true)"
  if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
    echo "==> stopping previous dev server (pid $pid)"
    kill "$pid"
    for _ in $(seq 1 20); do kill -0 "$pid" 2>/dev/null || break; sleep 0.5; done
  fi
  rm -f "$DEV_DIR/server.pid"
fi

printf 'PORT=%s\nPLUGIN_NAME=%s\n' "$PORT" "$PLUGIN_NAME" > "$DEV_DIR/env"

export SSO_SECRET="${SSO_SECRET:-dev-secret-for-testing}"
export PLUGIN_DIRS="$PLUGIN_MOUNT"

echo "==> starting windshift on :$PORT (SQLite: $DB_PATH)"
"$DEV_DIR/windshift" \
  -port "$PORT" \
  -db "$DB_PATH" \
  -attachment-path "$DEV_DIR/data" \
  -no-csrf \
  -log-level info &
SERVER_PID=$!
echo "$SERVER_PID" > "$DEV_DIR/server.pid"
trap 'kill "$SERVER_PID" 2>/dev/null || true; rm -f "$DEV_DIR/server.pid"' EXIT

for _ in $(seq 1 120); do
  curl -sf "$BASE/api/setup/status" >/dev/null 2>&1 && break
  kill -0 "$SERVER_PID" 2>/dev/null || { echo "!! server exited during startup — see its log output above" >&2; exit 1; }
  sleep 0.5
done
curl -sf "$BASE/api/setup/status" >/dev/null || { echo "!! server not reachable at $BASE" >&2; exit 1; }

STATUS="$(curl -sf "$BASE/api/setup/status")"
if [ "$(printf '%s' "$STATUS" | python3 -c 'import json,sys;print(json.load(sys.stdin)["setup_completed"])')" = "False" ]; then
  echo "==> fresh DB: bootstrapping admin user ($USERNAME / $EMAIL)"
  PAYLOAD="$(python3 - "$EMAIL" "$USERNAME" "$PASSWORD" <<'PY'
import json, sys
print(json.dumps({"admin_user": {"email": sys.argv[1], "username": sys.argv[2], "first_name": "Local", "last_name": "Dev", "language": "en", "password": sys.argv[3]}, "module_settings": {}}))
PY
)"
  curl -sf -X POST "$BASE/api/setup/complete" -H 'Content-Type: application/json' -d "$PAYLOAD" >/dev/null
fi

echo ""
echo "windshift dev server:   $BASE"
echo "admin login:            $USERNAME or $EMAIL / password: $PASSWORD"
echo "admin tab:              $BASE/admin/shortcut-plugin"
echo "plugin mounted from:    $PLUGIN_MOUNT/$PLUGIN_NAME"
echo "after plugin edits:     make dev-reload   (no server restart needed)"
echo "Ctrl+C stops the server."
wait "$SERVER_PID"
