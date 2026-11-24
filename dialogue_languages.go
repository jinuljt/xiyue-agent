package main

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
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

type languageCatalog struct {
	Languages []dialogueLanguage             `json:"languages"`
	TTS       map[string][]string            `json:"tts"`
	Voices    map[string]map[string][]string `json:"voices"`
	ASR       map[string][]string            `json:"asr"`
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
		"doubao": {doubaoDefaultVoice: {"zh", "en", "ja", "id", "es-MX"}},
	},
	ASR:      map[string][]string{"doubao": {"zh", "en"}, "sensevoice": {"zh", "en", "ja", "ko", "yue"}},
	Practice: []string{"en"},
}

func supportedReplyLanguages(config ServiceConfig) []string {
	if languages := languageOptions.Voices[config.Provider][config.Model]; languages != nil {
		return languages
	}
	return languageOptions.TTS[config.Provider]
}

func (s SystemConfig) replyLanguage() string {
	if s.Mode == "external" || !slices.Contains(supportedReplyLanguages(s.TTS), s.ReplyLanguage) {
		return "zh"
	}
	return s.ReplyLanguage
}

func (s SystemConfig) supportsPractice(language string) bool {
	return s.Mode != "external" && asrReady(s.ASR) &&
		slices.Contains(languageOptions.ASR[s.ASR.Provider], language) &&
		slices.Contains(languageOptions.Practice, language) &&
		slices.Contains(supportedReplyLanguages(s.TTS), language) &&
		(s.TTS.Provider == "system" || s.TTS.KeyConfigured)
}

func validateDialogueSettings(s SystemConfig) error {
	if (s.Mode == "external" && s.ReplyLanguage != "zh") ||
		(s.ReplyLanguage != "zh" && !slices.Contains(supportedReplyLanguages(s.TTS), s.ReplyLanguage)) {
		return errors.New("请选择当前播报服务可用的回答语言")
	}
	if s.PracticeEnabled && !s.supportsPractice("en") {
		return errors.New("英语陪练需要可用的英语语音识别、播报和音箱对话模式")
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

func dialoguePrompt(s SystemConfig, practice bool) string {
	if practice {
		return "\n当前为英语口语陪练。用自然、简短的英语扮演用户选择的情景。每轮最多给一个友善的表达改进，再继续角色对话并提一个问题。关注措辞和语法，无法从转写判断发音。只用 speak；用户明确要求退出陪练时返回 {\"action\":\"stop_practice\"}，想结束整场对话时返回 {\"action\":\"end\"}。"
	}
	languages := supportedReplyLanguages(s.TTS)
	if len(languages) <= 1 {
		return ""
	}
	codes, _ := json.Marshal(languages)
	prompt := "\n默认回答语言为 " + s.replyLanguage() + "。用户明确要求本轮用某种语言时，用该语言生成 speech，并加上 language 字段，使用这一列表里的语种代码：" + string(codes) + "。默认语言也应输出对应的 language。明确要求的语种不在列表时仍填写其语种代码，由音箱处理。"
	if s.PracticeEnabled && s.supportsPractice("en") {
		prompt += "\n仅当用户明确要求开始英语口语陪练时，返回 {\"action\":\"start_practice\",\"speech\":\"一句英文开场，并提出第一个问题\"}。明确指定其他陪练语言时用当前默认语言说明目前可以练英语。"
	}
	return prompt
}

type dialogueSession struct {
	history  []voiceMessage
	practice bool
}

func selectSpeechLanguage(settings SystemConfig, plan voicePlan) (string, bool) {
	lang := strings.TrimSpace(plan.Language)
	if lang == "" {
		return settings.replyLanguage(), true
	}
	return lang, slices.Contains(supportedReplyLanguages(settings.TTS), lang)
}
