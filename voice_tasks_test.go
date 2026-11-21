package main

import (
	"path/filepath"
	"testing"
	"time"
)

func TestSpokenTimersUseLocalScheduling(t *testing.T) {
	p, err := openPlatform(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, phrase := range []string{"三分钟后，提醒我取茶。", "计时10秒", "倒计时一秒钟。"} {
		if answer, handled, err := p.voiceTask(phrase, now); !handled || err != nil || answer == "" {
			t.Fatalf("voice task %q: %q %t %v", phrase, answer, handled, err)
		}
	}
	if _, handled, err := p.voiceTask("如何做三分钟后提醒我的程序", now); handled || err != nil {
		t.Fatal("informational request should not act")
	}
	s := p.snapshot()
	if len(s.Schedules) != 3 || !s.Schedules[0].NextAt.Equal(now.Add(3*time.Minute)) ||
		s.Schedules[0].Inputs["content"] != "取茶" || !s.Schedules[1].NextAt.Equal(now.Add(10*time.Second)) ||
		!s.Schedules[2].NextAt.Equal(now.Add(time.Second)) {
		t.Fatalf("wrong local schedules: %+v", s.Schedules)
	}
	if err = p.tick(now.Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	s = p.snapshot()
	if s.Schedules[1].Enabled || s.Schedules[2].Enabled || len(s.Tasks) != 2 || s.Tasks[0].Status != "done" {
		t.Fatal("one-shot timer did not complete")
	}
}
