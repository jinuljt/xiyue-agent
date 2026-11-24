package main

import (
	"context"
	"errors"
	"strconv"
	"time"
)

type automationParameter struct {
	Name  string `json:"name"`
	Label string `json:"label,omitempty"`
	Type  string `json:"type"`
	Max   int    `json:"max,omitempty"`
}

type automationToolSpec struct {
	ID         string                `json:"id"`
	Name       string                `json:"name"`
	Parameters []automationParameter `json:"parameters,omitempty"`
	Result     bool                  `json:"result,omitempty"`
	Hint       string                `json:"hint,omitempty"`
	Note       string                `json:"note,omitempty"`

	available func(*State) error
	run       func(*Platform, map[string]string, time.Time) error
}

const timerToolID = "timer.start"

func timerAutomationTool() automationToolSpec {
	return automationToolSpec{
		ID: timerToolID, Name: "启动响铃倒计时",
		Parameters: []automationParameter{{Name: "seconds", Label: "倒计时时长", Type: "duration", Max: 86400}, {Name: "name", Label: "名称（可选）", Type: "optional_text", Max: 120}},
		available: func(s *State) error {
			if !featureEnabled(s, "timers") {
				return errors.New("请在能力中心启用倒计时")
			}
			return nil
		},
		run: func(p *Platform, args map[string]string, now time.Time) error {
			seconds, _ := strconv.Atoi(args["seconds"])
			name := "计时结束。"
			if args["name"] != "" {
				name = args["name"] + "，计时结束。"
			}
			return p.scheduleVoiceTask(quickTimer.ID, name, time.Duration(seconds)*time.Second, now)
		},
	}
}

func localAutomationTools(room *roomAudio) []automationToolSpec {
	return append(roomAutomationTools(room), timerAutomationTool())
}

func lookupLocalAutomationTool(id string, room *roomAudio) (automationToolSpec, bool) {
	for _, tool := range localAutomationTools(room) {
		if tool.ID == id {
			return tool, true
		}
	}
	return automationToolSpec{}, false
}

func (tool automationToolSpec) validate(args map[string]string, templates bool) error {
	known := 0
	for _, param := range tool.Parameters {
		value, present := args[param.Name]
		if present {
			known++
		}
		switch param.Type {
		case "duration":
			if templates && automationReference.MatchString(value) {
				continue
			}
			number, err := strconv.Atoi(value)
			if err != nil || number < 1 || number > param.Max {
				return errors.New("请检查" + param.Label)
			}
		case "optional_text":
			if len(value) > param.Max {
				return errors.New("名称过长")
			}
		}
	}
	if known != len(args) {
		return errors.New("包含未知工具参数")
	}
	return nil
}

type automationStateSpec struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	Options map[string]string `json:"options,omitempty"`

	// Identity invalidates observations when the underlying source is replaced.
	identity [3]string
	group    string
	key      string
	read     func(context.Context, *Platform, State) map[string]string
}

func (spec automationStateSpec) accepts(value string) bool {
	if spec.Options != nil {
		_, ok := spec.Options[value]
		return ok
	}
	return value != "" && len(value) <= 80 && value != "unknown" && value != "unavailable"
}

func automationStateSources(s *State, room *roomAudio) map[string]automationStateSpec {
	local := roomAutomationState(room)
	sources := map[string]automationStateSpec{local.ID: local}
	for _, tool := range s.HTTPTools {
		if spec, ok := tool.automationState(s); ok {
			sources[spec.ID] = spec
		}
	}
	return sources
}

type automationObservation struct {
	identity [3]string
	value    string
}

func (p *Platform) observeAutomationStates(ctx context.Context, s State, room *roomAudio, ids []string) map[string]automationObservation {
	sources := automationStateSources(&s, room)
	observed := make(map[string]automationObservation, len(ids))
	batches := map[string]map[string]string{}
	for _, id := range ids {
		spec, ok := sources[id]
		if !ok {
			continue
		}
		group, loaded := batches[spec.group]
		if !loaded {
			group = spec.read(ctx, p, s)
			batches[spec.group] = group
		}
		if value := group[spec.key]; spec.accepts(value) {
			observed[id] = automationObservation{identity: spec.identity, value: value}
		}
	}
	return observed
}

func (a *api) automationOptions(s State) map[string]any {
	tools := localAutomationTools(a.room)
	for i := range tools {
		if err := tools[i].available(&s); err != nil {
			tools[i].Note = err.Error()
		}
	}
	for _, network := range s.HTTPTools {
		if network.Automation {
			spec := automationToolSpec{ID: network.ID, Name: network.Name, Result: true}
			for _, param := range network.Parameters {
				spec.Parameters = append(spec.Parameters, automationParameter{Name: param.Name, Type: param.Type})
			}
			tools = append(tools, spec)
		}
	}
	states := automationStateSources(&s, a.room)
	return map[string]any{"tools": tools, "states": states}
}
