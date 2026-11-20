package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func fixtureWAV() []byte {
	wav := []byte("RIFF\x00\x00\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00\x80\x3e\x00\x00\x00\x7d\x00\x00\x02\x00\x10\x00data\x04\x00\x00\x00\x01\x00\x02\x00")
	binary.LittleEndian.PutUint32(wav[4:], uint32(len(wav)-8))
	return wav
}

func TestVoiceTurnUsesConfiguredASRAndModelBeforePlayback(t *testing.T) {
	wav := fixtureWAV()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/audio/transcriptions":
			file, info, err := r.FormFile("file")
			if err != nil {
				t.Errorf("missing audio: %v", err)
				w.WriteHeader(400)
				return
			}
			defer file.Close()
			data, err := io.ReadAll(file)
			if err != nil || !bytes.Equal(wav, data) || info.Filename != "utterance.wav" ||
				r.FormValue("model") != "sensevoice" || r.Header.Get("Authorization") != "Bearer synthetic" {
				t.Error("wrong transcription inputs")
			}
			_, _ = io.WriteString(w, `{"text":"你好，喜悦"}`)
		case "/v1/chat/completions":
			data, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(data), "你好，喜悦") {
				t.Error("model did not receive recognized text")
			}
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"你好呀"}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	p, err := openPlatform(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	input := settingsInput(p.snapshot().Settings)
	input.ASR = ServiceInput{Provider: "sensevoice", Endpoint: server.URL + "/v1", APIKey: "synthetic"}
	input.LLM = ServiceInput{Provider: "custom", Endpoint: server.URL + "/v1", Model: "fixture-model"}
	if err := p.saveSettings(input); err != nil {
		t.Fatal(err)
	}
	var spoken string
	v := &voiceRuntime{speak: func(_ context.Context, text string) error { spoken = text; return nil }}
	h := (&api{platform: p, voice: v}).routes()
	run := func(body []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/voice/turn", bytes.NewReader(body))
		req.Header.Set("Content-Type", "audio/wav")
		req.Header.Set("X-Xiyue-Request", "console")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	if got := run(wav[:len(wav)-1]); got.Code != 400 {
		t.Fatalf("invalid wav returned %d", got.Code)
	}
	got := run(wav)
	if got.Code != 200 || spoken != "你好呀" {
		t.Fatalf("unexpected turn: %d %s / %q", got.Code, got.Body, spoken)
	}
	status, err := json.Marshal(v.status())
	if err != nil || !bytes.Contains(status, []byte(`"stage":"completed"`)) || bytes.Contains(status, []byte(spoken)) {
		t.Fatalf("unexpected voice status: %s", status)
	}
	if p.snapshot().Settings.SetupComplete {
		t.Fatal("a WAV probe cannot mark the actual wake-word setup complete")
	}
}
