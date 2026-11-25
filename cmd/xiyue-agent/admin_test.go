package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
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

	const account = `{"username":"owner","password":"x"}`
	if err := sendOwnerSetup(socket, strings.NewReader(`{"username":"owner","password":""}`)); err == nil {
		t.Fatal("empty password was accepted")
	}
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
	for _, view := range []string{"home", "system&catalog=1", "device-status"} {
		w := call("GET", "/api/console?view="+view, "", "", nil)
		body := w.Body.String()
		if w.Code != 200 || !strings.Contains(body, `"authenticated":false`) ||
			strings.Contains(body, `"state":`) || strings.Contains(body, `"status":`) || strings.Contains(body, `"providers":`) || strings.Contains(body, `"usage":`) {
			t.Fatalf("unauthenticated console exposed data for %q: %s", view, body)
		}
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
	for _, item := range []struct{ query, include, exclude string }{
		{"home", "usage", "providers"},
		{"system&catalog=1", "providers", "usage"},
		{"system", "state", "providers"},
		{"device-status", "status", "usage"},
	} {
		w := call("GET", "/api/console?view="+item.query, "", "", cookie)
		var frame map[string]json.RawMessage
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &frame) != nil || frame[item.include] == nil || frame[item.exclude] != nil ||
			strings.Contains(w.Body.String(), `"password_hash":`) || strings.Contains(w.Body.String(), digest(cookie.Value)) {
			t.Fatalf("unexpected authenticated console response for %q", item.query)
		}
		if item.query == "device-status" && !strings.Contains(string(frame["status"]), `"memory_bytes":`) {
			t.Fatal("missing device diagnostics")
		}
	}
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
	if raw, err := os.ReadFile(path); err != nil || strings.Contains(string(raw), `"password": "x"`) {
		t.Fatal("plaintext credential persisted")
	}
}

func TestConsoleSetupAllowedOnlyOnceOnLocalNetwork(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	p, err := openPlatform(path)
	if err != nil {
		t.Fatal(err)
	}
	const host = "speaker.test:8443"
	handler := (&api{platform: p, admin: &adminGate{host: host}}).routes()
	account := `{"username":"speaker-owner","password":"1"}`
	submit := func(peer, origin string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "https://"+host+"/api/session/setup", strings.NewReader(account))
		r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey,
			&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 8443}))
		r.RemoteAddr = peer
		r.Header.Set("Origin", origin)
		r.Header.Set("X-Xiyue-Request", "console")
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Forwarded-For", "127.0.0.1")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if w := submit("203.0.113.1:4920", "https://"+host); w.Code != 403 {
		t.Fatal("external peer or forwarded address was accepted")
	}
	if w := submit("127.0.0.1:4920", "https://another.test"); w.Code != 403 {
		t.Fatal("foreign origin could configure administrator")
	}
	w := submit("127.0.0.1:4920", "https://"+host)
	if w.Code != 200 || len(w.Result().Cookies()) != 1 || !w.Result().Cookies()[0].Secure {
		t.Fatalf("local setup didn't log the browser in: %d", w.Code)
	}
	if w := submit("127.0.0.1:4921", "https://"+host); w.Code != 409 {
		t.Fatal("setup remained open after the account was set")
	}
}

func TestLegacyPasswordMigratesOnlyAfterSuccessfulLogin(t *testing.T) {
	p, err := openPlatform(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	salt := make([]byte, 16)
	legacy, err := passwordHash("fixture-owner-password", salt)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.change(func(s *State) error {
		s.Owner = &ownerState{Username: "owner", PasswordSalt: salt, PasswordHash: legacy}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	const host = "speaker.test:8443"
	g := &adminGate{host: host}
	h := (&api{platform: p, admin: g}).routes()
	attempt := func(password string) int {
		req := httptest.NewRequest("POST", "https://"+host+"/api/session/login",
			strings.NewReader(`{"username":"owner","password":"`+password+`"}`))
		req.Header.Set("Origin", "https://"+host)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Xiyue-Request", "console")
		recorder := httptest.NewRecorder()
		h.ServeHTTP(recorder, req)
		g.lastAttempt = time.Time{}
		return recorder.Code
	}
	if status := attempt("incorrect"); status != 401 || len(p.snapshot().Owner.LoginHash) != 0 {
		t.Fatal("invalid credentials triggered migration")
	}
	if status := attempt("fixture-owner-password"); status != 200 {
		t.Fatalf("legacy login failed with %d", status)
	}
	owner := p.snapshot().Owner
	if len(owner.LoginHash) != 32 || !bytes.Equal(owner.PasswordHash, legacy) ||
		!owner.checkPassword("fixture-owner-password") || owner.checkPassword("incorrect") {
		t.Fatal("stored credentials do not support fast login or software rollback")
	}
	if status := attempt("fixture-owner-password"); status != 200 {
		t.Fatalf("fast login failed with %d", status)
	}
}
