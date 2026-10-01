# Builds dist/shortcut.zip — the release artifact:
#   manifest.json  plugin.wasm  assets/index.html
PLUGIN_NAME := shortcut

.PHONY: build verify clean

build:
	cd plugin && GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o ../dist/plugin.wasm .
	cp plugin/manifest.json dist/manifest.json
	cp -R plugin/assets dist/assets
	cd dist && zip -q $(PLUGIN_NAME).zip manifest.json plugin.wasm assets/index.html

verify: build
	cd test && go run . ../dist/plugin.wasm
	unzip -l dist/$(PLUGIN_NAME).zip
	@unzip -p dist/$(PLUGIN_NAME).zip manifest.json | python3 -m json.tool > /dev/null
	@unzip -p dist/$(PLUGIN_NAME).zip assets/index.html | grep -q doctype

clean:
	rm -rf dist
