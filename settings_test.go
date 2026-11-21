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

func settingsInput(c SystemConfig) SettingsInput {
	convert := func(s ServiceConfig) ServiceInput {
		return ServiceInput{Provider: s.Provider, Endpoint: s.Endpoint, Model: s.Model}
	}
	return SettingsInput{Revision: c.Revision, Name: c.Name, WakeWord: c.WakeWord, Timezone: c.Timezone, Mode: c.Mode, ASR: convert(c.ASR), LLM: convert(c.LLM), TTS: convert(c.TTS), External: convert(c.External)}
}
func TestSettingsPersistAndNeverExposeCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	p, err := openPlatform(path)
	if err != nil {
		t.Fatal(err)
	}
	in := settingsInput(p.snapshot().Settings)
	in.Name = "测试音箱"
	in.LLM.APIKey = "synthetic-credential-for-local-test"
	in.ASR = ServiceInput{Provider: "doubao", Model: "volc.seedasr.sauc.concurrent", APIKey: "synthetic-asr-key"}
	if err = p.saveSettings(in); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), in.LLM.APIKey) {
		t.Fatal("plaintext persisted")
	}
	info, err := os.Stat(path + ".key")
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("key file must be private")
	}
	p, err = openPlatform(path)
	if err != nil {
		t.Fatal(err)
	}
	if p.snapshot().Settings.Name != "测试音箱" || !p.snapshot().Settings.LLM.KeyConfigured {
		t.Fatal("configuration lost after restart")
	}
	if asr := p.snapshot().Settings.ASR; asr.Model != doubaoASRResource || !asr.KeyConfigured {
		t.Fatal("Doubao settings must use the hourly resource and retain the credential")
	}
	public, _ := json.Marshal(p.publicState())
	if strings.Contains(string(public), "sealed") || strings.Contains(string(public), in.LLM.APIKey) {
		t.Fatal("credential material leaked through public state")
	}
	old := p.snapshot().Sealed["service:llm"]
	in = settingsInput(p.snapshot().Settings)
	in.LLM.Model = "another-model"
	if err = p.saveSettings(in); err != nil {
		t.Fatal(err)
	}
	if p.snapshot().Sealed["service:llm"] != old {
		t.Fatal("blank key must preserve existing credential")
	}
	in = settingsInput(p.snapshot().Settings)
	in.LLM.Endpoint = "https://another-provider.example"
	if err = p.saveSettings(in); err == nil {
		t.Fatal("must not forward saved credential to a changed endpoint")
	}
	in.LLM.ClearKey = true
	if err = p.saveSettings(in); err != nil {
		t.Fatal(err)
	}
	if p.snapshot().Settings.LLM.KeyConfigured || p.snapshot().Sealed["service:llm"] != "" {
		t.Fatal("clear should remove credential")
	}
	if err = p.saveSettings(in); err == nil {
		t.Fatal("stale settings revision must be rejected")
	}
}

