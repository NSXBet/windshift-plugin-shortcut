#!/usr/bin/env bash
# Hot-reload the shortcut plugin into a running dev server, without restarting:
# rebuilds the zip (verify), re-installs it into .dev/plugins/, reloads via the
# admin API, then probes the plugin's live /status route.
#
#   make dev-reload
set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEV_DIR="$REPO_DIR/.dev"
[ -f "$DEV_DIR/server.pid" ] && kill -0 "$(cat "$DEV_DIR/server.pid")" 2>/dev/null \
  || { echo "dev server not running — run: make dev" >&2; exit 1; }

[ -f "$DEV_DIR/env" ] && . "$DEV_DIR/env"
PORT="${PORT:-7777}"
PLUGIN_NAME="${PLUGIN_NAME:-shortcut}"
BASE="http://localhost:$PORT"

echo "==> rebuilding plugin"
make -s verify

stage="$(mktemp -d)"
trap 'rm -rf "$stage"' EXIT
unzip -q "dist/$PLUGIN_NAME.zip" -d "$stage"
rm -rf "$DEV_DIR/plugins/$PLUGIN_NAME"
mkdir -p "$DEV_DIR/plugins"
mv "$stage" "$DEV_DIR/plugins/$PLUGIN_NAME"

echo "==> reloading via admin API"
curl -sf -c "$DEV_DIR/.cookie" -X POST "$BASE/api/auth/login" \
  -H 'Content-Type: application/json' \
  -d "$(python3 - "$PORT" <<'PY'
import json, sys, os
print(json.dumps({"email_or_username": os.environ.get("WINDSHIFT_DEV_USERNAME", "dev"),
                  "password": os.environ.get("WINDSHIFT_DEV_PASSWORD", "windshift-dev-1")}))
PY
)" >/dev/null
curl -sf -b "$DEV_DIR/.cookie" -X POST "$BASE/api/plugins/$PLUGIN_NAME/reload"

echo ""
echo "==> live probe"
curl -s "$BASE/api/plugins/$PLUGIN_NAME/status"
echo ""
echo "reload the browser tab to pick up asset changes: $BASE/admin/$PLUGIN_NAME-plugin"
