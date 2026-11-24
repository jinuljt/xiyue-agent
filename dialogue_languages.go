package main

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"
)

type dialogueLanguage struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Sample string `json:"sample"`
}

var dialogueLanguages = []dialogueLanguage{
	{"zh", "普通话", testSpokenText},
	{"en", "英语", "Could I have a cup of coffee, please?"},
	{"ja", "日语", "こんにちは。コーヒーを一杯お願いします。"},
	{"ko", "韩语", "안녕하세요. 커피 한 잔 주세요."},
	{"yue", "粤语", "唔該，我想要一杯咖啡。"},
	{"ms", "马来语", "Boleh saya minta secawan kopi?"},
	{"ar", "阿拉伯语", "أريد كوبًا من القهوة من فضلك."},
	{"id", "印度尼西亚语", "Boleh saya minta secangkir kopi?"},
	{"th", "泰语", "ขอกาแฟหนึ่งแก้วครับ"},
	{"vi", "越南语", "Cho tôi một ly cà phê, cảm ơn."},
	{"es-MX", "西班牙语（墨西哥）", "¿Me da un café, por favor?"},
}

type voicePreset struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type languageCatalog struct {
	Languages []dialogueLanguage             `json:"languages"`
	TTS       map[string][]string            `json:"tts"`
	Voices    map[string]map[string][]string `json:"voices"`
	ASR       map[string][]string            `json:"asr"`
	Presets   map[string][]voicePreset       `json:"presets"`
	Practice  []string                       `json:"practice"`
}

var languageOptions = languageCatalog{
	Languages: dialogueLanguages,
	TTS: map[string][]string{
		"system":   {"zh"},
		"doubao":   {"zh"},
		"deepseek": {"zh", "en", "ja", "ko", "yue", "ms", "ar", "id", "th", "vi"},
	},
	Voices: map[string]map[string][]string{
		"doubao": {
			doubaoDefaultVoice:               {"zh", "en", "ja", "id", "es-MX"},
			"zh_female_xiaohe_uranus_bigtts": {"zh", "en"},
			"zh_male_m191_uranus_bigtts":     {"zh", "en"},
		},
	},
	Presets: map[string][]voicePreset{
		"doubao":   {{doubaoDefaultVoice, "Vivi 2.0"}, {"zh_female_xiaohe_uranus_bigtts", "小何 2.0"}, {"zh_male_m191_uranus_bigtts", "云舟 2.0"}},
		"deepseek": {{"stella", "海星"}, {"tide", "暗潮"}},
	},
	ASR:      map[string][]string{"doubao": {"zh", "en", "yue"}, "sensevoice": {"zh", "en", "ja", "ko", "yue"}},
	Practice: []string{"en", "ja", "ko", "yue"},
}

func supportedReplyLanguages(config ServiceConfig) []string {
	if languages := languageOptions.Voices[config.Provider][config.Model]; languages != nil {
		return languages
	}
	return languageOptions.TTS[config.Provider]
}

func (s SystemConfig) supportsDialogue(language string) bool {
	return language == "zh" || (s.Mode != "external" &&
		slices.Contains(languageOptions.ASR[s.ASR.Provider], language) &&
		slices.Contains(supportedReplyLanguages(s.TTS), language))
}

func (s SystemConfig) replyLanguage() string {
	if !s.supportsDialogue(s.ReplyLanguage) {
		return "zh"
	}
	return s.ReplyLanguage
}

func (s SystemConfig) supportsPractice(language string) bool {
	return asrReady(s.ASR) && s.supportsDialogue(language) &&
		slices.Contains(languageOptions.Practice, language) &&
		(s.TTS.Provider == "system" || s.TTS.KeyConfigured)
}

func validateDialogueSettings(s SystemConfig) error {
	if !s.supportsDialogue(s.ReplyLanguage) {
		return errors.New("对话语言需要当前语音识别和音色同时支持")
	}
	return nil
}

func speechSample(language string) (string, error) {
	for _, item := range dialogueLanguages {
		if item.ID == language {
			return item.Sample, nil
		}
	}
	return "", errors.New("请选择试听语言")
}

func (s SystemConfig) practiceLanguage() string {
	if s.PracticeLanguage == "" {
		return "en"
	}
	return s.PracticeLanguage
}

func (s SystemConfig) practiceChoices() []dialogueLanguage {
	choices := []dialogueLanguage{}
	for _, item := range dialogueLanguages {
		if s.supportsPractice(item.ID) {
			choices = append(choices, item)
		}
	}
	return choices
}

func (s SystemConfig) practiceStatus() map[string]any {
	return map[string]any{"revision": s.Revision, "enabled": s.PracticeEnabled, "language": s.practiceLanguage(), "available": s.practiceChoices(),
		"asr_ready": asrReady(s.ASR), "tts_ready": s.TTS.Provider == "system" || s.TTS.KeyConfigured, "external_only": s.Mode == "external",
		"asr_languages": languageOptions.ASR[s.ASR.Provider], "tts_languages": supportedReplyLanguages(s.TTS),
		"languages": slices.DeleteFunc(slices.Clone(dialogueLanguages), func(item dialogueLanguage) bool {
			return !slices.Contains(languageOptions.Practice, item.ID)
		})}
}

