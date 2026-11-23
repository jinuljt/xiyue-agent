package main

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"
)

type automationStep struct {
	Action  string `json:"action"`
	Value   string `json:"value,omitempty"`
	Seconds int    `json:"seconds,omitempty"`
}

type automationRun struct {
	ID     string    `json:"id"`
	At     time.Time `json:"at"`
	Source string    `json:"source"`
	Status string    `json:"status"`
	Step   int       `json:"step,omitempty"`
	Error  string    `json:"error,omitempty"`
}

type automationRule struct {
	ID        string           `json:"id"`
	Name      string           `json:"name"`
	Schedule  Schedule         `json:"schedule"`
	Condition string           `json:"condition,omitempty"`
	Actions   []automationStep `json:"actions"`
	Runs      []automationRun  `json:"runs,omitempty"`
}

func setRuleTime(sc *Schedule, local, tz string, now time.Time) error {
	next, err := scheduleLocalTime(local, tz)
	if err != nil {
		return err
	}
	if !next.After(now) || next.After(now.AddDate(2, 0, 0)) {
		return errors.New("请选择未来两年内的时间")
	}
	if sc.Repeat != "once" && sc.Repeat != "daily" && sc.Repeat != "weekdays" {
		return errors.New("请选择重复方式")
	}
	loc, _ := time.LoadLocation(tz)
	if sc.Repeat == "weekdays" && (next.In(loc).Weekday() == time.Saturday || next.In(loc).Weekday() == time.Sunday) {
		return errors.New("工作日规则须从周一至周五开始")
	}
	sc.NextAt, sc.Timezone = next, tz
	return nil
}

func validateAutomation(s *State, rule automationRule) error {
	if len(rule.Name) == 0 || len(rule.Name) > 120 || len(rule.Actions) == 0 || len(rule.Actions) > 8 {
		return errors.New("请填写规则名称并添加 1 至 8 个动作")
	}
	if rule.Condition != "" && rule.Condition != "room_on" && rule.Condition != "room_off" {
		return errors.New("未知执行条件")
	}
	for _, step := range rule.Actions {
		switch step.Action {
		case "room_start", "room_stop":
		case "timer":
			if step.Seconds < 1 || step.Seconds > 86400 || len(step.Value) > 120 {
				return errors.New("请检查倒计时秒数和名称")
			}
		case "speak":
			if len(strings.TrimSpace(step.Value)) == 0 || len(step.Value) > 1000 {
				return errors.New("请填写播报内容，最多 1000 字节")
			}
		case "flow":
			ability, ok := s.Abilities[step.Value]
			if !ok || !ability.Enabled {
				return errors.New("请选择已启用的流程")
			}
			if _, err := configValues(ability, nil); err != nil {
				return err
			}
		default:
			return errors.New("未知自动化动作")
		}
	}
	return nil
}

func (a *api) automationRoutes(m *http.ServeMux) {
	save := func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name      string           `json:"name"`
			LocalTime string           `json:"local_time"`
			Repeat    string           `json:"repeat"`
			Condition string           `json:"condition"`
			Actions   []automationStep `json:"actions"`
		}
		if err := decode(w, r, &in); err != nil {
			bad(w, err)
			return
		}
		id := r.PathValue("id")
		rule := automationRule{ID: newID(), Name: strings.TrimSpace(in.Name), Schedule: Schedule{Repeat: in.Repeat, Enabled: true}, Condition: in.Condition, Actions: in.Actions}
		err := a.platform.change(func(s *State) error {
			if err := validateAutomation(s, rule); err != nil {
				return err
			}
			if err := setRuleTime(&rule.Schedule, in.LocalTime, s.Settings.Timezone, time.Now()); err != nil {
				return err
			}
			if id != "" {
				for i, previous := range s.Rules {
					if previous.ID != id {
						continue
					}
					if len(previous.Runs) > 0 && previous.Runs[0].Status == "running" {
						return errors.New("规则正在运行")
					}
					rule.ID, rule.Runs = id, previous.Runs
					rule.Schedule.Enabled = previous.Schedule.Enabled
					s.Rules[i] = rule
					return nil
				}
				return errors.New("规则不存在")
			}
			if len(s.Rules) >= 32 {
				return errors.New("最多可保存 32 条规则")
			}
			s.Rules = append(s.Rules, rule)
			return nil
		})
		clockResponse(w, err)
	}
	m.HandleFunc("POST /api/automation", save)
	m.HandleFunc("PUT /api/automation/{id}", save)
	m.HandleFunc("POST /api/automation/{id}/{action}", func(w http.ResponseWriter, r *http.Request) {
		id, action := r.PathValue("id"), r.PathValue("action")
		now := time.Now().UTC()
		if action == "run" {
			clockResponse(w, a.platform.executeAutomation(now, a.room, id, "manual"))
			return
		}
		err := a.platform.change(func(s *State) error {
			i := slices.IndexFunc(s.Rules, func(rule automationRule) bool { return rule.ID == id })
			if i < 0 {
				return errors.New("规则不存在")
			}
			rule := &s.Rules[i]
			if len(rule.Runs) > 0 && rule.Runs[0].Status == "running" {
				return errors.New("规则正在运行")
			}
			switch action {
			case "remove":
				s.Rules = slices.Delete(s.Rules, i, i+1)
			case "disable":
				rule.Schedule.Enabled = false
			case "enable":
				if !rule.Schedule.NextAt.After(now) {
					if rule.Schedule.Repeat == "once" {
						return errors.New("一次性规则已过期，请新建规则")
					}
					next, err := nextCalendarRun(rule.Schedule, now)
					if err != nil {
						return err
					}
					rule.Schedule.NextAt = next
				}
				rule.Schedule.Enabled = true
			default:
				return errors.New("未知规则操作")
			}
			return nil
		})
		clockResponse(w, err)
	})
}

