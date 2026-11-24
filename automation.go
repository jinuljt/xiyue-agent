package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"slices"
	"strings"
	"time"
)

type automationStep struct {
	Action     string            `json:"action"`
	Value      string            `json:"value,omitempty"`
	Seconds    int               `json:"seconds,omitempty"`
	Parameters map[string]string `json:"parameters,omitempty"`
	Next       int               `json:"next,omitempty"`
	OnTimeout  string            `json:"on_timeout,omitempty"`
	SaveAs     string            `json:"save_as,omitempty"`
	Equals     json.RawMessage   `json:"equals,omitempty"`
}

type automationRun struct {
	ID      string     `json:"id"`
	At      time.Time  `json:"at"`
	Source  string     `json:"source"`
	Status  string     `json:"status"`
	Step    int        `json:"step,omitempty"`
	Error   string     `json:"error,omitempty"`
	Phase   string     `json:"phase,omitempty"`
	Until   *time.Time `json:"until,omitempty"`
	EventID string     `json:"event_id,omitempty"`
	ChildID string     `json:"child_task_id,omitempty"`
	Results []string   `json:"results,omitempty"`
	Actions []string   `json:"actions,omitempty"`
}

type automationRule struct {
	ID        string           `json:"id"`
	Name      string           `json:"name"`
	Schedule  Schedule         `json:"schedule"`
	Condition string           `json:"condition,omitempty"`
	Trigger   string           `json:"trigger,omitempty"`
	Entity    string           `json:"entity_tool,omitempty"`
	ToState   string           `json:"to_state,omitempty"`
	After     string           `json:"after,omitempty"`
	Before    string           `json:"before,omitempty"`
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
	if len(rule.Name) == 0 || len(rule.Name) > 120 || len(rule.Actions) == 0 || len(rule.Actions) > 64 {
		return errors.New("请填写规则名称并添加 1 至 64 个动作")
	}
	if rule.Condition != "" && rule.Condition != "room_on" && rule.Condition != "room_off" {
		return errors.New("未知执行条件")
	}
	if (rule.After == "") != (rule.Before == "") {
		return errors.New("请选择时间条件的开始和结束时间")
	}
	if rule.After != "" {
		_, begin := time.Parse("15:04", rule.After)
		_, end := time.Parse("15:04", rule.Before)
		if begin != nil || end != nil || rule.After == rule.Before {
			return errors.New("时间条件无效")
		}
	}
	if rule.Trigger == "entity" {
		t, ok := findNetworkTool(s, rule.Entity)
		if !ok || !t.Automation || t.Kind != "ha" || t.Operation != "state" || rule.ToState == "" || len(rule.ToState) > 80 ||
			rule.ToState == "unavailable" || rule.ToState == "unknown" {
			return errors.New("请选择已授权的 Home Assistant 读取状态工具，并填写要触发的状态")
		}
	}
	variables := map[string]bool{}
	for i, step := range rule.Actions {
		for _, text := range step.Parameters {
			if err := validateAutomationReferences(text, variables); err != nil {
				return err
			}
		}
		if err := validateAutomationReferences(step.Value, variables); err != nil {
			return err
		}
		switch step.Action {
		case "room_start", "room_stop":
		case "http_tool":
			tool, ok := findNetworkTool(s, step.Value)
			if !ok || !tool.Automation {
				return errors.New("请选择已授权给自动化的工具")
			}
			args := make(map[string]string, len(step.Parameters))
			for key, value := range step.Parameters {
				args[key] = value
			}
			for _, param := range tool.Parameters {
				if automationReference.MatchString(args[param.Name]) {
					switch param.Type {
					case "number":
						args[param.Name] = "0"
					case "boolean":
						args[param.Name] = "false"
					}
				}
			}
			if _, err := tool.values(args); err != nil {
				return err
			}
			if step.SaveAs != "" {
				if !parameterID.MatchString(step.SaveAs) || variables[step.SaveAs] {
					return errors.New("结果变量名须是唯一字母、数字或下划线标识符，并以字母或下划线开头")
				}
				variables[step.SaveAs] = true
			}
		case "if":
			match := automationReference.FindStringSubmatch("{{" + step.Value + "}}")
			if len(match) != 2 || match[1] != step.Value {
				return errors.New("请输入结果变量或字段路径")
			}
			if err := validateAutomationReferences(match[0], variables); err != nil {
				return err
			}
			if len(step.Equals) > 1000 {
				return errors.New("用于比较的值过长")
			}
			if _, err := automationScalar(step.Equals); err != nil {
				return err
			}
			if step.Next != 0 && (step.Next <= i+1 || step.Next > len(rule.Actions)) {
				return errors.New("条件不成立时只能跳到后续步骤")
			}
		case "tone":
			if _, ok := deviceTones[step.Value]; !ok {
				return errors.New("请选择有效的提示音")
			}
		case "wait":
			if step.Seconds < 1 || step.Seconds > 604800 {
				return errors.New("等待时间须为 1 至 604800 秒")
			}
		case "confirm":
			if step.Seconds < 0 || step.Seconds > 86400 || (step.OnTimeout != "stop" && step.OnTimeout != "continue") ||
				strings.TrimSpace(step.Value) == "" || len(step.Value) > 1000 {
				return errors.New("请填写确认提示、等待时间和超时处理")
			}
			if step.Next != 0 && (step.Next <= i+1 || step.Next > len(rule.Actions)) {
				return errors.New("确认后只能跳到后续步骤")
			}
		case "timer":
			if step.Seconds < 1 || step.Seconds > 86400 || len(step.Value) > 120 {
				return errors.New("请检查倒计时秒数和名称")
			}
		case "speak":
			if len(strings.TrimSpace(step.Value)) == 0 || len(step.Value) > 4000 {
				return errors.New("请填写播报内容，最多 4000 字节")
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
	m.HandleFunc("POST /api/automation/convert-legacy", func(w http.ResponseWriter, r *http.Request) {
		var manifest Manifest
		if err := decode(w, r, &manifest); err != nil {
			bad(w, err)
			return
		}
		if err := validateManifest(manifest); err != nil {
			bad(w, err)
			return
		}
		values := map[string]string{}
		for _, field := range manifest.Fields {
			values[field.Key] = field.Default
			if field.Required && field.Default == "" {
				values[field.Key] = "〔填写" + field.Label + "〕"
			}
		}
		steps, err := convertLegacyFlow(manifest, values)
		if err != nil {
			bad(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"name": manifest.Name, "trigger": "manual", "actions": steps})
	})
	save := func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name      string           `json:"name"`
			LocalTime string           `json:"local_time"`
			Repeat    string           `json:"repeat"`
			Condition string           `json:"condition"`
			Actions   []automationStep `json:"actions"`
			Trigger   string           `json:"trigger"`
			Entity    string           `json:"entity_tool"`
			ToState   string           `json:"to_state"`
			After     string           `json:"after"`
			Before    string           `json:"before"`
		}
		if err := decode(w, r, &in); err != nil {
			bad(w, err)
			return
		}
		id := r.PathValue("id")
		rule := automationRule{ID: newID(), Name: strings.TrimSpace(in.Name), Schedule: Schedule{Repeat: in.Repeat, Enabled: true}, Condition: in.Condition, Actions: in.Actions,
			Trigger: in.Trigger, Entity: in.Entity, ToState: strings.TrimSpace(in.ToState), After: in.After, Before: in.Before}
		err := a.platform.change(func(s *State) error {
			if err := validateAutomation(s, rule); err != nil {
				return err
			}
			rule.Schedule.Timezone = s.Settings.Timezone
			switch rule.Trigger {
			case "", "schedule":
				if err := setRuleTime(&rule.Schedule, in.LocalTime, s.Settings.Timezone, time.Now()); err != nil {
					return err
				}
			case "manual", "entity":
				rule.Schedule.Repeat = ""
			default:
				return errors.New("请选择触发方式")
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
			clockResponse(w, a.platform.startAutomation(now, a.room, id, "manual"))
			return
		}
		if action == "confirm" {
			var in struct {
				Run  string `json:"run_id"`
				Step int    `json:"step"`
			}
			if err := decode(w, r, &in); err != nil {
				bad(w, err)
				return
			}
			clockResponse(w, a.platform.confirmAutomation(id, in.Run, in.Step, now))
			return
		}
		if action == "cancel" {
			var in struct {
				Run string `json:"run_id"`
			}
			if err := decode(w, r, &in); err != nil {
				bad(w, err)
				return
			}
			clockResponse(w, a.platform.changeAutomationRun(id, in.Run, func(s *State, run *automationRun) error {
				if run.Phase == "child" {
					if err := taskAction(s, run.ChildID, "cancel", now); err != nil {
						return err
					}
				} else if run.Phase != "wait" && run.Phase != "confirm" {
					return errors.New("当前步骤正在执行，请等候结果")
				}
				run.Status = "cancelled"
				return nil
			}))
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
				if isScheduledAutomation(*rule) && !rule.Schedule.NextAt.After(now) {
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
						run := &runs[0]
						run.Status, run.Error = "interrupted", "音箱重启，运行已中断"
						for i := range s.Events {
							if e := &s.Events[i]; e.ID == run.EventID && e.Delivery == "pending" {
								e.Delivery = "skipped"
							}
						}
					}
				}
				return nil
			})
		}
	}
	return nil
}

