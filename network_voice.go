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
	if len(defs) > 0 && !options.practice {
		data, _ := json.Marshal(defs)
		options.toolPrompt = "\n可用工具如下：" + string(data) + "\n遇到依赖当下情况或近期事实的问题，主动查证；需要设备实时状态时可调用相应工具。只有问题需要工具时才返回工具动作。格式为 {\"action\":\"tool\",\"tool\":\"工具 ID\",\"arguments\":{\"参数名\":\"值\"}}，所有参数值用字符串表示，每轮只调用一个工具。"
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
	execute, ok := handlers[plan.Tool]
	if !ok {
		return unavailable, nil
	}
	result, err := execute.run(ctx, plan.Arguments)
	if err != nil || result == "" {
		return unavailable, nil
	}
	if result == "[]" && execute.emptyResponse != "" {
		target.Speech = localizedVoiceText(language, "search_empty")
		return target.Speech, nil
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
