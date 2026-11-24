package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestAutomationBindsHTTPResultAndBranches(t *testing.T) {
	var pending atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if pending.Load() {
			fmt.Fprint(w, `{"items":[{"status":"pending","ready":false}]}`)
			return
		}
		fmt.Fprint(w, `{"items":[{"status":"shipped","ready":true,"number":100000000000000000001}]}`)
	}))
	defer upstream.Close()
	p, err := openPlatform(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	p.queueSpeech = true
	tool := networkTool{ID: "order", Kind: "http", Method: "GET", Name: "订单", Description: "查订单", Endpoint: upstream.URL, Automation: true, Unit: "元"}
	steps := []automationStep{
		{Action: "http_tool", Value: tool.ID, SaveAs: "result"},
		{Action: "if", Value: "result.items.0.ready", Equals: json.RawMessage("true")},
		{Action: "speak", Value: "订单状态 {{result.items.0.status}}"},
	}
	if err := p.change(func(s *State) error {
		s.HTTPTools = append(s.HTTPTools, tool)
		rule := automationRule{ID: "order-flow", Trigger: "manual", Name: "查订单", Schedule: Schedule{Enabled: true}, Actions: steps}
		if err := validateAutomation(s, rule); err != nil {
			return err
		}
		s.Rules = []automationRule{rule}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tick := func(seconds int) {
		t.Helper()
		if err := p.runAutomation(now.Add(time.Duration(seconds)*time.Second), nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.startAutomation(now, nil, "order-flow", "manual"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i <= 2; i++ {
		tick(i)
	}
	s := p.snapshot()
	if len(s.Events) != 1 || s.Events[0].Text != "订单状态 shipped" {
		t.Fatalf("unexpected playback: %+v", s.Events)
	}
	if err := p.change(func(s *State) error { s.Events[0].Delivery = "delivered"; return nil }); err != nil {
		t.Fatal(err)
	}
	tick(3)
	if len(p.automationValues) != 0 || p.snapshot().Rules[0].Runs[0].Status != "completed" {
		t.Fatal("values should be dropped when the run finishes")
	}

	pending.Store(true)
	if err := p.startAutomation(now, nil, "order-flow", "manual"); err != nil {
		t.Fatal(err)
	}
	tick(4)
	tick(5)
	s = p.snapshot()
	if s.Rules[0].Runs[0].Status != "completed" || s.Rules[0].Runs[0].Results[2] != "已跳过" || len(s.Events) != 1 {
		t.Fatal("false comparison should skip the playback")
	}
}

func TestNetworkResponseRetainsJSONType(t *testing.T) {
	cases := []struct {
		body, path, want string
	}{
		{`{"items":[{"ok":true}]}`, "items.0.ok", "true"},
		{`{"x/y":{"a~b":null}}`, "/x~1y/a~0b", "null"},
		{`[1,{"n":100000000000000000001}]`, "1.n", "100000000000000000001"},
		{`{"a":1}`, "", `{"a":1}`},
		{"hello", "", `"hello"`},
	}
	for _, c := range cases {
		raw, err := selectNetworkValue([]byte(c.body), c.path)
		if err != nil || string(raw) != c.want {
			t.Fatalf("path %s: %v %s", c.path, err, raw)
		}
	}
}

func TestMigrationKeepsLegacyDependenciesAndHistory(t *testing.T) {
	p, err := openPlatform(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	tea := Manifest{
		ID: "my-flow", Version: "1", Name: "我的旧流程",
		Fields: []Field{{Key: "item", Label: "内容", Kind: "text", Required: true}},
		Steps:  []Step{{Kind: "confirm", Text: "准备{{item}}"}, {Kind: "timer", Text: "开始计时", Seconds: 7}},
	}
	if err := p.change(func(s *State) error {
		s.Abilities[tea.ID] = Ability{Manifest: tea, Enabled: true, Config: map[string]string{"item": "材料"}}
		s.Rules = []automationRule{{ID: "parent", Trigger: "manual", Schedule: Schedule{Enabled: true}, Actions: []automationStep{{Action: "flow", Value: tea.ID}}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.migrateLegacyFlows(); err != nil {
		t.Fatal(err)
	}
	s := p.snapshot()
	if len(s.Rules) != 2 || s.Rules[1].Actions[0].Value != "准备材料" || s.Rules[1].Actions[2].Seconds != 7 || !s.Abilities[tea.ID].Enabled {
		t.Fatalf("legacy rule values or references were lost: %+v", s.Rules)
	}
	now := time.Now()
	if err := p.startAutomation(now, nil, "parent", "manual"); err != nil {
		t.Fatal(err)
	}
	if err := p.runAutomation(now, nil); err != nil {
		t.Fatal(err)
	}
	s = p.snapshot()
	child := s.Rules[0].Runs[0].ChildID
	if child == "" || s.Rules[0].Runs[0].Phase != "child" || s.Tasks[0].Status != "waiting" {
		t.Fatal("parent should wait for legacy confirmation")
	}
	if err := p.change(func(s *State) error { return taskAction(s, child, "confirm", now) }); err != nil {
		t.Fatal(err)
	}
	if err := p.tick(now.Add(8 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := p.runAutomation(now.Add(8*time.Second), nil); err != nil {
		t.Fatal(err)
	}
	if p.snapshot().Rules[0].Runs[0].Status != "completed" {
		t.Fatal("parent should finish after its child")
	}
	reopened, err := openPlatform(p.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.migrateLegacyFlows(); err != nil || len(reopened.snapshot().Rules) != 2 {
		t.Fatal("migration should not run twice")
	}
	bytes, err := os.ReadFile(p.path)
	if err != nil || !json.Valid(bytes) {
		t.Fatal("migration failed to persist compatible state")
	}
}
