// Host-side smoke test: loads dist/plugin.wasm through the same runtime
// stack windshift uses (github.com/extism/go-sdk on wazero) and exercises
// every route with an in-memory mock of the kv_get/kv_set host functions,
// mirroring windshift's extism:host/user pointer ABI (JSON payload at
// stack[0] offset in, JSON response ptr out).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	extism "github.com/extism/go-sdk"
)

var kv = map[string]string{}

type kvGetReq struct {
	Key string `json:"key"`
}
type kvGetResp struct {
	Status string `json:"status"`
	Value  string `json:"value,omitempty"`
}
type kvSetReq struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}
type kvSetResp struct {
	Status string `json:"status"`
}

func kvGet(_ context.Context, p *extism.CurrentPlugin, stack []uint64) {
	raw, err := p.ReadBytes(stack[0])
	if err != nil {
		stack[0] = 0
		return
	}
	var req kvGetReq
	if json.Unmarshal(raw, &req) != nil {
		stack[0] = 0
		return
	}
	val, ok := kv[req.Key]
	status := "ok"
	if !ok {
		status = "not_found"
	}
	resp, _ := json.Marshal(kvGetResp{Status: status, Value: val})
	ptr, err := p.WriteBytes(resp)
	if err != nil {
		stack[0] = 0
		return
	}
	stack[0] = ptr
}

func kvSet(_ context.Context, p *extism.CurrentPlugin, stack []uint64) {
	raw, err := p.ReadBytes(stack[0])
	if err != nil {
		stack[0] = 0
		return
	}
	var req kvSetReq
	if json.Unmarshal(raw, &req) != nil {
		stack[0] = 0
		return
	}
	kv[req.Key] = req.Value
	resp, _ := json.Marshal(kvSetResp{Status: "ok"})
	ptr, err := p.WriteBytes(resp)
	if err != nil {
		stack[0] = 0
		return
	}
	stack[0] = ptr
}

type wsRequest struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
	Query   map[string]string `json:"query"`
}
type wsResponse struct {
	StatusCode int               `json:"statusCode"`
	Headers    map[string]string `json:"headers"`
	Body       string            `json:"body"`
}

var fails int

func check(name string, got wsResponse, wantCode int, wantBody string) {
	ok := got.StatusCode == wantCode
	if wantBody != "" && got.Body != wantBody {
		ok = false
	}
	if ok {
		fmt.Printf("PASS %-38s %d %s\n", name, got.StatusCode, wantBody)
		return
	}
	fails++
	fmt.Printf("FAIL %-38s code=%d body=%q (want %d %q)\n", name, got.StatusCode, got.Body, wantCode, wantBody)
}

func call(ctx context.Context, plugin *extism.Plugin, method, path string, query map[string]string, body string) wsResponse {
	in, _ := json.Marshal(wsRequest{
		Method:  method,
		Path:    path,
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    body,
		Query:   query,
	})
	exit, out, err := plugin.Call("handle_request", in)
	if err != nil {
		fails++
		fmt.Printf("FAIL call %s %s: %v\n", method, path, err)
		return wsResponse{}
	}
	if exit != 0 {
		fails++
		fmt.Printf("FAIL call %s %s: plugin exit code %d\n", method, path, exit)
		return wsResponse{}
	}
	var resp wsResponse
	if json.Unmarshal(out, &resp) != nil {
		fails++
		fmt.Printf("FAIL unmarshal response for %s %s: %v\n", method, path, err)
		return wsResponse{}
	}
	return resp
}

func main() {
	wasmPath := os.Args[1]
	ctx := context.Background()
	manifest := extism.Manifest{
		Wasm: []extism.Wasm{extism.WasmFile{Path: wasmPath}},
	}
	plugin, err := extism.NewPlugin(ctx, manifest, extism.PluginConfig{EnableWasi: true},
		[]extism.HostFunction{
			extism.NewHostFunctionWithStack("kv_get", kvGet, []extism.ValueType{extism.ValueTypeI64}, []extism.ValueType{extism.ValueTypeI64}),
			extism.NewHostFunctionWithStack("kv_set", kvSet, []extism.ValueType{extism.ValueTypeI64}, []extism.ValueType{extism.ValueTypeI64}),
		})
	if err != nil {
		fmt.Printf("FAIL instantiate: %v\n", err)
		os.Exit(1)
	}
	defer plugin.Close(ctx)

	call(ctx, plugin, "GET", "/status", nil, "")
	check("GET /status", call(ctx, plugin, "GET", "/status", nil, ""), 200, `{"kv":true,"plugin":"shortcuts","version":"0.1.0"}`)
	check("GET /counter (1st)", call(ctx, plugin, "GET", "/counter", nil, ""), 200, `{"visits":1}`)
	check("GET /counter (2nd, persisted)", call(ctx, plugin, "GET", "/counter", nil, ""), 200, `{"visits":2}`)
	check("GET /bookmarks (empty)", call(ctx, plugin, "GET", "/bookmarks", nil, ""), 200, `[]`)
	check("POST /bookmarks Linear", call(ctx, plugin, "POST", "/bookmarks", nil, `{"label":"Linear","url":"https://linear.app"}`), 201, "")
	check("POST /bookmarks gh", call(ctx, plugin, "POST", "/bookmarks", nil, `{"label":"gh","url":"https://github.com"}`), 201, "")
	list := call(ctx, plugin, "GET", "/bookmarks", nil, "")
	if len(list.Body) < 50 || list.StatusCode != 200 {
		fails++
		fmt.Printf("FAIL list bookmarks: code=%d body=%q\n", list.StatusCode, list.Body)
	} else {
		fmt.Printf("PASS %-38s %d %s\n", "GET /bookmarks (2 items)", list.StatusCode, list.Body)
	}
	check("DELETE /bookmarks?label=Linear", call(ctx, plugin, "DELETE", "/bookmarks", map[string]string{"label": "Linear"}, ""), 200, "")
	check("GET /bookmarks (1 item)", call(ctx, plugin, "GET", "/bookmarks", nil, ""), 200, "")
	check("GET /unmatched route", call(ctx, plugin, "GET", "/unmatched", nil, ""), 404, "")

	if fails > 0 {
		fmt.Printf("\n%d FAILURES\n", fails)
		os.Exit(1)
	}
	fmt.Println("\nAll route tests passed — wasm verified against the windshift runtime stack.")
}
