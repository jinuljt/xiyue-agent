package main

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"
)

func featureEnabled(s *State, name string) bool {
	value, configured := s.Features[name]
	return !configured || value
}

func clockDue(s State, sc Schedule, now time.Time) bool {
	if sc.AlarmName != "" {
		return featureEnabled(&s, "alarms") && (scheduleDue(sc, now) || (sc.SnoozeAt != nil && !now.Before(*sc.SnoozeAt)))
	}
	return (sc.AbilityID != quickTimer.ID || featureEnabled(&s, "timers")) && scheduleDue(sc, now)
}

func tickClock(s *State, sc *Schedule, now time.Time) {
	if !clockDue(*s, *sc, now) {
		return
	}
	sound := false
	if sc.SnoozeAt != nil && !now.Before(*sc.SnoozeAt) {
		sound = now.Sub(*sc.SnoozeAt) <= deliveryMaxAge
		sc.SnoozeAt = nil
	}
	if scheduleDue(*sc, now) {
		sound = sound || now.Sub(sc.NextAt) <= deliveryMaxAge
		if sc.Repeat == "once" {
			sc.Enabled = false
		} else {
			next, err := nextCalendarRun(*sc, now)
			if err != nil {
				sc.Enabled, sc.Failure = false, err.Error()
			} else {
				sc.NextAt = next
			}
		}
	}
	if !sound {
		return
	}
	emit(s, &Task{}, "speech", sc.AlarmName+"，时间到了。", now)
	event := &s.Events[len(s.Events)-1]
	event.Alarm, event.ClockID = true, sc.ID
}

func (a *api) clockRoutes(m *http.ServeMux) {
	m.HandleFunc("PUT /api/features/{feature}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("feature")
		if id != "timers" && id != "alarms" {
			http.NotFound(w, r)
			return
		}
		var in struct {
			Enabled *bool `json:"enabled"`
		}
		if err := decode(w, r, &in); err != nil || in.Enabled == nil {
			bad(w, errors.New("请选择启用或停用"))
			return
		}
		v := a.voice
		if v != nil {
			v.mu.Lock()
			defer v.mu.Unlock()
		}
		stopRinging := false
		err := a.platform.change(func(s *State) error {
			if s.Features == nil {
				s.Features = map[string]bool{}
			}
			if *in.Enabled && !featureEnabled(s, id) {
				now := time.Now()
				for i := range s.Schedules {
					sc := &s.Schedules[i]
					matches := (id == "alarms" && sc.AlarmName != "") || (id == "timers" && sc.AbilityID == quickTimer.ID)
					if !matches {
						continue
					}
					sc.SnoozeAt = nil
					if !sc.Enabled || sc.NextAt.After(now) {
						continue
					}
					if sc.Repeat == "once" {
						sc.Enabled = false
						continue
					}
					var err error
					sc.NextAt, err = nextCalendarRun(*sc, now)
					if err != nil {
						return err
					}
				}
			}
			s.Features[id] = *in.Enabled
			if !*in.Enabled {
				for _, sc := range s.Schedules {
					if (id == "alarms" && sc.AlarmName != "") || (id == "timers" && sc.AbilityID == quickTimer.ID) {
						skipClockEvents(s, sc.ID)
						stopRinging = stopRinging || (v != nil && v.alarmClock == sc.ID)
					}
				}
			}
			return nil
		})
		if err == nil && stopRinging && v.alarmCancel != nil {
			v.alarmCancel()
		}
		clockResponse(w, err)
	})
	m.HandleFunc("POST /api/clocks/timer", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Seconds int    `json:"seconds"`
			Name    string `json:"name"`
		}
		if err := decode(w, r, &in); err != nil {
			bad(w, err)
			return
		}
		in.Name = strings.TrimSpace(in.Name)
		if in.Seconds < 1 || in.Seconds > 86400 || len(in.Name) > 120 {
			bad(w, errors.New("倒计时须为 1 至 86400 秒，名称过长时请缩短"))
			return
		}
		text := "计时结束。"
		if in.Name != "" {
			text = in.Name + "，计时结束。"
		}
		clockResponse(w, a.platform.scheduleVoiceTask(quickTimer.ID, text, time.Duration(in.Seconds)*time.Second, time.Now().UTC()))
	})
	saveAlarm := func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name      string `json:"name"`
			LocalTime string `json:"local_time"`
			Repeat    string `json:"repeat"`
		}
		if err := decode(w, r, &in); err != nil {
			bad(w, err)
			return
		}
		in.Name = strings.TrimSpace(in.Name)
		if len(in.Name) == 0 || len(in.Name) > 120 {
			bad(w, errors.New("请填写闹钟名称，最多 120 字节"))
			return
		}
		err := a.platform.change(func(s *State) error {
			if !featureEnabled(s, "alarms") {
				return errors.New("闹钟未启用")
			}
			sc := Schedule{ID: newID(), AlarmName: in.Name, Repeat: in.Repeat, Enabled: true}
			if err := setRuleTime(&sc, in.LocalTime, s.Settings.Timezone, time.Now()); err != nil {
				return err
			}
			if id := r.PathValue("id"); id != "" {
				for i, existing := range s.Schedules {
					if existing.ID != id || existing.AlarmName == "" {
						continue
					}
					for _, event := range s.Events {
						if event.ClockID == id && event.Delivery == "playing" {
							return errors.New("请先停止响铃，再编辑闹钟")
						}
					}
					skipClockEvents(s, id)
					sc.ID, sc.Enabled = id, existing.Enabled
					s.Schedules[i] = sc
					return nil
				}
				return errors.New("闹钟不存在")
			}
			if len(s.Schedules) >= 100 {
				return errors.New("计划已满")
			}
			s.Schedules = append(s.Schedules, sc)
			return nil
		})
		clockResponse(w, err)
	}
	m.HandleFunc("POST /api/clocks/alarm", saveAlarm)
	m.HandleFunc("PUT /api/clocks/alarm/{id}", saveAlarm)
	m.HandleFunc("POST /api/clocks/{id}/{action}", func(w http.ResponseWriter, r *http.Request) {
		clockResponse(w, a.clockAction(r.PathValue("id"), r.PathValue("action"), time.Now().UTC()))
	})
}

