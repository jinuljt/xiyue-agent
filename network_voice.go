package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

type voiceToolDefinition struct {
	ID          string             `json:"id"`
	Description string             `json:"description"`
	Parameters  []networkParameter `json:"parameters"`
}

type voiceTool struct {
	definition    voiceToolDefinition
	emptyResponse string
	run           func(context.Context, map[string]string) (string, error)
}

func (p *Platform) voiceReply(ctx context.Context, s State, message string, options replyOptions) (string, error) {
	var defs []voiceToolDefinition
	handlers := map[string]voiceTool{}
	register := func(tool voiceTool) {
		defs = append(defs, tool.definition)
		handlers[tool.definition.ID] = tool
	}
	for _, tool := range p.searchVoiceTools(s) {
		register(tool)
	}
	for _, tool := range s.HTTPTools {
		if tool.Voice && tool.Method == http.MethodGet {
			register(voiceTool{
				definition: voiceToolDefinition{tool.ID, tool.Name + "。" + tool.Description, tool.Parameters},
				run: func(ctx context.Context, args map[string]string) (string, error) {
					return p.runNetworkTool(ctx, s, tool, args)
				},
			})
		}
	}
	c := capabilityContext{platform: p, room: p.room, model: p.deviceModel, firmware: p.deviceFirmware, beforeRoom: options.beforeRoom}
	deviceTools := map[string]capability{}
	for _, op := range deviceCapabilities {
		if op.Voice && p.room != nil {
			deviceTools[op.name()] = op
		}
	}
	if !options.practice && (len(defs) > 0 || len(deviceTools) > 0) {
		tools, _ := json.Marshal(defs)
		declarations := []map[string]any{}
		for _, op := range deviceCapabilities {
			if op.Voice && p.room != nil {
				declarations = append(declarations, map[string]any{"id": op.name(), "description": op.Description, "input_schema": op.Input})
			}
		}
		local, _ := json.Marshal(declarations)
		options.toolPrompt = "\n设备工具（参数遵循 input_schema）：" + string(local) + "\n联网工具（参数值用字符串）：" + string(tools) +
			"\n遇到设备状态、当前时刻的数据或近期事实，请调用合适工具。只需一种操作。格式为 {\"action\":\"tool\",\"tool\":\"工具 ID\",\"arguments\":{}}。如果听音因本轮对话让行，区分之前与当前的状态。"
	}
	reply, err := p.requestReply(ctx, s, message, options)
	if err != nil || options.voicePlan.Action != "tool" {
		return reply, err
	}
	target := options.voicePlan
	plan := *target
	if options.practice {
		*target = voicePlan{Action: "speak", Language: "zh", Speech: "请再说一遍。"}
		return target.Speech, nil
	}
	language := s.Settings.replyLanguage()
	unavailable := localizedVoiceText(language, "tool_unavailable")
	*options.voicePlan = voicePlan{Action: "speak", Language: language, Speech: unavailable}
	var result string
	if op, ok := deviceTools[plan.Tool]; ok {
		value, err := c.call(ctx, op.ID, plan.Arguments)
		if err != nil {
			value = capabilityFailure(err)
		}
		encoded, _ := json.Marshal(value)
		result = string(encoded)
	} else {
		execute, ok := handlers[plan.Tool]
		if !ok {
			return unavailable, nil
		}
		args := map[string]string{}
		if len(plan.Arguments) != 0 && json.Unmarshal(plan.Arguments, &args) != nil {
			return unavailable, nil
		}
		result, err = execute.run(ctx, args)
		if err != nil || result == "" {
			return unavailable, nil
		}
		if result == "[]" && execute.emptyResponse != "" {
			target.Speech = localizedVoiceText(language, "search_empty")
			return target.Speech, nil
		}
	}
	data, _ := json.Marshal(result)
	options.toolResult = string(data)
	options.question = nil
	var final voicePlan
	options.voicePlan = &final
	reply, err = p.requestReply(ctx, s, message, options)
	if err != nil {
		return unavailable, nil
	}
	if final.Action != "speak" || strings.TrimSpace(reply) == "" {
		return unavailable, nil
	}
	// Update the caller's plan only after the second response is restricted to speech.
	*target = final
	return reply, nil
}
