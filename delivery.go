package main

import (
	"context"
	"log"
	"time"
)

const deliveryMaxAge = 5 * time.Minute

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
	p.mu.Lock()
	pending := false
	for _, event := range p.state.Events {
		if event.Delivery == "pending" {
			pending = true
			break
		}
	}
	p.mu.Unlock()
	if !pending {
		return nil
	}

	v.mu.Lock()
	if v.active {
		v.mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	v.active, v.cancel = true, cancel
	v.mu.Unlock()
	defer func() {
		cancel()
		v.mu.Lock()
		v.active, v.cancel = false, nil
		v.mu.Unlock()
	}()
	var claimed Event
	err := p.change(func(s *State) error {
		for i := range s.Events {
			e := &s.Events[i]
			if e.Delivery != "pending" {
				continue
			}
			if now.Sub(e.At) > deliveryMaxAge {
				e.Delivery = "expired"
				continue
			}
			for _, task := range s.Tasks {
				if task.ID == e.TaskID && (task.Status == "cancelled" || task.Status == "paused") {
					e.Delivery = "skipped"
					break
				}
			}
			if e.Delivery != "pending" {
				continue
			}
			if claimed.ID == "" && s.Settings.TTS.Provider == "system" {
				e.Delivery = "playing"
				claimed = *e
			} else if s.Settings.TTS.Provider != "system" {
				e.Delivery = "unavailable"
			}
		}
		return nil
	})
	if err != nil || claimed.ID == "" {
		return err
	}
	result := "delivered"
	if err := v.speak(ctx, claimed.Text); err != nil {
		result = "failed"
	}
	if ctx.Err() != nil {
		result = "interrupted"
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
	ticker := time.NewTicker(time.Second)
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
