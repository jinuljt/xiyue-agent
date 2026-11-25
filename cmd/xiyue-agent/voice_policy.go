package main

import (
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const speakerInstructions = `你是喜悦 Voice 音箱的语音助手。根据用户这一轮的完整意图，仅返回一个 JSON 对象，不要使用代码块：
{"action":"speak","speech":"可以直接播报的文字"}
按设置的对话语言生成播报，默认普通话。整个回合保持该语言。被要求更换对话语言时，用当前语言说明可在语音设置中切换。
语音先说结论，日常默认一两句，目标约 8 秒。省略寒暄、复述、套话、Markdown 和括号旁白。用户明确要求详细说明时可分短句展开。保留重要数字、单位和条件。
用户确实想结束当前对话时返回 {"action":"end"}。引用、询问退出方式、拒绝某个操作后继续提问，均继续正常对话。
用户明确要求设置倒计时时返回 {"action":"countdown","seconds":秒数}。明确要求在一段时间后提醒时返回 {"action":"reminder","seconds":秒数,"content":"要提醒的事情"}。参数缺失时只用 speak 简短追问缺的参数。用户明确确认继续当前已暂停等待确认的步骤时返回 {"action":"next_step"}。
只返回一种动作。询问、假设、多项指令或指令含糊时不要发起设备动作。请勿承诺尚未执行的操作。缺少实时信息时如实说明。`

var endDialogue = regexp.MustCompile(`^(?:(?:谢谢|好了|好|行)[，,\s]*)?(?:请|那就|我们)?(?:先)?(?:结束(?:对话|聊天)|退出(?:对话|聊天)?|不聊|不用(?:再|继续)聊|(?:今天|这次)?(?:就这样|到这(?:里|儿))|下次再聊|再见|拜拜)(?:了|吧|啦)?$`)
var hypotheticalTask = regexp.MustCompile(`^(?:请|你能)?(?:说说|解释|举例|如何|怎么|怎样|如果|假如|不要|不用)`)

type voicePlan struct {
	Action    string          `json:"action"`
	Speech    string          `json:"speech,omitempty"`
	Seconds   int             `json:"seconds,omitempty"`
	Content   string          `json:"content,omitempty"`
	Tool      string          `json:"tool,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Language  string          `json:"language,omitempty"`
}

func parseVoicePlan(reply string) (voicePlan, error) {
	raw := strings.TrimSpace(reply)
	if fenced, ok := strings.CutPrefix(raw, "```json"); ok {
		raw = strings.TrimSpace(strings.TrimSuffix(fenced, "```"))
	}
	if !strings.HasPrefix(raw, "{") {
		return voicePlan{Action: "speak", Speech: raw}, nil
	}
	var plan voicePlan
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&plan); err != nil || decoder.Decode(new(any)) != io.EOF {
		return voicePlan{}, errors.New("语音回复格式无效")
	}
	switch plan.Action {
	case "speak", "end", "countdown", "reminder", "next_step", "tool", "start_practice", "stop_practice":
		return plan, nil
	default:
		return voicePlan{}, errors.New("语音回复动作无效")
	}
}

func (p *Platform) applyVoicePlan(plan voicePlan, request, language string, now time.Time) (voiceTaskReply, error) {
	ask := func(text string) voiceTaskReply {
		if language != "zh" {
			text = localizedVoiceText(language, "verify_action")
		}
		return voiceTaskReply{text: text, handled: true}
	}
	if plan.Action != "speak" && hypotheticalTask.MatchString(request) {
		return ask("你想让我实际执行吗？"), nil
	}
	switch plan.Action {
	case "countdown", "reminder":
		if plan.Seconds < 1 || plan.Seconds > 86400 {
			return ask("请说清楚时长，最多一天。"), nil
		}
		content := strings.TrimSpace(plan.Content)
		ability := "reminder"
		if plan.Action == "countdown" {
			content, ability = "计时结束。", quickTimer.ID
		}
		if content == "" || len(content) > 2000 {
			return ask("要提醒什么事？"), nil
		}
		err := p.scheduleVoiceTask(ability, content, time.Duration(plan.Seconds)*time.Second, now)
		return voiceTaskReply{text: localizedVoiceText(language, "action_ok"), handled: true, timer: plan.Action == "countdown"}, err
	case "next_step":
		return p.voiceTask("继续下一步", now)
	default:
		return voiceTaskReply{text: plan.Speech, handled: true}, nil
	}
}

func speechForLanguage(text, language string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return localizedVoiceText(language, "repeat")
	}
	return text
}

func nextSpeechPart(text string) (part, rest string) {
	const targetRunes = 120
	runes, sentence, clause := 0, 0, 0
	for index, char := range text {
		if runes == targetRunes {
			end := index
			if sentence > 0 {
				end = sentence
			} else if clause > 0 {
				end = clause
			}
			return strings.TrimSpace(text[:end]), strings.TrimSpace(text[end:])
		}
		runes++
		end := index + utf8.RuneLen(char)
		switch {
		case strings.ContainsRune("。！？!?…\n", char),
			char == '.' && (end == len(text) || text[end] == ' ' || text[end] == '\n'):
			sentence = end
		case strings.ContainsRune("，、；：", char), unicode.IsSpace(char):
			clause = end
		}
	}
	return text, ""
}
