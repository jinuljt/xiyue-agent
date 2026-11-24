package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestDialogueLanguagesFollowTheSelectedVoice(t *testing.T) {
	p, err := openPlatform(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	input := settingsInput(p.snapshot().Settings)
	language, practice := "en", true
	input.ReplyLanguage, input.PracticeEnabled = &language, &practice
	if err := p.saveSettings(input); err == nil {
		t.Fatal("system TTS was allowed to advertise English")
	}
	input.ASR = ServiceInput{Provider: "doubao", APIKey: "fixture-asr-key"}
	input.TTS = ServiceInput{Provider: "doubao", Model: "unknown-voice", APIKey: "fixture-tts-key"}
	if err := p.saveSettings(input); err == nil {
		t.Fatal("unknown voice was allowed to advertise English")
	}
	input.TTS.Model = doubaoDefaultVoice
	if err := p.saveSettings(input); err != nil {
		t.Fatal(err)
	}
	settings := p.snapshot().Settings
	if settings.replyLanguage() != "en" || !settings.supportsPractice("en") || settings.supportsPractice("ja") {
		t.Fatal("unexpected supported language combinations")
	}
	if _, ok := selectSpeechLanguage(settings, voicePlan{Language: "ko"}); ok {
		t.Fatal("unsupported override accepted")
	}
	switchInput := settingsInput(settings)
	switchInput.TTS = ServiceInput{Provider: "system", ClearKey: true}
	if err := p.saveSettings(switchInput); err == nil {
		t.Fatal("incompatible TTS change silently dropped settings")
	}
}

func TestMultilingualConversationAndPractice(t *testing.T) {
	type turn struct{ text, plan, spoken string }
	turns := []turn{
		{"陪我练英语，模拟点咖啡", `{"action":"start_practice","speech":"Hi! What would you like to order?"}`, "Hi! What would you like to order?"},
		{"I want coffee please", `{"action":"speak","speech":"You can say, I'd like a coffee, please. Would you like milk?"}`, "You can say, I'd like a coffee, please. Would you like milk?"},
		{"退出陪练", `{"action":"stop_practice"}`, "已退出英语陪练。"},
		{"用日语打个招呼", `{"action":"speak","language":"ja","speech":"こんにちは。"}`, "こんにちは。"},
	}
	var index atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(index.Load())
		if r.URL.Path == "/v1/audio/transcriptions" {
			json.NewEncoder(w).Encode(map[string]string{"text": turns[i].text})
			return
		}
		var request struct{ Messages []voiceMessage }
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.Messages) == 0 {
			t.Error("bad model request")
			w.WriteHeader(400)
			return
		}
		prompt := request.Messages[0].Content
		if i == 1 && (!strings.Contains(prompt, "当前为英语口语陪练") || len(request.Messages) != 4) {
			t.Error("practice settings or history missing")
		}
		if i == 3 && (strings.Contains(prompt, "当前为英语口语陪练") || len(request.Messages) != 2) {
			t.Error("practice settings persisted after exit")
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": turns[i].plan}}}})
	}))
	defer srv.Close()
	oldClient := deepseekTTSClient
	defer func() { deepseekTTSClient = oldClient }()
	deepseekTTSClient = &http.Client{Transport: networkRoundTrip(func(r *http.Request) (*http.Response, error) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["text"] != turns[int(index.Load())].spoken || body["format"] != "pcm" || body["voice_id"] != "tide" || r.URL.String() != deepseekTTSEndpoint || r.Header.Get("Authorization") != "Bearer fixture-deepseek-tts" || body["language"] != "" {
			t.Errorf("unexpected synthesis request: %v", body)
		}
		return &http.Response{StatusCode: 200, ContentLength: 4, Body: io.NopCloser(strings.NewReader("\x01\x02\x03\x04"))}, nil
	})}
	p, err := openPlatform(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	input := settingsInput(p.snapshot().Settings)
	input.LLM = ServiceInput{Provider: "custom", Endpoint: srv.URL + "/v1", Model: "fixture"}
	input.ASR = ServiceInput{Provider: "sensevoice", Endpoint: srv.URL + "/v1", Model: "sensevoice"}
	input.TTS = ServiceInput{Provider: "deepseek", Model: "tide", APIKey: "fixture-deepseek-tts"}
	practice := true
	input.PracticeEnabled = &practice
	if err := p.saveSettings(input); err != nil {
		t.Fatal(err)
	}
	v := &voiceRuntime{playPCM: func(_ context.Context, pcm io.Reader) error {
		data, err := io.ReadAll(pcm)
		if len(data) != 4 {
			t.Error("PCM did not reach the player")
		}
		return err
	}}
	capture := func(context.Context, func([]byte) error) ([]byte, error) { return fixtureWAV(), nil }
	var dialogue dialogueSession
	for i := range turns {
		index.Store(int32(i))
		got, err := p.runSpeechTurn(context.Background(), v, capture, nil, &dialogue, false)
		if err != nil || got.Stage != "completed" || dialogue.practice != (i < 2) {
			t.Fatalf("turn %d: %+v %+v %v", i, got, dialogue, err)
		}
	}
	if deepseekCharacters("你好 A") != 6 {
		t.Fatal("DeepSeek character count mismatch")
	}
	for _, u := range p.activity.recent() {
		if u.Service == "tts" && (u.Provider != "deepseek" || u.Estimated == nil || u.Characters == nil) {
			t.Fatalf("unexpected usage record %+v", u)
		}
	}
}

func TestDoubaoSpeechAddsSelectedLanguage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Req struct{ Additions string } `json:"req_params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Req.Additions != `{"explicit_language":"en"}` {
			t.Errorf("bad language: %q", body.Req.Additions)
		}
		io.WriteString(w, `{"code":0,"data":"AQI="}`)
	}))
	defer server.Close()
	err := doubaoSpeech(context.Background(), server.URL, doubaoDefaultVoice, "fixture-key", "Hello", func(_ context.Context, r io.Reader) error {
		_, err := io.Copy(io.Discard, r)
		return err
	}, "en")
	if err != nil {
		t.Fatal(err)
	}
}