func isScheduledAutomation(rule automationRule) bool {
	return rule.Trigger == "" || rule.Trigger == "schedule"
}

func (p *Platform) runAutomation(now time.Time, room *roomAudio) error {
	p.mu.Lock()
	rules := p.state.Rules
	p.mu.Unlock()
	for _, rule := range rules {
		due := isScheduledAutomation(rule) && scheduleDue(rule.Schedule, now)
		running := len(rule.Runs) > 0 && rule.Runs[0].Status == "running"
		if due && !running {
			if err := p.startAutomation(now, room, rule.ID, "scheduled"); err != nil {
				return err
			}
		}
		if running || due {
			if err := p.advanceAutomation(now, room, rule.ID); err != nil {
				return err
			}
		}
	}
	if len(p.automationValues) > 0 {
		p.mu.Lock()
		active := map[string]bool{}
		for _, rule := range p.state.Rules {
			if len(rule.Runs) > 0 && rule.Runs[0].Status == "running" {
				active[rule.Runs[0].ID] = true
			}
		}
		p.mu.Unlock()
		for id := range p.automationValues {
			if !active[id] {
				delete(p.automationValues, id)
			}
		}
	}
	return nil
}

// Poll all enabled rules together. The first valid observation after startup
// or a disconnected source establishes a baseline.
type automationEntityBaseline struct {
	endpoint, credential string
	previous             map[string]string
}