func (p *Platform) recoverAutomation() error {
	for _, rule := range p.snapshot().Rules {
		if len(rule.Runs) > 0 && rule.Runs[0].Status == "running" {
			return p.change(func(s *State) error {
				for i := range s.Rules {
					if runs := s.Rules[i].Runs; len(runs) > 0 && runs[0].Status == "running" {
						runs[0].Status = "interrupted"
					}
				}
				return nil
			})
		}
	}
	return nil
}

func (p *Platform) runAutomation(now time.Time, room *roomAudio) error {
	p.mu.Lock()
	var ids []string
	for _, rule := range p.state.Rules {
		if scheduleDue(rule.Schedule, now) {
			ids = append(ids, rule.ID)
		}
	}
	p.mu.Unlock()
	for _, id := range ids {
		if err := p.executeAutomation(now, room, id, "scheduled"); err != nil {
			return err
		}
	}
	return nil
}

func (p *Platform) executeAutomation(now time.Time, room *roomAudio, id, source string) error {
	run := automationRun{ID: newID(), At: now, Source: source, Status: "running"}
	var rule automationRule
	err := p.change(func(s *State) error {
		for i := range s.Rules {
			r := &s.Rules[i]
			if r.ID != id {
				continue
			}
			if len(r.Runs) > 0 && r.Runs[0].Status == "running" {
				if source == "scheduled" {
					return errNoVoiceChange
				}
				return errors.New("规则正在运行")
			}
			if source == "scheduled" {
				if !scheduleDue(r.Schedule, now) {
					return errNoVoiceChange
				}
				missed := now.Sub(r.Schedule.NextAt) > deliveryMaxAge
				if r.Schedule.Repeat == "once" {
					r.Schedule.Enabled = false
				} else {
					next, err := nextCalendarRun(r.Schedule, now)
					if err != nil {
						return err
					}
					r.Schedule.NextAt = next
				}
				if missed {
					run.Status = "skipped"
					run.Error = "已错过计划时间"
				}
			}
			r.Runs = append([]automationRun{run}, r.Runs[:min(12, len(r.Runs))]...)
			rule = *r
			return nil
		}
		if source == "scheduled" {
			return errNoVoiceChange
		}
		return errors.New("规则不存在")
	})
	if errors.Is(err, errNoVoiceChange) {
		return nil
	}
	if err != nil || run.Status == "skipped" {
		return err
	}
	var roomRunning bool
	if room != nil {
		room.mu.Lock()
		roomRunning = room.status.Running
		room.mu.Unlock()
	}
	switch {
	case (rule.Condition == "room_on" && !roomRunning) || (rule.Condition == "room_off" && roomRunning):
		run.Status, run.Error = "skipped", "监听状态不满足条件"
	default:
		run.Status = "completed"
		start := time.Now()
		for i, step := range rule.Actions {
			if err := p.runAutomationStep(room, step, now.Add(time.Since(start))); err != nil {
				run.Step, run.Status, run.Error = i+1, "failed", err.Error()
				break
			}
		}
	}
	return p.change(func(s *State) error {
		for i := range s.Rules {
			r := &s.Rules[i]
			if r.ID == id && len(r.Runs) > 0 && r.Runs[0].ID == run.ID {
				r.Runs[0] = run
				return nil
			}
		}
		return errors.New("规则运行记录不存在")
	})
}

func (p *Platform) runAutomationStep(room *roomAudio, action automationStep, now time.Time) error {
	switch action.Action {
	case "room_start":
		_, err := room.start(p)
		return err
	case "room_stop":
		if room.stop(context.Background()).Running {
			return errors.New("在限定时间内未能停止听音")
		}
		return nil
	case "timer":
		name := "计时结束。"
		if action.Value != "" {
			name = action.Value + "，计时结束。"
		}
		return p.scheduleVoiceTask(quickTimer.ID, name, time.Duration(action.Seconds)*time.Second, now)
	case "speak":
		if !p.queueSpeech {
			return errors.New("音箱播报未启用")
		}
		return p.change(func(s *State) error {
			emit(s, &Task{}, "speech", action.Value, now)
			if s.Events[len(s.Events)-1].Delivery == "queue_full" {
				return errors.New("播报队列已满")
			}
			return nil
		})
	case "flow":
		return p.change(func(s *State) error { _, err := startTask(s, action.Value, nil, now); return err })
	default:
		return errors.New("未知自动化动作")
	}
}
