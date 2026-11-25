package main

import (
	"path/filepath"
	"testing"
	"time"
)

func TestServiceStatusReportsActualUsageIndependentlyOfChecks(t *testing.T) {
	p, err := openPlatform(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	config := defaultSettings()
	config.Revision = 4
	config.ASR = ServiceConfig{Provider: "doubao", KeyConfigured: true}
	config.TTS = ServiceConfig{Provider: "system"}
	state := State{Settings: config, Usage: []UsageRecord{
		{Service: "asr", Provider: "doubao", At: now.Add(-time.Minute), Success: true, ConfigRevision: 3},
		{Service: "asr", Provider: "doubao", At: now, Test: true, Success: false, ConfigRevision: 4},
		{Service: "llm", Provider: config.LLM.Provider, At: now, Success: true, ConfigRevision: 4},
	}}
	p.activity.observeLocal("tts", "system", 4, nil, now)
	if err := p.activity.flush(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openPlatform(p.path)
	if err != nil {
		t.Fatal(err)
	}
	status := (&api{platform: reopened, voice: &voiceRuntime{}}).serviceStatus(state)
	asr := status["asr"].LastUse
	if asr == nil || !asr.Success || asr.CurrentConfig || !asr.At.Equal(now.Add(-time.Minute)) {
		t.Fatalf("verification overwrote actual recognition: %+v", asr)
	}
	for _, service := range []string{"llm", "tts"} {
		use := status[service].LastUse
		if use == nil || !use.Success || !use.CurrentConfig || !use.At.Equal(now) {
			t.Fatalf("%s: %+v", service, use)
		}
	}
	if len(reopened.activity.recent()) != 0 {
		t.Fatal("local diagnostics appeared in usage")
	}
}
