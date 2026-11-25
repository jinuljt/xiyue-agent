package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestDurableReminderDeliveryAvoidsAmbiguousReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	p, err := openPlatform(path)
	if err != nil {
		t.Fatal(err)
	}
	p.queueSpeech = true
	now := time.Now().UTC()
	if err = p.change(func(s *State) error {
		emit(s, &Task{ID: "t"}, "speech", "新提醒", now)
		s.Events = append(s.Events, Event{ID: "claimed", Kind: "speech", Delivery: "playing", At: now},
			Event{ID: "stale", Kind: "speech", Delivery: "pending", At: now.Add(-time.Hour)})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	p, err = openPlatform(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.recoverDeliveries(); err != nil {
		t.Fatal(err)
	}
	calls := 0
	v := &voiceRuntime{speak: func(_ context.Context, message string) error {
		calls++
		if message != "新提醒" {
			t.Fatal("previously claimed or expired reminder replayed")
		}
		if p.snapshot().Events[0].Delivery != "playing" {
			t.Fatal("must durably claim a reminder before speaking")
		}
		return nil
	}}
	for i := 0; i < 2; i++ {
		if err = p.deliverOne(context.Background(), v, now); err != nil {
			t.Fatal(err)
		}
	}
	events := p.snapshot().Events
	if calls != 1 || events[0].Delivery != "delivered" || events[1].Delivery != "interrupted" || events[2].Delivery != "expired" {
		t.Fatalf("unexpected delivery state: calls=%d events=%+v", calls, events)
	}
}

func TestCountdownRepeatsUntilStopped(t *testing.T) {
	p, err := openPlatform(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	p.queueSpeech = true
	now := time.Now().UTC()
	for _, phrase := range []string{"倒计时一秒钟", "倒计时一分钟"} {
		reply, err := p.voiceTask(phrase, now)
		if err != nil || !reply.timer {
			t.Fatal("countdown was not scheduled", err)
		}
	}
	due := now.Add(time.Minute)
	if err = p.tick(due); err != nil {
		t.Fatal(err)
	}
	var alarms int
	for _, event := range p.snapshot().Events {
		if event.Alarm {
			alarms++
		}
	}
	if alarms != 2 {
		t.Fatalf("all timer lengths need identical alarms, got %d", alarms)
	}
	tones, spoken := 0, 0
	v := &voiceRuntime{}
	v.speak = func(_ context.Context, text string) error {
		spoken++
		if text != "计时结束。" {
			t.Fatal(text)
		}
		return nil
	}
	v.playPCM = func(_ context.Context, src io.Reader) error {
		tones++
		clip, err := io.ReadAll(src)
		if err != nil || !bytes.Equal(clip, alarmTone) {
			t.Fatal("unexpected alarm tone", err)
		}
		if tones == 2 {
			key := make([]byte, strconv.IntSize/4+8)
			binary.NativeEndian.PutUint16(key[strconv.IntSize/4:], 1)
			binary.NativeEndian.PutUint32(key[strconv.IntSize/4+4:], 1)
			readAlarmButton(bytes.NewReader(key), v.stopAlarm)
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if err = p.deliverOne(ctx, v, due); err != nil {
		t.Fatal(err)
	}
	if tones != 2 || spoken != 1 || p.snapshot().Events[0].Delivery != "alarm_stopped" {
		t.Fatalf("alarm did not stop on button: %d tones %d announcements", tones, spoken)
	}
}