func clockResponse(w http.ResponseWriter, err error) {
	if err != nil {
		bad(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *api) clockAction(id, action string, now time.Time) error {
	v := a.voice
	if v != nil {
		v.mu.Lock()
		defer v.mu.Unlock()
	}
	ringing := v != nil && v.alarmClock == id && v.alarmCancel != nil
	err := a.platform.change(func(s *State) error {
		index := slices.IndexFunc(s.Schedules, func(sc Schedule) bool { return sc.ID == id && (sc.AlarmName != "" || sc.AbilityID == quickTimer.ID) })
		if index < 0 {
			return errors.New("找不到这个计时或闹钟")
		}
		sc := &s.Schedules[index]
		switch action {
		case "snooze":
			if !ringing || sc.AlarmName == "" || !featureEnabled(s, "alarms") {
				return errors.New("该闹钟当前无法稍后提醒")
			}
			if sc.SnoozeAt != nil {
				return errors.New("已设置稍后提醒")
			}
			due := now.Add(5 * time.Minute)
			sc.SnoozeAt = &due
		case "dismiss":
			if !ringing {
				return errors.New("当前没有响铃")
			}
		case "enable", "disable":
			if sc.AlarmName == "" {
				return errors.New("此操作仅适用于闹钟")
			}
			if action == "enable" && !sc.NextAt.After(now) {
				if sc.Repeat == "once" {
					return errors.New("一次性闹钟已到期，请新建闹钟")
				}
				next, err := nextCalendarRun(*sc, now)
				if err != nil {
					return err
				}
				sc.NextAt = next
			}
			sc.Enabled = action == "enable"
			sc.SnoozeAt = nil
			if action == "disable" {
				skipClockEvents(s, id)
			}
		case "remove":
			skipClockEvents(s, id)
			s.Schedules = slices.Delete(s.Schedules, index, index+1)
		default:
			return errors.New("未知闹钟操作")
		}
		return nil
	})
	if err == nil && ringing && action != "enable" {
		v.alarmCancel()
	}
	return err
}

func skipClockEvents(s *State, id string) {
	for i := range s.Events {
		if e := &s.Events[i]; e.ClockID == id && e.Delivery == "pending" {
			e.Delivery = "skipped"
		}
	}
}