func TestTextPreviewRecordsRealUsageWithoutConversation(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer synthetic-local-credential" {
			t.Error("unexpected request")
		}
		data, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(data), "synthetic-private-message") {
			t.Error("message missing")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"local response"}}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`)
	}))
	defer provider.Close()
	p, _ := openPlatform(filepath.Join(t.TempDir(), "state.json"))
	in := settingsInput(p.snapshot().Settings)
	in.LLM = ServiceInput{Provider: "custom", Endpoint: provider.URL, Model: "local-test", APIKey: "synthetic-local-credential"}
	if err := p.saveSettings(in); err != nil {
		t.Fatal(err)
	}
	reply, err := p.chat(context.Background(), "synthetic-private-message")
	if err != nil || reply != "local response" {
		t.Fatal("preview failed", err)
	}
	s := p.snapshot()
	if len(s.Usage) != 1 || !s.Usage[0].Success || s.Usage[0].InputTokens == nil || *s.Usage[0].InputTokens != 7 || s.Usage[0].ConfigRevision != s.Settings.Revision {
		t.Fatal("usage not recorded")
	}
	data, _ := os.ReadFile(p.path)
	if strings.Contains(string(data), "synthetic-private-message") || strings.Contains(string(data), "local response") {
		t.Fatal("conversation persisted in usage log")
	}
}

func TestProviderErrorsAndRedirectsDoNotLeakSecrets(t *testing.T) {
	leaked := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked = true }))
	defer target.Close()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer provider.Close()
	p, _ := openPlatform(filepath.Join(t.TempDir(), "state.json"))
	in := settingsInput(p.snapshot().Settings)
	in.LLM = ServiceInput{Provider: "custom", Endpoint: provider.URL, Model: "local-test", APIKey: "synthetic-local-credential"}
	if err := p.saveSettings(in); err != nil {
		t.Fatal(err)
	}
	_, err := p.chat(context.Background(), "hello")
	if err == nil || leaked || strings.Contains(err.Error(), in.LLM.APIKey) {
		t.Fatal("redirect or error leaked credentials")
	}
	if p.snapshot().Usage[0].Success {
		t.Fatal("redirect must not count as successful model call")
	}
}

func TestAbilitySecretFormStoresOnlyPresence(t *testing.T) {
	p, _ := openPlatform(filepath.Join(t.TempDir(), "state.json"))
	m := Manifest{ID: "secret-test", Name: "配置表单测试", Version: "0.2.0", Fields: []Field{{Key: "access", Label: "访问密钥", Kind: "secret"}}, Steps: []Step{{Kind: "say", Text: "已启动"}}}
	if err := validateManifest(m); err != nil {
		t.Fatal(err)
	}
	if err := p.change(func(s *State) error {
		s.Abilities[m.ID] = Ability{Manifest: m, Config: map[string]string{}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	h := (&api{platform: p, assets: map[string][]byte{}}).routes()
	r := httptest.NewRequest("PUT", "http://127.0.0.1:8765/api/abilities/secret-test", strings.NewReader(`{"enabled":true,"config":{},"secrets":{"access":"synthetic-ability-credential"}}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Xiyue-Request", "console")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	s := p.publicState()
	if s.Abilities[m.ID].Config["access"] != "已配置" {
		t.Fatal("missing presence marker")
	}
	data, _ := json.Marshal(s)
	if strings.Contains(string(data), "synthetic-ability-credential") || strings.Contains(string(data), "sealed") {
		t.Fatal("ability secret leaked")
	}
	m.Steps[0].Text = "{{access}}"
	if validateManifest(m) == nil {
		t.Fatal("secret must not be spoken")
	}
}

