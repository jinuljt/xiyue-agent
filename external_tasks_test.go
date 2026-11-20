package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestExternalTaskBindingAndIdempotencyHeader(t *testing.T) {
	const id = "abcdef0123456789abcdef0123456789"
	const requestID = "0123456789abcdef0123456789abcdef"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture" {
			t.Error("missing connector credential")
		}
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/tasks":
			if r.Header.Get("Idempotency-Key") != requestID {
				t.Error("missing retry-safe request ID")
			}
			writeJSON(w, 202, externalTaskStatus{ID: id, Status: "running"})
		case r.Method == "GET" && r.URL.Path == "/v1/tasks/"+id:
			writeJSON(w, 200, externalTaskStatus{ID: id, Status: "completed", Reply: "result"})
		case r.Method == "DELETE" && r.URL.Path == "/v1/tasks/"+id:
			writeJSON(w, 200, externalTaskStatus{ID: id, Status: "cancelling"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "state.json")
	p, err := openPlatform(path)
	if err != nil {
		t.Fatal(err)
	}
	in := settingsInput(p.snapshot().Settings)
	in.Mode = "external"
	in.External = ServiceInput{Provider: "http_json", Endpoint: server.URL + "/v1/chat", APIKey: "fixture"}
	if err = p.saveSettings(in); err != nil {
		t.Fatal(err)
	}
	handler := (&api{platform: p}).routes()
	call := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(body))
		r.Header.Set("X-Xiyue-Request", "console")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	for i := 0; i < 2; i++ {
		w := call("POST", "/api/external/tasks", `{"message":"request","request_id":"`+requestID+`"}`)
		if w.Code != 202 {
			t.Fatalf("external submit returned %d: %s", w.Code, w.Body)
		}
	}
	if len(p.snapshot().ExternalTasks) != 1 {
		t.Fatal("duplicate task reference")
	}
	p, err = openPlatform(path)
	if err != nil {
		t.Fatal(err)
	}
	handler = (&api{platform: p}).routes()
	item := "/api/external/tasks/" + id
	for _, method := range []string{"GET", "DELETE"} {
		if w := call(method, item, ""); w.Code != 200 {
			t.Fatalf("tracked task after restart returned %d", w.Code)
		}
	}
	in = settingsInput(p.snapshot().Settings)
	in.External.Endpoint = server.URL + "/other/v1/chat"
	in.External.ClearKey = true
	if err = p.saveSettings(in); err != nil {
		t.Fatal(err)
	}
	if w := call("GET", item, ""); w.Code != 404 {
		t.Fatalf("old task must not be forwarded to changed connector: %d", w.Code)
	}
}
