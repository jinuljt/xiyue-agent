package main

import (
	"context"
	"path/filepath"
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
