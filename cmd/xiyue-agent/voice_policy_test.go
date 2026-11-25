package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSpeakerDialoguePolicyExecutesAndEnds(t *testing.T) {
	modelCalls := 0
	utterance := "我得去忙别的了"
	modelReply := `{"action":"end"}`
	var led atomic.Bool
	var modelOffline bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !led.Load() {
			t.Error("speaker indicator turned off during speech recognition or model processing")
		}
		if r.URL.Path == "/v1/audio/transcriptions" {
			_ = json.NewEncoder(w).Encode(map[string]string{"text": utterance})
			return
		}
		modelCalls++
		if modelOffline {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		var request struct{ Messages []voiceMessage }
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil ||
			len(request.Messages) != 2 || request.Messages[0].Content != speakerInstructions {
			t.Error("speaker instructions were missing")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]string{"content": modelReply}}},
		})
	}))
	defer server.Close()
	p, err := openPlatform(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := settingsInput(p.snapshot().Settings)
	cfg.ASR = ServiceInput{Provider: "sensevoice", Endpoint: server.URL + "/v1"}
	cfg.LLM = ServiceInput{Provider: "custom", Endpoint: server.URL + "/v1", Model: "fixture"}
	if err := p.saveSettings(cfg); err != nil {
		t.Fatal(err)
	}
	v := &voiceRuntime{}
	var spoken []string
	v.speak = func(_ context.Context, text string) error {
		if !led.Load() {
			t.Error("speaker indicator turned off before playback")
		}
		spoken = append(spoken, text)
		return nil
	}
	capture := func(context.Context, func([]byte) error) ([]byte, error) { return fixtureWAV(), nil }
	run := func() voiceResult {
		t.Helper()
		result, err := p.runVoiceTurn(context.Background(), v, capture, led.Store)
		if err != nil || led.Load() || v.active {
			t.Fatalf("turn did not return to idle: %v, %v", result, err)
		}
		return result
	}
	if got := run(); got.Stage != "conversation_ended" || len(spoken) != 0 || modelCalls != 1 {
		t.Fatalf("semantic exit failed: %+v", got)
	}

	utterance = "从现在开始往后数二十秒"
	modelReply = `{"action":"countdown","seconds":20,"speech":"模型未执行的回执"}`
	if got := run(); got.Stage != "completed" || len(p.snapshot().Schedules) != 1 || len(spoken) != 1 || spoken[0] != "已设置。" {
		t.Fatalf("confirmed local action failed: %+v %v", got, spoken)
	}
	if duration := time.Until(p.snapshot().Schedules[0].NextAt); duration < 19*time.Second || duration > 20*time.Second {
		t.Fatalf("countdown used wrong duration: %s", duration)
	}

	utterance = "如果我想学怎么设置倒计时呢"
	if got := run(); got.Stage != "completed" || len(p.snapshot().Schedules) != 1 {
		t.Fatal("hypothetical commands must not schedule")
	}
	utterance = "先不用开灯，明早下雨吗"
	modelReply = `{"action":"speak","speech":"预计有雨。早上出门带伞。可以多关注一下。"}`
	if got := run(); got.Stage != "completed" || spoken[len(spoken)-1] != "预计有雨。早上出门带伞。可以多关注一下。" {
		t.Fatal("mixed request must preserve the spoken answer", spoken)
	}
	modelOffline = true
	result, err := p.runVoiceTurn(context.Background(), v, capture, led.Store)
	if err == nil || result.Stage != "model" || led.Load() || v.active {
		t.Fatalf("model failure did not clear indicator and return to idle: %+v %v", result, err)
	}
}

func TestSpeakerReplyRejectsMalformedActions(t *testing.T) {
	for _, input := range []string{
		`{"action":"anything"}`, `{"action":"countdown","seconds":1.5}`,
		`{"action":"end","unexpected":true}`, `{"action":"end"} {"action":"speak"}`,
	} {
		if _, err := parseVoicePlan(input); err == nil {
			t.Fatalf("accepted %q", input)
		}
	}
}

func TestLongSpeechKeepsCompleteAnswerAndCanBeInterrupted(t *testing.T) {
	const first = "室温是 23.5 度。湿度是 51.2%。"
	tail := strings.Repeat("这一段没有句号但仍然要播报", 20)
	reply := first + tail
	if got := speechForLanguage(reply, "zh"); got != reply {
		t.Fatalf("speech changed: %q", got)
	}
	p, err := openPlatform(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := p.snapshot()
	s.Settings.TTS.Provider = "system"
	var parts []string
	v := &voiceRuntime{speak: func(_ context.Context, part string) error {
		parts = append(parts, part)
		return nil
	}}
	if err := p.playSpeechWithKey(context.Background(), v, s, reply, "zh", nil, true); err != nil {
		t.Fatal(err)
	}
	if len(parts) < 3 || parts[0] != first || strings.Join(parts, "") != reply {
		t.Fatalf("unexpected parts: %q", parts)
	}
	parts = nil
	v.speak = func(_ context.Context, part string) error {
		parts = append(parts, part)
		if !v.stopPlayback() {
			t.Fatal("playback could not be interrupted")
		}
		return nil
	}
	if err := p.playSpeechWithKey(context.Background(), v, s, reply, "zh", nil, true); err != context.Canceled || len(parts) != 1 {
		t.Fatalf("interrupt: parts=%q err=%v", parts, err)
	}
}