func (p *Platform) watchAutomationEntities(ctx context.Context, room *roomAudio) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	baseline := automationEntityBaseline{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := p.pollAutomationEntities(ctx, room, &baseline); err != nil {
				log.Print("家庭设备自动化触发状态保存失败")
			}
		}
	}
}

func (p *Platform) pollAutomationEntities(ctx context.Context, room *roomAudio, baseline *automationEntityBaseline) error {
	p.mu.Lock()
	s := p.state
	p.mu.Unlock()
	credential := s.Sealed[haSecret]
	if s.HomeAssistant != baseline.endpoint || credential != baseline.credential {
		*baseline = automationEntityBaseline{endpoint: s.HomeAssistant, credential: credential}
	}
	rules := map[string]networkTool{}
	for _, rule := range s.Rules {
		if rule.Trigger == "entity" && rule.Schedule.Enabled {
			if tool, ok := findNetworkTool(&s, rule.Entity); ok && tool.Automation && tool.Kind == "ha" && tool.Operation == "state" {
				rules[rule.ID] = tool
			}
		}
	}
	if len(rules) == 0 {
		baseline.previous = nil
		return nil
	}
	entities, err := p.haEntities(ctx, s)
	if err != nil {
		baseline.previous = nil
		return nil
	}
	states := make(map[string]string, len(entities))
	for _, entity := range entities {
		if entity.State != "unknown" && entity.State != "unavailable" && entity.State != "" {
			states[entity.ID] = entity.State
		}
	}
	now := time.Now().UTC()
	current := map[string]string{}
	var failure error
	for _, rule := range s.Rules {
		tool, ok := rules[rule.ID]
		if !ok {
			continue
		}
		key := rule.ID + "|" + tool.Entity + "|" + rule.ToState
		value := states[tool.Entity]
		if value != "" {
			current[key] = value
		}
		if old, seen := baseline.previous[key]; seen && old != value && value == rule.ToState {
			if err := p.startAutomationIf(now, room, rule.ID, "entity", func(next *State, latest automationRule) bool {
				authorized, ok := findNetworkTool(next, latest.Entity)
				return ok && authorized.Automation && authorized.Kind == "ha" && authorized.Operation == "state" &&
					authorized.Entity == tool.Entity && latest.ToState == rule.ToState && next.HomeAssistant == s.HomeAssistant &&
					next.Sealed[haSecret] == credential
			}); err != nil {
				current[key] = old
				failure = err
			}
		}
	}
	baseline.previous = current
	return failure
}

