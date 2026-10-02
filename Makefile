# Builds dist/shortcut.zip — the release artifact:
#   manifest.json  plugin.wasm  assets/index.html
PLUGIN_NAME := shortcut

.PHONY: build verify clean dev dev-reload dev-clean

build:
	rm -rf dist
	mkdir dist
	cd plugin && GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o ../dist/plugin.wasm .
	cp plugin/manifest.json dist/manifest.json
	cp -R plugin/assets dist/assets
	cd dist && zip -q -r $(PLUGIN_NAME).zip manifest.json plugin.wasm assets

verify: build
	cd test && go run . ../dist/plugin.wasm
	unzip -l dist/$(PLUGIN_NAME).zip
	@unzip -p dist/$(PLUGIN_NAME).zip manifest.json | python3 -m json.tool > /dev/null
	@unzip -p dist/$(PLUGIN_NAME).zip assets/index.html | grep -q doctype
	# every file under plugin/assets must ship — app.js is load-bearing (CSP
	# blocks inline scripts, so index.html has none of its own logic)
	@for f in plugin/assets/*; do unzip -l dist/$(PLUGIN_NAME).zip | grep -q "assets/$$(basename $$f)" || { echo "missing from zip: assets/$$(basename $$f)" >&2; exit 1; }; done

clean:
	rm -rf dist

# Local dev loop: real windshift core (source build, SQLite) serving the local
# plugin on http://localhost:7777. Hot loop: edit plugin/ → `make dev-reload`.
dev:
	bash scripts/dev.sh

dev-reload:
	bash scripts/dev-reload.sh

dev-clean:
	@if [ -f .dev/server.pid ]; then kill "$$(cat .dev/server.pid)" 2>/dev/null || true; rm -f .dev/server.pid; fi
	rm -rf .dev
