package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSettingsScopesKeepOtherSavedValues(t *testing.T) {
	p, err := openPlatform(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	full := settingsInput(p.snapshot().Settings)
	full.LLM.APIKey = "fixture-model-key"
	full.ASR = ServiceInput{Provider: "sensevoice", Endpoint: "http://localhost/v1"}
	full.TTS = ServiceInput{Provider: "deepseek", Model: "tide", APIKey: "fixture-tts-key"}
	en := "en"
	full.ReplyLanguage = &en
	if err := p.saveSettings(full); err != nil {
		t.Fatal(err)
	}
	before := p.snapshot()
	if err := p.saveSettings(SettingsInput{Scope: "device", Revision: before.Settings.Revision, WakeWord: "喜悦", Timezone: "Asia/Tokyo"}); err != nil {
		t.Fatal(err)
	}
	after := p.snapshot()
	if after.Settings.Timezone != "Asia/Tokyo" || after.Settings.ReplyLanguage != "en" || after.Settings.ASR != before.Settings.ASR ||
		after.Settings.TTS != before.Settings.TTS || after.Settings.LLM != before.Settings.LLM || after.Sealed["service:llm"] != before.Sealed["service:llm"] {
		t.Fatal("device save affected another tab")
	}
	if err := p.saveSettings(SettingsInput{Scope: "voice", Revision: before.Settings.Revision}); err == nil {
		t.Fatal("stale form was accepted")
	}
	if err := p.saveSettings(SettingsInput{Scope: "model", Revision: after.Settings.Revision, Mode: "external",
		LLM: full.LLM, External: full.External}); err != nil {
		t.Fatal(err)
	}
	if got := p.snapshot().Settings; got.ReplyLanguage != "en" || got.replyLanguage() != "zh" || len(got.practiceChoices()) != 0 {
		t.Fatal("external-only mode should preserve the built-in voice preference")
	}
}

func TestPracticeCapabilityUsesASRAndTTSIntersection(t *testing.T) {
	p, err := openPlatform(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := (&api{platform: p}).routes()
	path := "http://localhost/api/abilities/language-practice"
	call := func(method, body string, status int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Xiyue-Request", "console")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("%d: %s", w.Code, w.Body.String())
		}
		return w
	}
	if w := call(http.MethodGet, "", 200); !strings.Contains(w.Body.String(), `"available":[]`) {
		t.Fatal(w.Body.String())
	}
	if err := p.change(func(s *State) error {
		s.Settings.ASR = ServiceConfig{Provider: "sensevoice", Endpoint: "http://localhost/v1"}
		s.Settings.TTS = ServiceConfig{Provider: "doubao", Model: doubaoDefaultVoice, KeyConfigured: true}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var options struct{ Available []dialogueLanguage }
	json.Unmarshal(call(http.MethodGet, "", 200).Body.Bytes(), &options)
	var allowed []string
	for _, option := range options.Available {
		allowed = append(allowed, option.ID)
	}
	if !slices.Equal(allowed, []string{"en", "ja"}) {
		t.Fatalf("unexpected intersection %v", allowed)
	}
	call(http.MethodPut, `{"enabled":true,"language":"ko"}`, 400)
	call(http.MethodPut, `{"enabled":true,"language":"ja"}`, 200)
	s := p.snapshot().Settings
	if !s.PracticeEnabled || s.PracticeLanguage != "ja" || !strings.Contains(dialoguePrompt(s, s.practiceLanguage()), "语种代码为 ja") {
		t.Fatal("Japanese practice was not saved")
	}
	call(http.MethodPut, `{"enabled":false,"language":""}`, 200)
	if p.snapshot().Settings.PracticeEnabled {
		t.Fatal("practice remains active")
	}
}

func TestSetupStageUsesRuntimeReadiness(t *testing.T) {
	var a api
	s := State{Settings: defaultSettings()}
	if stage := a.setupStage(s); stage != "configure" {
		t.Fatal(stage)
	}
	s.Settings.Mode = "external"
	if stage := a.setupStage(s); stage != "configure_external" {
		t.Fatal(stage)
	}
	s.Settings.External.Endpoint = "https://connector.example/v1/chat"
	if stage := a.setupStage(s); stage != "verify" {
		t.Fatal(stage)
	}
	a.voice = &voiceRuntime{}
	if stage := a.setupStage(s); stage != "configure_asr" {
		t.Fatal(stage)
	}
	s.Settings.ASR.KeyConfigured = true
	if stage := a.setupStage(s); stage != "configure_tts" {
		t.Fatal(stage)
	}
}
