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

func (p *Platform) voiceReply(ctx context.Context, s State, message string, options replyOptions) (string, error) {
	var defs []voiceToolDefinition
	var searchKey string
	if s.Search.Enabled {
		searchKey, _ = p.searchKey(s, s.Search, "")
		if searchKey != "" {
			defs = append(defs, voiceToolDefinition{"web_search", "查询需要实时或可核实来源的信息，例如新闻、新品、政策和近期公告", []networkParameter{{"query", "string"}}},
				voiceToolDefinition{"web_fetch", "读取用户指定的公开网页地址", []networkParameter{{"url", "string"}}})
		}
	}
	for _, t := range s.HTTPTools {
		if t.Voice && t.Method == http.MethodGet {
			defs = append(defs, voiceToolDefinition{t.ID, t.Name + "。" + t.Description, t.Parameters})
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
		*target = voicePlan{Action: "speak", Language: "en", Speech: "Let’s stay with the exercise. What would you say next?"}
		return target.Speech, nil
	}
	const unavailable = "这次无法读取所需的实时信息。"
	*options.voicePlan = voicePlan{Action: "speak", Language: "zh", Speech: unavailable}
	var result string
	switch plan.Tool {
	case "web_search", "web_fetch":
		if searchKey == "" {
			return unavailable, nil
		}
		parameter := "query"
		if plan.Tool == "web_fetch" {
			parameter = "url"
		}
		value, ok := plan.Arguments[parameter]
		if !ok || len(plan.Arguments) != 1 {
			return unavailable, nil
		}
		result, err = p.webTool(ctx, s, strings.TrimPrefix(plan.Tool, "web_"), value, searchKey, false)
	default:
		tool, ok := findNetworkTool(&s, plan.Tool)
		if !ok || !tool.Voice || tool.Method != http.MethodGet {
			return unavailable, nil
		}
		result, err = p.runNetworkTool(ctx, s, tool, plan.Arguments)
	}
	if err != nil || result == "" {
		return unavailable, nil
	}
	if plan.Tool == "web_search" && result == "[]" {
		options.voicePlan.Speech = "这次搜索没有找到可用结果。"
		return options.voicePlan.Speech, nil
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
