package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestAlarmSnoozePersistsAndKeepsTheRecurringTime(t *testing.T) {
	p, err := openPlatform(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	p.queueSpeech = true
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	if err := p.change(func(s *State) error {
		s.Schedules = append(s.Schedules, Schedule{ID: "alarm", AlarmName: "fixture", Enabled: true, NextAt: now, Repeat: "daily", Timezone: "Asia/Shanghai"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.tick(now); err != nil {
		t.Fatal(err)
	}
	if ev := p.snapshot().Events[0]; !ev.Alarm || ev.ClockID != "alarm" || ev.Delivery != "pending" {
		t.Fatalf("missing alarm: %+v", ev)
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &api{platform: p, voice: &voiceRuntime{alarmCancel: cancel, alarmClock: "alarm"}}
	if err := a.clockAction("alarm", "snooze", now); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() == nil {
		t.Fatal("snooze left a ringing alarm")
	}
	if err := a.clockAction("alarm", "snooze", now); err == nil {
		t.Fatal("duplicate snooze changed the alarm")
	}
	p, err = openPlatform(p.path)
	if err != nil {
		t.Fatal(err)
	}
	p.queueSpeech = true
	if err := p.tick(now.Add(5 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	s := p.snapshot()
	if len(s.Events) != 2 || s.Schedules[0].SnoozeAt != nil || !s.Schedules[0].NextAt.Equal(now.Add(24*time.Hour)) {
		t.Fatalf("incorrect snooze state: %+v", s.Schedules[0])
	}
}

func TestAutomationRunsConditionsAndActionsInOrder(t *testing.T) {
	p, err := openPlatform(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	p.queueSpeech = true
	v := &voiceRuntime{active: true, playPCM: func(context.Context, io.Reader) error { return nil }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	room := newRoomAudio(ctx, v) // Busy output prevents opening a network connection.
	if err := p.change(func(s *State) error {
		s.RoomAudio = &roomAudioConfig{Source: "http://" + netip.AddrFrom4([4]byte{10, 0, 0, 1}).String() + "/audio.wav", Volume: 70}
		s.Rules = append(s.Rules, automationRule{ID: "start", Name: "fixture", Schedule: Schedule{NextAt: now, Repeat: "once", Enabled: true, Timezone: "Asia/Shanghai"},
			StateConditions: []automationStateCondition{{Source: roomStateID, Equals: "off"}},
			Actions: []automationStep{{Action: "tool", Value: roomStartToolID}, {Action: "speak", Value: "fixture"},
				{Action: "tool", Value: timerToolID, Parameters: map[string]string{"seconds": "15"}}}})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.runAutomation(now, room); err != nil {
		t.Fatal(err)
	}
	if err := p.runAutomation(now.Add(time.Second), room); err != nil {
		t.Fatal(err)
	}
	if err := p.change(func(s *State) error { s.Events[0].Delivery = "delivered"; return nil }); err != nil {
		t.Fatal(err)
	}
	for i := 2; i <= 3; i++ {
		if err := p.runAutomation(now.Add(time.Duration(i)*time.Second), room); err != nil {
			t.Fatal(err)
		}
	}
	s := p.snapshot()
	if s.Rules[0].Schedule.Enabled || s.Rules[0].Runs[0].Status != "completed" || len(s.Events) != 1 || len(s.Schedules) != 1 {
		t.Fatal("rule did not execute its actions once")
	}
	if err := p.startAutomation(now, room, "start", "manual"); err != nil {
		t.Fatal(err)
	}
	if s := p.snapshot(); s.Rules[0].Runs[0].Status != "skipped" || len(s.Schedules) != 1 {
		t.Fatal("false condition did not prevent actions")
	}
	if _, err := p.automationTool(room, roomStopToolID, nil, now); err != nil {
		t.Fatal(err)
	}
}

func TestAutomationWaitConfirmAndTimeout(t *testing.T) {
	p, err := openPlatform(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	p.queueSpeech = true
	rule := automationRule{ID: "cooking", Name: "炖菜", Trigger: "manual", Schedule: Schedule{Enabled: true},
		Actions: []automationStep{{Action: "confirm", Value: "准备好后说继续下一步。", Seconds: 10, OnTimeout: "stop"},
			{Action: "wait", Seconds: 4}, {Action: "tool", Value: timerToolID, Parameters: map[string]string{"seconds": "3"}}}}
	if err := p.change(func(s *State) error { s.Rules = []automationRule{rule}; return nil }); err != nil {
		t.Fatal(err)
	}
	tick := func(seconds int) {
		t.Helper()
		if err := p.runAutomation(now.Add(time.Duration(seconds)*time.Second), nil); err != nil {
			t.Fatal(err)
		}
	}
	run := func() automationRun { return p.snapshot().Rules[0].Runs[0] }
	start := func(seconds int) {
		t.Helper()
		if err := p.startAutomation(now.Add(time.Duration(seconds)*time.Second), nil, rule.ID, "manual"); err != nil {
			t.Fatal(err)
		}
		tick(seconds)
		if run().Phase != "speech" {
			t.Fatal("confirmation advanced before prompt was delivered")
		}
		if err := p.change(func(s *State) error { s.Events[len(s.Events)-1].Delivery = "delivered"; return nil }); err != nil {
			t.Fatal(err)
		}
		tick(seconds + 1)
		if run().Phase != "confirm" {
			t.Fatal("confirmation did not start after prompt")
		}
	}
	start(0)
	first := run().ID
	if reply, err := p.automationVoiceConfirmation("继续下一步", now.Add(2*time.Second)); err != nil || !reply.handled {
		t.Fatalf("voice confirmation failed: %v", err)
	}
	tick(2)
	tick(5)
	if run().Step != 2 || len(p.snapshot().Schedules) != 0 {
		t.Fatal("wait was bypassed")
	}
	tick(6)
	tick(7)
	if run().Status != "completed" || len(run().Results) != 3 || len(p.snapshot().Schedules) != 1 {
		t.Fatalf("confirmed flow failed: %+v", run())
	}
	start(20)
	if err := p.confirmAutomation(rule.ID, first, 1, now.Add(23*time.Second)); err == nil {
		t.Fatal("a stale confirmation was accepted")
	}
	tick(31)
	if run().Status != "failed" || len(p.snapshot().Schedules) != 1 {
		t.Fatal("timed out confirmation should not start a timer")
	}
}

func TestAutomationConfirmJumpsToChosenStep(t *testing.T) {
	p, err := openPlatform(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	until := now.Add(time.Minute)
	if err := p.change(func(s *State) error {
		s.Rules = []automationRule{{ID: "jump", Actions: []automationStep{
			{Action: "confirm", Next: 3}, {Action: "speak"}, {Action: "wait"},
		}, Runs: []automationRun{{ID: "run", Status: "running", Step: 1, Phase: "confirm", Until: &until}}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.confirmAutomation("jump", "run", 1, now); err != nil {
		t.Fatal(err)
	}
	run := p.snapshot().Rules[0].Runs[0]
	if run.Step != 3 || len(run.Results) != 2 || run.Results[1] != "已跳过" {
		t.Fatalf("confirmation moved to wrong step: %+v", run)
	}
}

func TestAutomationTimeConditionAcrossMidnight(t *testing.T) {
	rule := automationRule{StateConditions: []automationStateCondition{{Source: roomStateID, Equals: "off"}}, After: "21:00", Before: "06:00"}
	night := time.Date(2026, 11, 24, 18, 0, 0, 0, time.UTC)
	sources := automationStateSources(&State{}, nil)
	stopped := map[string]automationObservation{roomStateID: {value: "off"}}
	running := map[string]automationObservation{roomStateID: {value: "on"}}
	if !automationConditions(rule, "Asia/Shanghai", night, sources, stopped) ||
		automationConditions(rule, "Asia/Shanghai", night, sources, running) ||
		automationConditions(rule, "Asia/Shanghai", night, sources, nil) ||
		automationConditions(rule, "Asia/Shanghai", night.Add(8*time.Hour), sources, stopped) {
		t.Fatal("time or state condition did not match device timezone")
	}
}

func TestAutomationHomeAssistantTransition(t *testing.T) {
	var on atomic.Bool
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/states" || r.Header.Get("Authorization") != "Bearer fixture-token" {
			http.Error(w, "wrong request", 400)
			return
		}
		state := "off"
		if on.Load() {
			state = "on"
		}
		fmt.Fprintf(w, `[{"entity_id":"binary_sensor.door","state":%q}]`, state)
	}))
	defer fixture.Close()
	p, err := openPlatform(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.change(func(s *State) error {
		s.HomeAssistant = fixture.URL
		s.HTTPTools = []networkTool{{ID: "door", Kind: "ha", Entity: "binary_sensor.door", Operation: "state", Automation: true}}
		s.Rules = []automationRule{{ID: "door-opens", Name: "门打开", Trigger: "state", StateSource: "door", ToState: "on",
			Schedule: Schedule{Enabled: true}, Actions: []automationStep{{Action: "wait", Seconds: 1}}}}
		return p.putNetworkKey(s, haSecret, "fixture-token", false)
	}); err != nil {
		t.Fatal(err)
	}
	baseline := map[string]automationStateBaseline{}
	poll := func() int {
		t.Helper()
		if err := p.pollAutomationStates(context.Background(), nil, baseline); err != nil {
			t.Fatal(err)
		}
		return len(p.snapshot().Rules[0].Runs)
	}
	on.Store(true)
	if poll() != 0 {
		t.Fatal("first sample must only set baseline")
	}
	on.Store(false)
	if poll() != 0 {
		t.Fatal("unexpected trigger on close")
	}
	on.Store(true)
	if poll() != 1 || poll() != 1 {
		t.Fatal("opening must trigger exactly once")
	}
}
