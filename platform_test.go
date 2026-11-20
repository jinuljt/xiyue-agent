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

func TestFlowSurvivesRestartAndExpiresOnlyOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	p, err := openPlatform(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	var id string
	err = p.change(func(s *State) error { var e error; id, e = startTask(s, "guided-flow", nil, now); return e })
	if err != nil {
		t.Fatal(err)
	}
	if p.snapshot().Tasks[0].Status != "waiting" {
		t.Fatal("must wait for physical preparation")
	}
	if err = p.change(func(s *State) error { return taskAction(s, id, "confirm", now) }); err != nil {
		t.Fatal(err)
	}
	p, err = openPlatform(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.tick(now.Add(11 * time.Second)); err != nil {
		t.Fatal(err)
	}
	s := p.snapshot()
	n := len(s.Events)
	if s.Tasks[0].Status != "waiting" || s.Tasks[0].Index != 2 {
		t.Fatal("overdue timer must resume at next confirmation")
	}
	if err = p.tick(now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(p.snapshot().Events) != n {
		t.Fatal("timer emitted more than once")
	}
	if err = p.change(func(s *State) error { return taskAction(s, id, "confirm", now.Add(time.Hour)) }); err != nil {
		t.Fatal(err)
	}
	if p.snapshot().Tasks[0].Status != "done" {
		t.Fatal("confirmation should finish workflow")
	}
}

func TestPausePreservesRemainingTime(t *testing.T) {
	p, _ := openPlatform(filepath.Join(t.TempDir(), "state.json"))
	now := time.Now().UTC()
	var id string
	if err := p.change(func(s *State) error {
		var err error
		id, err = startTask(s, "guided-flow", nil, now)
		if err != nil {
			return err
		}
		if err = taskAction(s, id, "confirm", now); err != nil {
			return err
		}
		return taskAction(s, id, "pause", now.Add(3*time.Second))
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.tick(now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if p.snapshot().Tasks[0].Status != "paused" {
		t.Fatal("paused timer must not expire")
	}
	if err := p.change(func(s *State) error { return taskAction(s, id, "resume", now.Add(time.Hour)) }); err != nil {
		t.Fatal(err)
	}
	task := p.snapshot().Tasks[0]
	if task.DueAt == nil || !task.DueAt.Equal(now.Add(time.Hour+7*time.Second)) {
		t.Fatal("resume should retain seven seconds")
	}
}

func TestWeekdayScheduleCoalescesDowntime(t *testing.T) {
	p, _ := openPlatform(filepath.Join(t.TempDir(), "state.json"))
	loc, _ := time.LoadLocation("Asia/Shanghai")
	friday := time.Date(2026, 11, 20, 9, 0, 0, 0, loc)
	if err := p.change(func(s *State) error {
		s.Schedules = append(s.Schedules, Schedule{ID: "test", AbilityID: "reminder", Inputs: map[string]string{"content": "查看天气"}, NextAt: friday, Repeat: "weekdays", Timezone: "Asia/Shanghai", Enabled: true})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.tick(friday.Add(49 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	s := p.snapshot()
	if len(s.Tasks) != 1 || !s.Schedules[0].NextAt.Equal(friday.AddDate(0, 0, 3)) {
		t.Fatal("missed runs must coalesce, preserving next weekday local time")
	}
	if err := p.tick(friday.Add(50 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(p.snapshot().Tasks) != 1 {
		t.Fatal("schedule should not duplicate")
	}
}

func TestFailedPersistenceDoesNotAdvanceMemory(t *testing.T) {
	p, _ := openPlatform(filepath.Join(t.TempDir(), "state.json"))
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	p.path = filepath.Join(blocker, "state.json")
	err := p.change(func(s *State) error {
		_, e := startTask(s, "todo", map[string]string{"content": "交电费"}, time.Now())
		return e
	})
	if err == nil || len(p.snapshot().Tasks) != 0 {
		t.Fatal("failed write must roll back task creation")
	}
}

func TestManifestCannotExecuteCodeOrSpeakCredentials(t *testing.T) {
	m := builtinManifests()[0]
	m.Steps = []Step{{Kind: "shell", Text: "echo bad"}}
	if validateManifest(m) == nil {
		t.Fatal("arbitrary command must not be allowed")
	}
	m.Steps = []Step{{Kind: "say", Text: "{{key}}"}}
	m.Fields = []Field{{Key: "key", Label: "密钥", Kind: "credential_ref"}}
	if validateManifest(m) == nil {
		t.Fatal("credential must never be interpolated into speech")
	}
}

func TestHTTPGuardsAndStaleConfirmation(t *testing.T) {
	p, _ := openPlatform(filepath.Join(t.TempDir(), "state.json"))
	h := (&api{platform: p, assets: map[string][]byte{}}).routes()
	request := func(method, path, body, origin, host string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://127.0.0.1:8765"+path, strings.NewReader(body))
		r.Host = host
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Xiyue-Request", "console")
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := request("POST", "/api/tasks", `{"ability_id":"todo","inputs":{"content":"测试待办"}}`, "https://evil.example", "127.0.0.1:8765"); w.Code != http.StatusForbidden {
		t.Fatal("must reject foreign origin")
	}
	if w := request("GET", "/api/state", "", "", "evil.example"); w.Code != http.StatusForbidden {
		t.Fatal("must reject rebinding host")
	}
	w := request("POST", "/api/tasks", `{"ability_id":"guided-flow","inputs":{}}`, "", "127.0.0.1:8765")
	if w.Code != 201 {
		t.Fatal(w.Body.String())
	}
	var result map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	path := "/api/tasks/" + result["id"] + "/confirm"
	if w = request("POST", path, `{"index":0,"status":"waiting"}`, "", "127.0.0.1:8765"); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w = request("POST", path, `{"index":0,"status":"waiting"}`, "", "127.0.0.1:8765"); w.Code != 400 {
		t.Fatal("stale confirmation must not skip steps")
	}
	if w = request("PUT", "/api/abilities/todo", `{"enabled":true,"config":{"unknown":"value"}}`, "", "127.0.0.1:8765"); w.Code != 400 {
		t.Fatal("unknown field must be rejected")
	}
}
