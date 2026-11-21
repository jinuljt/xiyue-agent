package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestSpeakerDialoguePolicyExecutesAndEnds(t *testing.T) {
	modelCalls := 0
	utterance := "我得去忙别的了"
	modelReply := `{"action":"end"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/audio/transcriptions" {
			_ = json.NewEncoder(w).Encode(map[string]string{"text": utterance})
			return
		}
		modelCalls++
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
	v.speak = func(_ context.Context, text string) error { spoken = append(spoken, text); return nil }
	capture := func(context.Context, func([]byte) error) ([]byte, error) { return fixtureWAV(), nil }
	var led bool
	run := func() voiceResult {
		t.Helper()
		result, err := p.runVoiceTurn(context.Background(), v, capture, func(on bool) { led = on })
		if err != nil || led || v.active {
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
	if got := run(); got.Stage != "completed" || spoken[len(spoken)-1] != "预计有雨。早上出门带伞。" {
		t.Fatal("mixed request must continue and stay concise", spoken)
	}
}

func TestSpeakerReplyRejectsMalformedActionsAndPreservesUnits(t *testing.T) {
	for _, input := range []string{
		`{"action":"anything"}`, `{"action":"countdown","seconds":1.5}`,
		`{"action":"end","unexpected":true}`, `{"action":"end"} {"action":"speak"}`,
	} {
		if _, err := parseVoicePlan(input); err == nil {
			t.Fatalf("accepted %q", input)
		}
	}
	const reply = "室温是 23.5 度。湿度是 51.2%。建议保持通风。"
	if got := speechForSpeaker(reply, "现在室内环境如何"); got != "室温是 23.5 度。湿度是 51.2%。" {
		t.Fatal(got)
	}
	if got := speechForSpeaker(reply, "详细说说"); got != reply {
		t.Fatal(got)
	}
}
