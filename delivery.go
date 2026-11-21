package main

import (
	"context"
	"errors"
	"log"
	"time"
)

const deliveryMaxAge = 5 * time.Minute
const alarmDuration = time.Minute

// After a crash, a previously claimed notification may already have played.
// Mark it interrupted and let an explicit task repeat create a new delivery.
func (p *Platform) recoverDeliveries() error {
	for _, event := range p.snapshot().Events {
		if event.Delivery == "playing" {
			return p.change(func(s *State) error {
				for i := range s.Events {
					if s.Events[i].Delivery == "playing" {
						s.Events[i].Delivery = "interrupted"
					}
				}
				return nil
			})
		}
	}
	return nil
}

func (p *Platform) deliverOne(parent context.Context, v *voiceRuntime, now time.Time) error {
	return p.deliverTaskAudio(parent, v, now, "")
}

// A conversation can play audio queued by its own task before opening the
// follow-up microphone. Other notifications wait for the shared runtime.
func (p *Platform) deliverTaskAudio(parent context.Context, v *voiceRuntime, now time.Time, taskID string) error {
	p.mu.Lock()
	pending := false
	alarmPending := false
	for _, event := range p.state.Events {
		if event.Delivery == "pending" && (taskID == "" || taskID == event.TaskID) {
			pending = true
			if event.Alarm && now.Sub(event.At) <= deliveryMaxAge {
				alarmPending = true
			}
		}
	}
	p.mu.Unlock()
	if !pending {
		return nil
	}

	v.mu.Lock()
	if v.active {
		if alarmPending && v.following && v.cancel != nil {
			v.cancel()
		}
		v.mu.Unlock()
		return nil
	}
	if v.conversing && taskID == "" && !alarmPending {
		v.mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(parent)
	v.active, v.cancel = true, cancel
	v.mu.Unlock()
	defer func() {
		cancel()
		v.mu.Lock()
		v.active, v.cancel = false, nil
		v.alarmCancel = nil
		v.mu.Unlock()
	}()
	var claimed Event
	var speech State
	err := p.change(func(s *State) error {
		for i := range s.Events {
			e := &s.Events[i]
			if e.Delivery != "pending" || (taskID != "" && e.TaskID != taskID) || (alarmPending && !e.Alarm) {
				continue
			}
			if now.Sub(e.At) > deliveryMaxAge {
				e.Delivery = "expired"
				continue
			}
			for _, task := range s.Tasks {
				if task.ID != e.TaskID {
					continue
				}
				question := currentQuestion(*s, task)
				if task.Status == "cancelled" || task.Status == "paused" ||
					(e.Kind == "ask" && (question == nil || question.ID != e.ID)) {
					e.Delivery = "skipped"
					break
				}
			}
			if e.Delivery != "pending" {
				continue
			}
			if (e.Kind == "tone" && v.playPCM == nil) ||
				(e.Kind != "tone" && !ttsReady(v, *s) && (!e.Alarm || v.playPCM == nil)) {
				e.Delivery = "unavailable"
			} else if claimed.ID == "" {
				e.Delivery = "playing"
				claimed = *e
				speech = *s
			}
		}
		return nil
	})
	if err != nil || claimed.ID == "" {
		return err
	}
	result := "delivered"
	if claimed.Alarm {
		v.mu.Lock()
		v.alarmCancel = cancel
		v.mu.Unlock()
	}
	speechCtx, stopSpeech := context.WithTimeout(ctx, 30*time.Second)
	var playErr error
	if claimed.Kind == "tone" {
		clip, ok := deviceTones[claimed.Text]
		if ok {
			playErr = v.playTone(speechCtx, clip)
		} else {
			playErr = errUnknownTone
		}
	} else {
		playErr = p.playConfiguredSpeech(speechCtx, v, speech, claimed.Text)
	}
	if playErr != nil {
		result = "failed"
	}
	stopSpeech()
	if claimed.Alarm && ctx.Err() == nil {
		ringCtx, stopRing := context.WithTimeout(ctx, alarmDuration)
		if err := v.ring(ringCtx, 2*time.Second); err == nil {
			result = "delivered"
		}
		stopRing()
	}
	if ctx.Err() != nil || errors.Is(playErr, context.Canceled) {
		result = "interrupted"
		if claimed.Alarm && parent.Err() == nil {
			result = "alarm_stopped"
		}
	}
	return p.change(func(s *State) error {
		for i := range s.Events {
			if s.Events[i].ID == claimed.ID {
				s.Events[i].Delivery = result
				break
			}
		}
		return nil
	})
}

func (p *Platform) serveDeliveries(ctx context.Context, v *voiceRuntime) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if err := p.deliverOne(ctx, v, now); err != nil {
				log.Print("提醒播报状态保存失败")
			}
		}
	}
}
