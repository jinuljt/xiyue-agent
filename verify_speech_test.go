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
	call := func(service string, input ServiceInput, languages ...string) *httptest.ResponseRecorder {
		t.Helper()
		payload := map[string]any{"service": service, "config": input}
		if len(languages) > 0 {
			payload["language"] = languages[0]
		}
		body, _ := json.Marshal(payload)
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
	if w := call("tts", ServiceInput{Provider: "system"}, "en"); w.Code != 200 || spoken != "Could I have a cup of coffee, please?" {
		t.Fatal("system preview did not pass through the English sample")
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

func TestDialogueProbeKeepsTheTranscriptWhenPlaybackFails(t *testing.T) {
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/audio/transcriptions":
			if r.FormValue("language") != "en" || r.Header.Get("Authorization") != "Bearer draft-asr" {
				t.Error("probe did not use the draft recognition language and credential")
			}
			_, _ = io.WriteString(w, `{"text":"How are you?"}`)
		case "/v1/chat/completions":
			var input struct{ Messages []voiceMessage }
			err := json.NewDecoder(r.Body).Decode(&input)
			if err != nil || len(input.Messages) != 2 || !strings.Contains(input.Messages[0].Content, "测试语种为 en") || input.Messages[1].Content != "How are you?" {
				t.Error("model lost the configured language or recognition result")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{
				"content": `{"action":"speak","language":"en","speech":"I'm doing well."}`,
			}}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer model.Close()
	p, err := openPlatform(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	before := p.snapshot().Settings
	before.LLM = ServiceConfig{Provider: "custom", Endpoint: model.URL + "/v1", Model: "probe-fixture"}
	if err := p.change(func(s *State) error { s.Settings = before; return nil }); err != nil {
		t.Fatal(err)
	}
	var attempts int
	oldClient := deepseekTTSClient
	defer func() { deepseekTTSClient = oldClient }()
	deepseekTTSClient = &http.Client{Transport: networkRoundTrip(func(r *http.Request) (*http.Response, error) {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["text"] != "I'm doing well." ||
			body["voice_id"] != "tide" || r.Header.Get("Authorization") != "Bearer draft-tts" {
			t.Error("probe synthesized the wrong transcript, voice, or draft credential")
		}
		status := 200
		if attempts == 1 {
			status = 503
		}
		attempts++
		return &http.Response{StatusCode: status, ContentLength: 4, Body: io.NopCloser(strings.NewReader("\x01\x02\x03\x04"))}, nil
	})}
	player := &voiceRuntime{playPCM: func(_ context.Context, pcm io.Reader) error {
		_, err := io.Copy(io.Discard, pcm)
		return err
	}}
	handler := (&api{platform: p, speechProbe: player, probeCapture: func(context.Context, func([]byte) error) ([]byte, error) {
		return fixtureWAV(), nil
	}}).routes()
	payload, _ := json.Marshal(map[string]any{
		"service": "dialogue", "language": "en",
		"asr": ServiceInput{Provider: "sensevoice", Endpoint: model.URL + "/v1", APIKey: "draft-asr"},
		"tts": ServiceInput{Provider: "deepseek", Model: "tide", APIKey: "draft-tts"},
	})
	for _, expected := range []string{"completed", "playback"} {
		r := httptest.NewRequest("POST", "http://localhost/api/settings/verify", strings.NewReader(string(payload)))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Xiyue-Request", "console")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		var result dialogueProbeResult
		err := json.Unmarshal(w.Body.Bytes(), &result)
		if w.Code != 200 || err != nil || result.Stage != expected || result.Transcript != "How are you?" ||
			result.Reply != "I'm doing well." || result.Voice != "暗潮" || (result.Error != "") != (expected == "playback") {
			t.Fatalf("probe did not preserve the trace: %d %s", w.Code, w.Body.String())
		}
	}
	if p.snapshot().Settings != before || len(p.snapshot().Schedules) != 0 {
		t.Fatal("preview changed saved settings or ran a task")
	}
}