func (p *Platform) startAutomation(now time.Time, room *roomAudio, id, source string) error {
	return p.startAutomationIf(now, room, id, source, nil)
}

func (p *Platform) startAutomationIf(now time.Time, room *roomAudio, id, source string, matches func(*State, automationRule) bool) error {
	run := automationRun{ID: newID(), At: now, Source: source, Status: "running", Step: 1}
	roomOn := false
	if room != nil {
		room.mu.Lock()
		roomOn = room.status.Running
		room.mu.Unlock()
	}
	err := p.change(func(s *State) error {
		for i := range s.Rules {
			rule := &s.Rules[i]
			if rule.ID != id {
				continue
			}
			if len(rule.Runs) > 0 && rule.Runs[0].Status == "running" {
				if source != "manual" {
					return errNoVoiceChange
				}
				return errors.New("流程正在运行")
			}
			if source == "manual" && !rule.Schedule.Enabled && !isScheduledAutomation(*rule) {
				return errors.New("请先启用此流程")
			}
			if source == "entity" && (rule.Trigger != "entity" || !rule.Schedule.Enabled || matches == nil || !matches(s, *rule)) {
				return errNoVoiceChange
			}
			if source == "scheduled" {
				if !isScheduledAutomation(*rule) || !scheduleDue(rule.Schedule, now) {
					return errNoVoiceChange
				}
				if now.Sub(rule.Schedule.NextAt) > deliveryMaxAge {
					run.Status, run.Error = "skipped", "已错过计划时间"
				}
				if rule.Schedule.Repeat == "once" {
					rule.Schedule.Enabled = false
				} else {
					next, err := nextCalendarRun(rule.Schedule, now)
					if err != nil {
						return err
					}
					rule.Schedule.NextAt = next
				}
			}
			if run.Status == "running" && !automationConditions(*rule, s.Settings.Timezone, now, roomOn) {
				run.Status, run.Error = "skipped", "当前不满足执行条件"
			}
			if run.Status == "skipped" {
				run.Step = 0
			}
			for _, step := range rule.Actions {
				run.Actions = append(run.Actions, step.Action)
			}
			rule.Runs = append([]automationRun{run}, rule.Runs[:min(12, len(rule.Runs))]...)
			return nil
		}
		if source != "manual" {
			return errNoVoiceChange
		}
		return errors.New("流程不存在")
	})
	if errors.Is(err, errNoVoiceChange) {
		return nil
	}
	return err
}

func automationConditions(rule automationRule, tz string, now time.Time, roomOn bool) bool {
	if (rule.Condition == "room_on" && !roomOn) || (rule.Condition == "room_off" && roomOn) {
		return false
	}
	if rule.After == "" {
		return true
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return false
	}
	clock := now.In(loc).Format("15:04")
	if rule.After < rule.Before {
		return rule.After <= clock && clock < rule.Before
	}
	return clock >= rule.After || clock < rule.Before
}

func (p *Platform) changeAutomationRun(id, runID string, change func(*State, *automationRun) error) error {
	return p.change(func(s *State) error {
		for i := range s.Rules {
			rule := &s.Rules[i]
			if rule.ID == id && len(rule.Runs) > 0 && rule.Runs[0].ID == runID && rule.Runs[0].Status == "running" {
				return change(s, &rule.Runs[0])
			}
		}
		return errAutomationFinished
	})
}

var errAutomationFinished = errors.New("流程已经结束")

func finishAutomationStep(run *automationRun, result string, count int) {
	run.Results = append(run.Results, result)
	run.Phase, run.EventID, run.ChildID, run.Until = "", "", "", nil
	if run.Step == count {
		run.Status = "completed"
	} else {
		run.Step++
	}
}

