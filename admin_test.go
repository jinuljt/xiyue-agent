package main

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLANPairingGuardsConfigurationAndRevokesSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	p, err := openPlatform(path)
	if err != nil {
		t.Fatal(err)
	}
	const host = "speaker.test:8443"
	const code = "synthetic-pairing-code-for-unit-test"
	codeDir := t.TempDir()
	if err := os.Chmod(codeDir, 0700); err != nil {
		t.Fatal(err)
	}
	codePath := filepath.Join(codeDir, "owner-code")
	if err := os.WriteFile(codePath, []byte(code), 0600); err != nil {
		t.Fatal(err)
	}
	admin := &adminGate{host: host, codePath: codePath}
	const initialPassword = "first-fictional-password"
	initial := `{"code":"` + code + `","username":"owner","password":"` + initialPassword + `"}`
	handler := (&api{platform: p, admin: admin}).routes()
	call := func(method, path, origin, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "https://"+host+path, strings.NewReader(body))
		r.TLS = &tls.ConnectionState{}
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
	if w := call("POST", "/api/session/pair", "https://other.test", initial, nil); w.Code != 403 {
		t.Fatalf("foreign origin got %d", w.Code)
	}
	w := call("POST", "/api/session/pair", "https://"+host, initial, nil)
	cookies := w.Result().Cookies()
	if w.Code != 200 || len(cookies) != 1 || !cookies[0].Secure || !cookies[0].HttpOnly {
		t.Fatalf("valid pairing got %d", w.Code)
	}
	cookie := cookies[0]
	p, err = openPlatform(path)
	if err != nil {
		t.Fatal(err)
	}
	handler = (&api{platform: p, admin: admin}).routes()
	w = call("GET", "/api/state", "", "", cookie)
	if w.Code != 200 || strings.Contains(w.Body.String(), digest(code)) || strings.Contains(w.Body.String(), digest(cookie.Value)) {
		t.Fatal("paired state is unavailable after restart or includes auth materials")
	}
	if w := call("POST", "/api/session/logout", "https://"+host, "{}", cookie); w.Code != 200 {
		t.Fatalf("logout got %d", w.Code)
	}
	if w := call("GET", "/api/state", "", "", cookie); w.Code != 401 {
		t.Fatalf("old session got %d", w.Code)
	}
	admin.lastAttempt = time.Time{}
	if w := call("POST", "/api/session/pair", "https://"+host, initial, nil); w.Code != 401 {
		t.Fatalf("pairing code replay got %d", w.Code)
	}
	admin.lastAttempt = time.Time{}
	login := call("POST", "/api/session/login", "https://"+host, `{"username":"owner","password":"`+initialPassword+`"}`, nil)
	if login.Code != 200 || len(login.Result().Cookies()) != 1 {
		t.Fatalf("password login after restart got %d", login.Code)
	}
	before := login.Result().Cookies()[0]
	if err := p.change(func(s *State) error {
		s.Settings.Name = "preserved setting"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := writeAdminCode(codePath); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(codePath)
	if err != nil {
		t.Fatal(err)
	}
	fresh := strings.TrimSpace(string(raw))
	admin.lastAttempt = time.Time{}
	reset := call("POST", "/api/session/pair", "https://"+host,
		`{"username":"new-owner","password":"fictional-replacement-password","code":"`+fresh+`"}`, nil)
	if reset.Code != 200 {
		t.Fatalf("live recovery got %d", reset.Code)
	}
	if w := call("GET", "/api/state", "", "", before); w.Code != 401 {
		t.Fatal("recovery did not revoke previous session")
	}
	if p.snapshot().Settings.Name != "preserved setting" {
		t.Fatal("recovery reset device settings")
	}
	admin.lastAttempt = time.Time{}
	if w := call("POST", "/api/session/login", "https://"+host, `{"username":"owner","password":"`+initialPassword+`"}`, nil); w.Code != 401 {
		t.Fatal("old password still works after recovery")
	}
	expired := time.Now().Add(-16 * time.Minute)
	if err := os.Chtimes(codePath, expired, expired); err != nil {
		t.Fatal(err)
	}
	if _, expiry, err := readAdminCode(codePath); err != nil || expiry.After(time.Now()) {
		t.Fatal("recovery code expiration does not follow the file creation time")
	}
	if raw, err := os.ReadFile(path); err != nil || strings.Contains(string(raw), initialPassword) ||
		strings.Contains(string(raw), "fictional-replacement-password") || strings.Contains(string(raw), fresh) {
		t.Fatal("plaintext credential persisted")
	}
}
