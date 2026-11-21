package main

import (
	"bytes"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDeviceLoginBindsApprovalAndChangesPassword(t *testing.T) {
	p, err := openPlatform(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	salt := make([]byte, 16)
	hash, err := passwordHash("fixture-original-password", salt)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.change(func(s *State) error {
		s.Owner = &ownerState{Username: "owner", PasswordSalt: salt, PasswordHash: hash}
		s.Settings.ASR = ServiceConfig{Provider: "sensevoice", Endpoint: "http://localhost/v1"}
		s.Settings.Name = "fixture unchanged"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	const host = "speaker.test:8443"
	var led []bool
	g := &adminGate{host: host, voiceLogin: true, buttonReady: true, buttonLabel: "测试实体键",
		buttonLED: func(on bool) { led = append(led, on) }}
	handler := (&api{platform: p, admin: g}).routes()
	call := func(method, path, body string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "https://"+host+"/api/session/"+path, strings.NewReader(body))
		r.Header.Set("Origin", "https://"+host)
		r.Header.Set("X-Xiyue-Request", "console")
		r.Header.Set("Content-Type", "application/json")
		for _, c := range cookies {
			r.AddCookie(c)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	start := call("POST", "device", `{"method":"voice"}`)
	if start.Code != 202 {
		t.Fatalf("voice challenge returned %d: %s", start.Code, start.Body.String())
	}
	browser := start.Result().Cookies()[0]
	if !browser.Secure || !browser.HttpOnly {
		t.Fatal("challenge cookie is not protected")
	}
	if w := call("POST", "device", `{"method":"button"}`); w.Code != 409 {
		t.Fatal("parallel challenge replaced the original")
	}
	g.confirmVoiceLogin("确认登录，0000")
	if w := call("POST", "device/claim", "{}", browser); w.Code != 202 {
		t.Fatal("wrong PIN approved the challenge")
	}
	var spoken strings.Builder
	for _, r := range g.pending.code {
		spoken.WriteRune([]rune("零一二三四五六七八九")[int(r-'0')])
	}
	if !strings.Contains(start.Body.String(), g.pending.code) ||
		!g.confirmVoiceLogin("确认登录，"+spoken.String()+"。") {
		t.Fatal("spoken digits did not reach the login handler")
	}
	if w := call("POST", "device/claim", "{}"); w.Code != 404 {
		t.Fatal("another browser obtained the approval")
	}
	claimed := call("POST", "device/claim", "{}", browser)
	if claimed.Code != 200 {
		t.Fatal("bound browser could not claim its approval")
	}
	login := claimed.Result().Cookies()[0]
	if w := call("POST", "device/claim", "{}", browser); w.Code != 404 {
		t.Fatal("approval was claimable more than once")
	}
	if w := call("POST", "password", `{"password":"fixture-new-password"}`); w.Code != 401 {
		t.Fatal("anonymous password change was allowed")
	}
	changed := call("POST", "password", `{"password":"fixture-new-password"}`, login)
	if changed.Code != 200 {
		t.Fatalf("password update returned %d: %s", changed.Code, changed.Body.String())
	}
	rotated := changed.Result().Cookies()[0]
	stored := p.snapshot()
	newHash, err := passwordHash("fixture-new-password", stored.Owner.PasswordSalt)
	if err != nil || !bytes.Equal(newHash, stored.Owner.PasswordHash) || stored.Owner.SessionDigest != digest(rotated.Value) ||
		stored.Owner.SessionDigest == digest(login.Value) || stored.Settings.Name != "fixture unchanged" {
		t.Fatal("password update failed to rotate the session or mutated settings")
	}
	g.lastDeviceStart = time.Time{}
	start = call("POST", "device", `{"method":"button"}`)
	if start.Code != 202 || !strings.Contains(start.Body.String(), "测试实体键") {
		t.Fatal("button challenge is not available with its configured label")
	}
	browser = start.Result().Cookies()[0]
	g.confirmButtonLogin(time.Now().Add(-time.Minute))
	if w := call("POST", "device/claim", "{}", browser); w.Code != 202 {
		t.Fatal("press preceding the challenge was accepted")
	}
	g.confirmButtonLogin(time.Now())
	if w := call("POST", "device/claim", "{}", browser); w.Code != 200 || len(led) != 2 || !led[0] || led[1] {
		t.Fatal("button approval or light cleanup failed")
	}
	g.lastDeviceStart = time.Time{}
	start = call("POST", "device", `{"method":"voice"}`)
	if start.Code != 202 {
		t.Fatal("could not start new attempt")
	}
	browser = start.Result().Cookies()[0]
	g.mu.Lock()
	g.pending.expires = time.Now().Add(-time.Second)
	g.mu.Unlock()
	if w := call("POST", "device/claim", "{}", browser); w.Code != 404 {
		t.Fatal("expired request was not removed")
	}

}

func TestAdminButtonRequiresTwoShortPressesOfConfiguredKey(t *testing.T) {
	header := strconv.IntSize / 4
	var input bytes.Buffer
	event := func(code uint16, value uint32) {
		data := make([]byte, header+8)
		now := time.Now()
		if header == 16 {
			binary.NativeEndian.PutUint64(data, uint64(now.Unix()))
			binary.NativeEndian.PutUint64(data[8:], uint64(now.Nanosecond()/1000))
		} else {
			binary.NativeEndian.PutUint32(data, uint32(now.Unix()))
			binary.NativeEndian.PutUint32(data[4:], uint32(now.Nanosecond()/1000))
		}
		binary.NativeEndian.PutUint16(data[header:], 1)
		binary.NativeEndian.PutUint16(data[header+2:], code)
		binary.NativeEndian.PutUint32(data[header+4:], value)
		input.Write(data)
	}
	event(39, 1)
	event(39, 0)
	event(38, 1)
	event(38, 2)
	event(38, 0)
	event(38, 1)
	event(38, 0)
	var confirmed int
	readAdminButton(&input, 38, func(time.Time) { confirmed++ })
	if confirmed != 1 {
		t.Fatalf("expected one double press, got %d", confirmed)
	}
}
