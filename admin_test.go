package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInstallerSetsAdministratorOnceAndLoginSurvivesRestart(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "xiyue-owner-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "state.json")
	socket := filepath.Join(dir, "setup.sock")
	p, err := openPlatform(path)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := openPrivateSocket(socket)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.serveOwnerSetup(ctx, listener)

	const account = `{"username":"owner","password":"fictional-first-password"}`
	if err := sendOwnerSetup(socket, strings.NewReader(account)); err != nil {
		t.Fatal(err)
	}
	if err := sendOwnerSetup(socket, strings.NewReader(account)); err != nil {
		t.Fatal("identical install retry failed:", err)
	}
	if err := sendOwnerSetup(socket, strings.NewReader(`{"username":"other","password":"different-fictional-password"}`)); err == nil {
		t.Fatal("install step overwrote an existing administrator")
	}
	const host = "speaker.test:8443"
	admin := &adminGate{host: host}
	handler := (&api{platform: p, admin: admin}).routes()
	call := func(method, path, origin, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "https://"+host+path, strings.NewReader(body))
		r.Header.Set("Origin", origin)
		r.Header.Set("X-Xiyue-Request", "console")
		r.Header.Set("Content-Type", "application/json")
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if w := call("GET", "/api/state", "", "", nil); w.Code != 401 {
		t.Fatalf("anonymous state got %d", w.Code)
	}
	if w := call("POST", "/api/session/pair", "https://"+host, account, nil); w.Code != 401 {
		t.Fatalf("old network account setup route got %d", w.Code)
	}
	if w := call("POST", "/api/session/login", "https://other.test", account, nil); w.Code != 403 {
		t.Fatalf("foreign origin got %d", w.Code)
	}
	w := call("POST", "/api/session/login", "https://"+host, account, nil)
	cookies := w.Result().Cookies()
	if w.Code != 200 || len(cookies) != 1 || !cookies[0].Secure || !cookies[0].HttpOnly {
		t.Fatalf("valid login got %d", w.Code)
	}
	cookie := cookies[0]
	reloaded, err := openPlatform(path)
	if err != nil {
		t.Fatal(err)
	}
	handler = (&api{platform: reloaded, admin: admin}).routes()
	w = call("GET", "/api/state", "", "", cookie)
	if w.Code != 200 || strings.Contains(w.Body.String(), "password_hash") || strings.Contains(w.Body.String(), digest(cookie.Value)) {
		t.Fatal("session failed after restart or public state leaked auth materials")
	}
	if w := call("POST", "/api/session/logout", "https://"+host, "{}", cookie); w.Code != 200 {
		t.Fatalf("logout got %d", w.Code)
	}
	if w := call("GET", "/api/state", "", "", cookie); w.Code != 401 {
		t.Fatalf("old session got %d", w.Code)
	}
	admin.lastAttempt = time.Time{}
	if w := call("POST", "/api/session/login", "https://"+host, account, nil); w.Code != 200 {
		t.Fatalf("password login after restart got %d", w.Code)
	}
	if raw, err := os.ReadFile(path); err != nil || strings.Contains(string(raw), "fictional-first-password") {
		t.Fatal("plaintext credential persisted")
	}
}