func (a *api) practiceRoutes(m *http.ServeMux) {
	p := a.platform
	m.HandleFunc("GET /api/abilities/language-practice", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, p.snapshot().Settings.practiceStatus())
	})
	m.HandleFunc("PUT /api/abilities/language-practice", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Enabled  bool   `json:"enabled"`
			Language string `json:"language"`
		}
		if err := decode(w, r, &in); err != nil {
			bad(w, err)
			return
		}
		err := p.change(func(s *State) error {
			if in.Enabled && !s.Settings.supportsPractice(in.Language) {
				return errors.New("请选择当前识别和播报都支持的陪练语言")
			}
			if in.Enabled {
				s.Settings.PracticeLanguage = in.Language
			}
			s.Settings.PracticeEnabled = in.Enabled
			s.Settings.Revision++
			now := time.Now().UTC()
			s.Settings.UpdatedAt = &now
			return nil
		})
		if err != nil {
			bad(w, err)
			return
		}
		writeJSON(w, 200, p.snapshot().Settings.practiceStatus())
	})
}

func dialoguePrompt(s SystemConfig, practice string) string {
	if practice != "" {
		return "\n当前为口语陪练，语种代码为 " + practice + "。用该语言自然、简短地扮演用户选择的情景。每轮最多给一个友善的表达改进，再继续角色对话并提一个问题。关注措辞和语法，无法从转写判断发音。只用 speak；用户明确要求退出陪练时返回 {\"action\":\"stop_practice\"}，想结束整场对话时返回 {\"action\":\"end\"}。"
	}
	prompt := ""
	if language := s.replyLanguage(); language != "zh" {
		prompt = "\n本轮对话语言固定为 " + language + "。所有播报内容都用该语种，并在 speak 中填写对应的 language。遇到要求切换语言的请求，用当前语种说明可以在语音设置更改对话语言。"
	}
	if s.PracticeEnabled && s.supportsPractice(s.practiceLanguage()) {
		prompt += "\n仅当用户明确要求开始口语陪练时，使用已配置的陪练语种 " + s.practiceLanguage() + "，返回 {\"action\":\"start_practice\",\"speech\":\"用已配置语种开场，并提出第一个问题\"}。如果用户明确指定另一种陪练语言，说明目前配置的陪练语言。"
	}
	return prompt
}

type dialogueSession struct {
	history  []voiceMessage
	practice string
}

func selectSpeechLanguage(settings SystemConfig, plan voicePlan) (string, bool) {
	language := settings.replyLanguage()
	reported := strings.TrimSpace(plan.Language)
	return language, reported == "" || strings.EqualFold(reported, language)
}

func voiceDisplayName(config ServiceConfig) string {
	if config.Provider == "system" {
		return "音箱系统语音"
	}
	for _, preset := range languageOptions.Presets[config.Provider] {
		if preset.ID == config.Model {
			return preset.Name
		}
	}
	return config.Model
}

// Short device-generated states follow the same language as model replies.
func localizedVoiceText(language, message string) string {
	text := map[string]map[string]string{
		"repeat":               {"zh": "我没听明白，请再说一遍。", "en": "I didn't catch that. Please say it again.", "ja": "もう一度お願いします。", "ko": "다시 말씀해 주세요.", "yue": "我聽唔清楚，請講多次。"},
		"practice_end":         {"zh": "已退出外语陪练。", "en": "Practice is over.", "ja": "会話の練習を終了しました。", "ko": "회화 연습을 마쳤어요.", "yue": "口語練習結束喇。"},
		"practice_unavailable": {"zh": "请先在能力中心启用可用的外语陪练。", "en": "Enable language practice in the console first.", "ja": "先に設定画面で会話練習を有効にしてください。", "ko": "설정에서 회화 연습을 먼저 켜 주세요.", "yue": "請先喺控制台啟用口語練習。"},
		"verify_action":        {"zh": "请确认操作和参数后再试。", "en": "Please check the request and try again.", "ja": "内容を確認して、もう一度お願いします。", "ko": "요청을 확인하고 다시 말씀해 주세요.", "yue": "請確認要做嘅事，再試一次。"},
		"action_ok":            {"zh": "已设置。", "en": "Done.", "ja": "設定しました。", "ko": "설정했어요.", "yue": "設定好喇。"},
		"tool_unavailable":     {"zh": "这次无法读取所需的实时信息。", "en": "I couldn't check the live information.", "ja": "最新情報を確認できませんでした。", "ko": "최신 정보를 확인할 수 없었어요.", "yue": "今次查唔到即時資料。"},
		"search_empty":         {"zh": "这次搜索没有找到可用结果。", "en": "I didn't find any useful results.", "ja": "該当する検索結果はありませんでした。", "ko": "검색 결과를 찾지 못했어요.", "yue": "今次搵唔到有用嘅結果。"},
	}[message]
	if result := text[language]; result != "" {
		return result
	}
	return text["zh"]
}
