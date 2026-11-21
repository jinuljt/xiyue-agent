package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSpeechVerificationUsesDraftConfigurationAndSpeaker(t *testing.T) {
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		file, _, err := r.FormFile("file")
		if err != nil {
			t.Error(err)
			http.Error(w, "invalid fixture", 400)
			return
		}
		defer file.Close()
		wav, _ := io.ReadAll(file)
		if !validVoiceWAV(wav) || r.URL.Path != "/v1/audio/transcriptions" || r.FormValue("model") != "draft-asr" ||
			r.Header.Get("Authorization") != "Bearer draft-test-key" {
			t.Error("verification used a wrong recording, model, or credential")
			http.Error(w, "invalid fixture", 400)
			return
		}
		_, _ = io.WriteString(w, `{"text":"这是识别测试"}`)
	}))
	defer gateway.Close()
	path := filepath.Join(t.TempDir(), "state.json")
	p, err := openPlatform(path)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	var captures int
	var spoken string
	v := &voiceRuntime{speak: func(_ context.Context, text string) error { spoken = text; return nil }}
	h := (&api{platform: p, speechProbe: v, probeCapture: func(context.Context, func([]byte) error) ([]byte, error) {
		captures++
		return fixtureWAV(), nil
	}}).routes()
	call := func(service string, input ServiceInput) *httptest.ResponseRecorder {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"service": service, "config": input})
		req := httptest.NewRequest("POST", "http://127.0.0.1/api/settings/verify", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Xiyue-Request", "console")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	if w := call("asr", ServiceInput{Provider: "sensevoice"}); w.Code != 400 || captures != 0 {
		t.Fatal("an incomplete ASR configuration unexpectedly recorded audio")
	}
	if w := call("asr", ServiceInput{Provider: "sensevoice", Endpoint: gateway.URL + "/v1", Model: "draft-asr",
		APIKey: "draft-test-key"}); w.Code != 200 || !strings.Contains(w.Body.String(), "这是识别测试") || captures != 1 {
		t.Fatalf("ASR verification failed: %d %s", w.Code, w.Body.String())
	}
	if w := call("tts", ServiceInput{Provider: "system"}); w.Code != 200 || spoken != testSpokenText {
		t.Fatalf("TTS verification did not use the speaker: %d %s", w.Code, w.Body.String())
	}
	v.active = true
	if w := call("tts", ServiceInput{Provider: "system"}); w.Code != 409 {
		t.Fatal("TTS probe interrupted an active voice turn")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatal("speech verification saved settings or a recording")
	}
}