func (p *Platform) advanceAutomation(now time.Time, room *roomAudio, id string) error {
	s := p.snapshot()
	for _, rule := range s.Rules {
		if rule.ID != id || len(rule.Runs) == 0 || rule.Runs[0].Status != "running" {
			continue
		}
		run := rule.Runs[0]
		step := rule.Actions[run.Step-1]
		count := len(rule.Actions)
		update := func(change func(*State, *automationRun) error) error {
			err := p.changeAutomationRun(id, run.ID, func(s *State, live *automationRun) error {
				if live.Step != run.Step || live.Phase != run.Phase {
					return errNoVoiceChange
				}
				return change(s, live)
			})
			if errors.Is(err, errNoVoiceChange) || errors.Is(err, errAutomationFinished) {
				return nil
			}
			return err
		}
		fail := func(err error) error {
			return update(func(_ *State, live *automationRun) error {
				live.Status, live.Error = "failed", err.Error()
				live.Results = append(live.Results, "执行失败")
				return nil
			})
		}
		values := p.automationValues[run.ID]

		switch run.Phase {
		case "action":
			return update(func(_ *State, live *automationRun) error {
				live.Status, live.Error = "interrupted", "上一步的执行结果未能保存，请检查设备状态"
				live.Results = append(live.Results, "结果未知")
				return nil
			})
		case "child":
			index := slices.IndexFunc(s.Tasks, func(t Task) bool { return t.ID == run.ChildID })
			if index < 0 || s.Tasks[index].Status == "cancelled" {
				return fail(errors.New("旧版子流程已取消或不存在"))
			}
			if s.Tasks[index].Status != "done" {
				return nil
			}
			return update(func(_ *State, live *automationRun) error {
				finishAutomationStep(live, "子流程已完成", count)
				return nil
			})
		case "wait", "confirm":
			if run.Until == nil || now.Before(*run.Until) {
				return nil
			}
			return update(func(_ *State, live *automationRun) error {
				if run.Phase == "confirm" && step.OnTimeout == "stop" {
					live.Status, live.Error = "failed", "等待确认超时"
					live.Results = append(live.Results, "确认超时")
				} else {
					result := "等待结束"
					if run.Phase == "confirm" {
						result = "确认超时，已继续"
					}
					finishAutomationStep(live, result, count)
				}
				return nil
			})
		case "speech":
			i := slices.IndexFunc(s.Events, func(e Event) bool { return e.ID == run.EventID })
			if i >= 0 && (s.Events[i].Delivery == "pending" || s.Events[i].Delivery == "playing") {
				return nil
			}
			return update(func(_ *State, live *automationRun) error {
				if i < 0 || s.Events[i].Delivery != "delivered" {
					live.Status, live.Error = "failed", "音箱未能播报此步骤"
					live.Results = append(live.Results, "播报失败")
				} else if step.Action == "confirm" {
					live.Phase = "confirm"
					if step.Seconds > 0 {
						until := now.Add(time.Duration(step.Seconds) * time.Second)
						live.Until = &until
					}
				} else {
					finishAutomationStep(live, "已播报", count)
				}
				return nil
			})
		}
		if step.Action == "flow" {
			return update(func(s *State, live *automationRun) error {
				child, err := startTask(s, step.Value, nil, now)
				if err != nil {
					live.Status, live.Error = "failed", err.Error()
					live.Results = append(live.Results, "执行失败")
				} else {
					live.Phase, live.ChildID = "child", child
				}
				return nil
			})
		}
		if step.Action == "if" {
			value, err := automationValue(values, step.Value)
			if err != nil {
				return fail(err)
			}
			match, err := equalAutomationValues(value, step.Equals)
			if err != nil {
				return fail(err)
			}
			return update(func(_ *State, live *automationRun) error {
				if match {
					finishAutomationStep(live, "条件成立", count)
					return nil
				}
				finishAutomationStep(live, "条件不成立", count)
				target := step.Next
				if target == 0 {
					target = count + 1
				}
				for live.Status == "running" && live.Step < target {
					finishAutomationStep(live, "已跳过", count)
				}
				return nil
			})
		}
		text, err := expandAutomationText(step.Value, values)
		if err != nil {
			return fail(err)
		}
		step.Value = text
		if step.Action == "speak" || step.Action == "confirm" || step.Action == "wait" || step.Action == "tone" {
			return update(func(s *State, live *automationRun) error {
				switch {
				case step.Action == "wait":
					until := now.Add(time.Duration(step.Seconds) * time.Second)
					live.Phase, live.Until = "wait", &until
				case !s.queueSpeech:
					live.Status, live.Error = "failed", "音箱播报未启用"
					live.Results = append(live.Results, "播报不可用")
				default:
					kind := "speech"
					if step.Action == "tone" {
						kind = "tone"
					}
					emit(s, &Task{ID: live.ID}, kind, step.Value, now)
					live.Phase, live.EventID = "speech", s.Events[len(s.Events)-1].ID
				}
				return nil
			})
		}
		// Record the intent before a side effect. If recording its result fails,
		// the next tick marks it interrupted rather than repeating the action.
		if err := update(func(_ *State, live *automationRun) error {
			live.Phase = "action"
			return nil
		}); err != nil {
			return err
		}
		run.Phase = "action"
		var result networkToolResult
		if step.Action == "http_tool" {
			args := make(map[string]string, len(step.Parameters))
			for key, value := range step.Parameters {
				args[key], err = expandAutomationText(value, values)
				if err != nil {
					return fail(err)
				}
			}
			result, err = p.automationTool(step.Value, args)
			if err == nil && step.SaveAs != "" {
				size := len(result.Value)
				for _, existing := range values {
					size += len(existing)
				}
				if size > automationValueBudget {
					err = errors.New("工具结果超过流程变量容量，请选取所需字段")
				}
			}
		} else {
			err = p.runAutomationStep(room, step, now)
		}
		updateErr := update(func(_ *State, live *automationRun) error {
			if err != nil {
				live.Status, live.Error = "failed", err.Error()
				live.Results = append(live.Results, "执行失败")
			} else {
				result := "已执行"
				if step.Action == "timer" {
					result = "已创建响铃倒计时"
				}
				if step.Action == "room_start" {
					result = "已请求连接音源"
				}
				if step.SaveAs != "" {
					result = "已保存变量 " + step.SaveAs
				}
				finishAutomationStep(live, result, count)
			}
			return nil
		})
		if updateErr == nil && err == nil && step.Action == "http_tool" && step.SaveAs != "" {
			if p.automationValues == nil {
				p.automationValues = map[string]map[string]json.RawMessage{}
			}
			if values == nil {
				values = map[string]json.RawMessage{}
				p.automationValues[run.ID] = values
			}
			values[step.SaveAs] = result.Value
		}
		return updateErr
	}
	return nil
}

