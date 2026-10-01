// Package main is the Windshift "shortcuts" sample plugin.
//
// Single WASM export: handle_request. All state lives in Windshift's
// per-plugin KV store (kv_get/kv_set host functions); a fresh WASM instance
// is created per call, so nothing in-process survives between requests.
//
// Routes and the admin-tab extension are declared in manifest.json — the
// Windshift loader picks them up without any get_metadata/get_routes export
// (same pattern as the upstream react-demo plugin).
package main

import (
	"encoding/json"
	"errors"
	"strconv"
)

const (
	pluginName   = "shortcuts"
	version      = "0.1.0"
	counterKey   = "shortcuts:counter"
	bookmarksKey = "shortcuts:bookmarks"
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

type bookmark struct {
	Label string `json:"label"`
	URL   string `json:"url"`
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
	case "GET /counter":
		return bumpCounter()
	case "GET /bookmarks":
		return listBookmarks()
	case "POST /bookmarks":
		return addBookmark(req)
	case "DELETE /bookmarks":
		return deleteBookmark(req)
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
	raw := callKV(hostKVGet, mustJSON(kvGetRequest{Key: "shortcuts:probe"}))
	var resp kvGetResponse
	kvOK := len(raw) > 0 && json.Unmarshal(raw, &resp) == nil &&
		(resp.Status == "ok" || resp.Status == "not_found")
	body, _ := json.Marshal(map[string]any{"plugin": pluginName, "version": version, "kv": kvOK})
	return HTTPResponse{StatusCode: 200, Headers: jsonHeaders, Body: string(body)}
}

// bumpCounter increments a persistent visit counter in the KV store.
func bumpCounter() HTTPResponse {
	raw := callKV(hostKVGet, mustJSON(kvGetRequest{Key: counterKey}))
	var getResp kvGetResponse
	if len(raw) == 0 || json.Unmarshal(raw, &getResp) != nil {
		return serverError("counter read failed")
	}
	visits := 0
	switch getResp.Status {
	case "ok":
		n, err := strconv.Atoi(getResp.Value)
		if err != nil {
			return serverError("counter value corrupted")
		}
		visits = n
	case "not_found":
		// first visit
	default:
		return serverError("counter read failed: " + getResp.Error)
	}

	visits++
	setRaw := callKV(hostKVSet, mustJSON(kvSetRequest{Key: counterKey, Value: strconv.Itoa(visits)}))
	var setResp kvSetResponse
	if len(setRaw) == 0 || json.Unmarshal(setRaw, &setResp) != nil || setResp.Status != "ok" {
		return serverError("counter write failed")
	}
	body, _ := json.Marshal(map[string]int{"visits": visits})
	return HTTPResponse{StatusCode: 200, Headers: jsonHeaders, Body: string(body)}
}

func loadBookmarks() ([]bookmark, error) {
	raw := callKV(hostKVGet, mustJSON(kvGetRequest{Key: bookmarksKey}))
	var resp kvGetResponse
	if len(raw) == 0 || json.Unmarshal(raw, &resp) != nil {
		return nil, errors.New("bookmarks read failed")
	}
	switch resp.Status {
	case "not_found":
		return []bookmark{}, nil
	case "ok":
		var list []bookmark
		if resp.Value == "" {
			return []bookmark{}, nil
		}
		if err := json.Unmarshal([]byte(resp.Value), &list); err != nil {
			return nil, errors.New("bookmarks store corrupted")
		}
		return list, nil
	default:
		return nil, errors.New("bookmarks read failed: " + resp.Error)
	}
}

func saveBookmarks(list []bookmark) error {
	value, _ := json.Marshal(list)
	raw := callKV(hostKVSet, mustJSON(kvSetRequest{Key: bookmarksKey, Value: string(value)}))
	var resp kvSetResponse
	if len(raw) == 0 || json.Unmarshal(raw, &resp) != nil || resp.Status != "ok" {
		return errors.New("bookmarks write failed")
	}
	return nil
}

func listBookmarks() HTTPResponse {
	list, err := loadBookmarks()
	if err != nil {
		return serverError(err.Error())
	}
	body, _ := json.Marshal(list)
	return HTTPResponse{StatusCode: 200, Headers: jsonHeaders, Body: string(body)}
}

func addBookmark(req HTTPRequest) HTTPResponse {
	var in bookmark
	if json.Unmarshal([]byte(req.Body), &in) != nil || in.Label == "" || in.URL == "" {
		return HTTPResponse{StatusCode: 400, Headers: jsonHeaders,
			Body: `{"error":"body must be JSON {\"label\": string, \"url\": string}"}`}
	}
	list, err := loadBookmarks()
	if err != nil {
		return serverError(err.Error())
	}
	replaced := false
	for i := range list {
		if list[i].Label == in.Label {
			list[i] = in
			replaced = true
		}
	}
	if !replaced {
		list = append(list, in)
	}
	if err := saveBookmarks(list); err != nil {
		return serverError(err.Error())
	}
	body, _ := json.Marshal(list)
	return HTTPResponse{StatusCode: 201, Headers: jsonHeaders, Body: string(body)}
}

func deleteBookmark(req HTTPRequest) HTTPResponse {
	label := req.Query["label"]
	if label == "" {
		return HTTPResponse{StatusCode: 400, Headers: jsonHeaders,
			Body: `{"error":"label query parameter required"}`}
	}
	list, err := loadBookmarks()
	if err != nil {
		return serverError(err.Error())
	}
	kept := make([]bookmark, 0, len(list))
	found := false
	for _, b := range list {
		if b.Label == label {
			found = true
			continue
		}
		kept = append(kept, b)
	}
	if !found {
		return HTTPResponse{StatusCode: 404, Headers: jsonHeaders,
			Body: `{"error":"no bookmark with that label"}`}
	}
	if err := saveBookmarks(kept); err != nil {
		return serverError(err.Error())
	}
	body, _ := json.Marshal(kept)
	return HTTPResponse{StatusCode: 200, Headers: jsonHeaders, Body: string(body)}
}

func serverError(msg string) HTTPResponse {
	body, _ := json.Marshal(map[string]string{"error": msg})
	return HTTPResponse{StatusCode: 500, Headers: jsonHeaders, Body: string(body)}
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
