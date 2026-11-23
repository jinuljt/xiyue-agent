package main

import (
	"context"
	"io"
	"net/netip"
	"path/filepath"
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
			Condition: "room_off", Actions: []automationStep{{Action: "room_start"}, {Action: "speak", Value: "fixture"}, {Action: "timer", Seconds: 15}}})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.runAutomation(now, room); err != nil {
		t.Fatal(err)
	}
	s := p.snapshot()
	if s.Rules[0].Schedule.Enabled || s.Rules[0].Runs[0].Status != "completed" || len(s.Events) != 1 || len(s.Schedules) != 1 {
		t.Fatal("rule did not execute its actions once")
	}
	if err := p.executeAutomation(now, room, "start", "manual"); err != nil {
		t.Fatal(err)
	}
	if s := p.snapshot(); s.Rules[0].Runs[0].Status != "skipped" || len(s.Schedules) != 1 {
		t.Fatal("false condition did not prevent actions")
	}
	if err := p.runAutomationStep(room, automationStep{Action: "room_stop"}, now); err != nil {
		t.Fatal(err)
	}
}