func (p *Platform) confirmAutomation(id, runID string, step int, now time.Time) error {
	return p.changeAutomationRun(id, runID, func(s *State, run *automationRun) error {
		if run.Step != step || run.Phase != "confirm" || run.Until != nil && !now.Before(*run.Until) {
			return errors.New("此步骤已变更或确认超时")
		}
		for _, rule := range s.Rules {
			if rule.ID == id {
				next := rule.Actions[run.Step-1].Next
				finishAutomationStep(run, "已确认", len(rule.Actions))
				for run.Status == "running" && run.Step < next {
					finishAutomationStep(run, "已跳过", len(rule.Actions))
				}
				break
			}
		}
		return nil
	})
}

func (p *Platform) automationVoiceConfirmation(text string, now time.Time) (voiceTaskReply, error) {
	switch text {
	case "继续", "继续下一步", "确认继续":
	default:
		return voiceTaskReply{}, nil
	}
	var waiting *automationRule
	for _, rule := range p.snapshot().Rules {
		if len(rule.Runs) == 0 {
			continue
		}
		run := rule.Runs[0]
		if run.Status != "running" || run.Phase != "confirm" || run.Until != nil && !now.Before(*run.Until) {
			continue
		}
		if waiting != nil {
			return voiceTaskReply{handled: true, text: "有多个流程等待确认，请在控制台选择。"}, nil
		}
		waiting = &rule
	}
	if waiting == nil {
		return voiceTaskReply{}, nil
	}
	run := waiting.Runs[0]
	err := p.confirmAutomation(waiting.ID, run.ID, run.Step, now)
	return voiceTaskReply{handled: true, text: "继续。"}, err
}

func (p *Platform) automationTool(id string, args map[string]string) (networkToolResult, error) {
	s := p.snapshot()
	tool, ok := findNetworkTool(&s, id)
	if !ok || !tool.Automation {
		return networkToolResult{}, errors.New("该工具已停用或撤销自动化授权")
	}
	return p.networkToolValue(context.Background(), s, tool, args)
}

func (p *Platform) runAutomationStep(room *roomAudio, action automationStep, now time.Time) error {
	switch action.Action {
	case "http_tool":
		_, err := p.automationTool(action.Value, action.Parameters)
		return err
	case "room_start":
		_, err := room.start(p)
		return err
	case "room_stop":
		if status, err := room.stop(context.Background(), p); err != nil {
			return err
		} else if status.Running {
			return errors.New("在限定时间内未能停止听音")
		}
		return nil
	case "timer":
		name := "计时结束。"
		if action.Value != "" {
			name = action.Value + "，计时结束。"
		}
		return p.scheduleVoiceTask(quickTimer.ID, name, time.Duration(action.Seconds)*time.Second, now)
	default:
		return errors.New("未知自动化动作")
	}
}
