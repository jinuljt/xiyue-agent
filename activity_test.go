package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestActivityRollupSurvivesDetailExpiryAndDialogueIsPrivate(t *testing.T) {
	p, err := openPlatform(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	store := p.activity
	now := time.Now().UTC()
	audio := int64(3_600)
	row := UsageRecord{Service: "asr", Provider: "doubao", Model: doubaoASRResource, At: now.AddDate(0, 0, -10), Success: true, AudioMS: &audio}
	store.add(row, "Asia/Shanghai")
	row.At = now
	store.add(row, "Asia/Shanghai")
	store.dialogue("默认关闭不记录", "", voiceResult{})
	if len(store.data.Dialogues) != 0 {
		t.Fatal("dialogue logging must be opt-in")
	}
	routes := (&api{platform: p}).routes()
	call := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "http://127.0.0.1"+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Xiyue-Request", "console")
		w := httptest.NewRecorder()
		routes.ServeHTTP(w, req)
		return w
	}
	if w := call(http.MethodPut, "/api/dialogues", `{"enabled":true}`); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	store.dialogue("确认登录 1234", "", voiceResult{})
	store.dialogue("测试原文", "测试回复", voiceResult{Stage: "completed"})
	if w := call(http.MethodGet, "/api/dialogues", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "测试回复") || strings.Contains(w.Body.String(), "1234") {
		t.Fatal(w.Body.String())
	}
	if w := call(http.MethodGet, "/api/usage?type=asr&days=30", ""); w.Code != 200 {
		t.Fatal(w.Body.String())
	} else {
		var view struct {
			Daily  []usageBucket
			Recent []UsageRecord
		}
		if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
			t.Fatal(err)
		}
		if len(view.Daily) != 2 || len(view.Recent) != 1 || view.Recent[0].Estimated == nil || *view.Recent[0].Estimated != .001 {
			t.Fatal("daily rollup or cost lost on detail rotation")
		}
	}
	if w := call(http.MethodGet, "/api/usage?type=tts", ""); strings.Contains(w.Body.String(), doubaoASRResource) {
		t.Fatal("usage filter mismatch")
	}
	if err := store.flush(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(store.path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("activity files must be private")
	}
	id := store.data.Dialogues[0].ID
	if w := call(http.MethodDelete, "/api/dialogues?id="+id, ""); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	reopened := openActivity(store.path)
	if reopened.loadErr != nil || !reopened.data.Logging || len(reopened.data.Dialogues) != 0 || len(reopened.data.Daily) != 2 {
		t.Fatal("activity changes did not survive reopening")
	}
}

func TestUsageCostUsesCacheRateAndDoesNotGuessMissingUnits(t *testing.T) {
	in, out, hit := int64(500), int64(100), int64(200)
	inRate, outRate, hitRate := 2.0, 8.0, .04
	r := usageRate{PerMillionIn: &inRate, PerMillionOut: &outRate, PerMillionHit: &hitRate}
	u := UsageRecord{Service: "llm", InputTokens: &in, OutputTokens: &out, CachedInputTokens: &hit}
	if got, ok := r.estimate(u); !ok || got < .0014079 || got > .0014081 {
		t.Fatalf("cost %v %v", got, ok)
	}
	u.CachedInputTokens = nil
	if _, ok := r.estimate(u); ok {
		t.Fatal("missing cache metering must not be guessed")
	}
}