func TestAnthropicPreviewCompletesFirstSetup(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model     string
			MaxTokens int `json:"max_tokens"`
		}
		if r.URL.Path != "/v1/messages" || r.Header.Get("x-api-key") != "test-credential" ||
			r.Header.Get("anthropic-version") != "2023-06-01" || r.Header.Get("Authorization") != "" {
			t.Error("incorrect Anthropic request")
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model != "manually-entered-model" || body.MaxTokens != 512 {
			t.Errorf("incorrect request body: %+v, %v", body, err)
		}
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"reply"}],"usage":{"input_tokens":10,"output_tokens":3}}`)
	}))
	defer provider.Close()
	p, err := openPlatform(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	input := settingsInput(p.snapshot().Settings)
	input.LLM = ServiceInput{Provider: "custom_anthropic", Endpoint: provider.URL + "/v1", Model: "manually-entered-model", APIKey: "test-credential"}
	if err := p.saveSettings(input); err != nil {
		t.Fatal(err)
	}
	if p.snapshot().Settings.SetupComplete {
		t.Fatal("saving must not dismiss the setup guide")
	}
	reply, err := p.chat(context.Background(), "hello")
	if err != nil || reply != "reply" {
		t.Fatalf("preview returned %q, %v", reply, err)
	}
	input = settingsInput(p.snapshot().Settings)
	input.Name = "Updated"
	if err := p.saveSettings(input); err != nil {
		t.Fatal(err)
	}
	reopened, err := openPlatform(p.path)
	if err != nil {
		t.Fatal(err)
	}
	state := reopened.snapshot()
	if !state.Settings.SetupComplete || len(state.Usage) != 1 || state.Usage[0].InputTokens == nil || *state.Usage[0].InputTokens != 10 {
		t.Fatal("setup completion and token usage must survive edits and reload")
	}
}

func TestHybridRoutesOnlyTheExplicitRequest(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/chat/completions":
			if r.Header.Get("Authorization") != "Bearer test-model" {
				t.Error("model received wrong credential")
			}
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"model"}}]}`)
		case "/v1/chat":
			var task map[string]string
			if err := json.NewDecoder(r.Body).Decode(&task); err != nil || task["message"] != "概括当前工作区" {
				t.Error("connector did not receive just the delegated task")
			}
			if r.Header.Get("Authorization") != "Bearer test-connector" {
				t.Error("connector received wrong credential")
			}
			_, _ = io.WriteString(w, `{"reply":"external"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()
	p, err := openPlatform(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	in := settingsInput(p.snapshot().Settings)
	in.Mode = "hybrid"
	in.LLM = ServiceInput{Provider: "custom", Endpoint: provider.URL, Model: "test", APIKey: "test-model"}
	in.External = ServiceInput{Provider: "http_json", Endpoint: provider.URL + "/v1/chat", APIKey: "test-connector"}
	if err := p.saveSettings(in); err != nil {
		t.Fatal(err)
	}
	for text, want := range map[string]string{"什么内容适合交给外部助手？": "model", "交给外部助手，概括当前工作区": "external"} {
		if reply, err := p.chat(context.Background(), text); err != nil || reply != want {
			t.Fatalf("route failed: got %q, error %v", reply, err)
		}
	}
	if _, err := p.chat(context.Background(), "交给外部助手。"); err == nil {
		t.Fatal("empty delegated task should be rejected")
	}
	if len(p.snapshot().Usage) != 2 {
		t.Fatal("each nonempty request should use exactly one service")
	}
}

func TestServiceHealthReportsOnlyCurrentConfigurationResults(t *testing.T) {
	config := defaultSettings()
	a := &api{}
	if got := a.serviceStatus(State{Settings: config}); got["asr"].State != "disabled" ||
		got["tts"].State != "disabled" || got["llm"].State != "not_configured" {
		t.Fatalf("unexpected initial status: %+v", got)
	}
	config.LLM.KeyConfigured = true
	config.ASR = ServiceConfig{Provider: "sensevoice", Endpoint: "http://localhost/v1"}
	a.voice = &voiceRuntime{speak: func(context.Context, string) error { return nil },
		last: &voiceResult{Revision: config.Revision, Stage: "playback"}}
	state := State{Settings: config, Usage: []UsageRecord{
		{ConfigRevision: config.Revision, Service: "llm", Success: true},
		{ConfigRevision: config.Revision + 1, Service: "llm", Success: false},
	}}
	got := a.serviceStatus(state)
	if got["asr"].State != "succeeded" || got["tts"].State != "failed" || got["llm"].State != "succeeded" {
		t.Fatalf("unexpected verified status: %+v", got)
	}
	state.Settings.Revision++
	got = a.serviceStatus(state)
	if got["asr"].State != "configured" || got["tts"].State != "configured" || got["llm"].State != "failed" {
		t.Fatalf("unexpected new revision status: %+v", got)
	}
}

func TestVerifyUsesUnsavedConfigWithoutPersistingOrForwardingOldKeys(t *testing.T) {
	type observed struct{ Model, Auth string }
	calls := make(chan observed, 4)
	gateway := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var in struct{ Model string }
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				http.Error(w, "invalid payload", 400)
				return
			}
			calls <- observed{Model: in.Model, Auth: r.Header.Get("Authorization")}
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"OK"}}]}`)
		}))
	}
	original, other := gateway(), gateway()
	defer original.Close()
	defer other.Close()
	path := filepath.Join(t.TempDir(), "state.json")
	p, err := openPlatform(path)
	if err != nil {
		t.Fatal(err)
	}
	in := settingsInput(p.snapshot().Settings)
	in.LLM = ServiceInput{Provider: "custom", Endpoint: original.URL, Model: "saved-model", APIKey: "fixture-old-key"}
	if err := p.saveSettings(in); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	handler := (&api{platform: p}).routes()
	test := func(config ServiceInput, auth string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"service": "llm", "config": config})
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8765/api/settings/verify", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Xiyue-Request", "console")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("verify failed with %d: %s", w.Code, w.Body.String())
		}
		if call := <-calls; call.Model != config.Model || call.Auth != auth {
			t.Fatal("verification used the wrong model or credentials")
		}
	}
	test(ServiceInput{Provider: "custom", Endpoint: other.URL, Model: "different-model"}, "")
	test(ServiceInput{Provider: "custom", Endpoint: other.URL, Model: "draft-model", APIKey: "fixture-draft-key"}, "Bearer fixture-draft-key")
	test(ServiceInput{Provider: "custom", Endpoint: original.URL + "/", Model: "unsaved-model"}, "Bearer fixture-old-key")
	test(ServiceInput{Provider: "custom", Endpoint: original.URL, Model: "no-key-model", ClearKey: true}, "")
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatal("draft verification persisted changes")
	}
}
