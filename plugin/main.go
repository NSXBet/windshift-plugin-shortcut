// Package main is the Windshift Shortcut migration plugin.
//
// Single WASM export: handle_request. Persistent state (migration cursors,
// id mappings) will live in Windshift's per-plugin KV store via the
// kv_get/kv_set host functions; a fresh WASM instance is created per call, so
// nothing in-process survives between requests.
//
// This is the deployment skeleton: it validates that the plugin loads,
// routes mount, the KV host-function ABI works, and the admin tab renders.
// The Shortcut→Windshift migration handlers are intentionally not implemented
// yet.
//
// Routes and the admin-tab extension are declared in manifest.json — the
// Windshift loader picks them up without any get_metadata/get_routes export
// (same pattern as the upstream react-demo plugin).
package main

import (
	"encoding/json"
)

const (
	pluginName = "shortcut"
	version    = "0.1.1"
	probeKey   = "shortcut:probe"
)

var jsonHeaders = map[string]string{"Content-Type": "application/json"}

// --- wire types (mirror windshift internal/plugins types.go) ---

type HTTPRequest struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
	Query   map[string]string `json:"query"`
	Params  map[string]string `json:"params"`
}

type HTTPResponse struct {
	StatusCode int               `json:"statusCode"`
	Headers    map[string]string `json:"headers"`
	Body       string            `json:"body"`
}

type kvGetRequest struct {
	Key string `json:"key"`
}

type kvGetResponse struct {
	Status string `json:"status"` // "ok" | "not_found"
	Value  string `json:"value,omitempty"`
	Error  string `json:"error,omitempty"`
}

type kvSetRequest struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type kvSetResponse struct {
	Status string `json:"status"` // "ok" | "error"
	Error  string `json:"error,omitempty"`
}

// --- WASM export ---

//go:wasmexport handle_request
func handle_request() {
	var req HTTPRequest
	if err := json.Unmarshal(inputBytes(), &req); err != nil {
		write(HTTPResponse{StatusCode: 400, Headers: jsonHeaders, Body: `{"error":"invalid request payload"}`})
		return
	}
	logInfo("handle_request " + req.Method + " " + req.Path)
	write(route(req))
}

// --- routing ---

func route(req HTTPRequest) HTTPResponse {
	switch req.Method + " " + req.Path {
	case "GET /status":
		return status()
	default:
		body, _ := json.Marshal(map[string]string{"error": "route not found: " + req.Method + " " + req.Path})
		return HTTPResponse{StatusCode: 404, Headers: jsonHeaders, Body: string(body)}
	}
}

func write(resp HTTPResponse) {
	b, err := json.Marshal(resp)
	if err != nil {
		outputBytes([]byte(`{"statusCode":500,"body":"response marshal failed"}`))
		return
	}
	outputBytes(b)
}

// --- handlers ---

// status exercises the KV host function ABI and reports the result.
func status() HTTPResponse {
	raw := callKV(hostKVGet, mustJSON(kvGetRequest{Key: probeKey}))
	var resp kvGetResponse
	kvOK := len(raw) > 0 && json.Unmarshal(raw, &resp) == nil &&
		(resp.Status == "ok" || resp.Status == "not_found")
	body, _ := json.Marshal(map[string]any{"plugin": pluginName, "version": version, "kv": kvOK})
	return HTTPResponse{StatusCode: 200, Headers: jsonHeaders, Body: string(body)}
}

// --- util ---

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // all payloads are plain data structs; marshal cannot fail
	}
	return b
}

func main() {}
