package main

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
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
	admin := &adminGate{host: host, pairingDigest: digest(code), pairingExpires: time.Now().Add(time.Minute)}
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
	if w := call("POST", "/api/session/pair", "https://other.test", `{"code":"`+code+`"}`, nil); w.Code != 403 {
		t.Fatalf("foreign origin got %d", w.Code)
	}
	w := call("POST", "/api/session/pair", "https://"+host, `{"code":"`+code+`"}`, nil)
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
	if w.Code != 200 || strings.Contains(w.Body.String(), admin.pairingDigest) || strings.Contains(w.Body.String(), digest(cookie.Value)) {
		t.Fatal("paired state is unavailable after restart or includes auth materials")
	}
	if w := call("POST", "/api/session/logout", "https://"+host, "{}", cookie); w.Code != 200 {
		t.Fatalf("logout got %d", w.Code)
	}
	if w := call("GET", "/api/state", "", "", cookie); w.Code != 401 {
		t.Fatalf("old session got %d", w.Code)
	}
	admin.lastAttempt = time.Time{}
	if w := call("POST", "/api/session/pair", "https://"+host, `{"code":"`+code+`"}`, nil); w.Code != 400 {
		t.Fatalf("pairing code replay got %d", w.Code)
	}
}
