package main

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"
)

const testSpokenText = "你好，这是一条喜悦的语音测试。"

func draftService(name string, current ServiceConfig, input ServiceInput) (ServiceConfig, *string, error) {
	candidate, err := normalizeServiceInput(name, input)
	if err != nil {
		return candidate, nil, err
	}
	key := input.APIKey
	if key != "" {
		if err := validateServiceKey(key); err != nil {
			return candidate, nil, err
		}
	}
	var temporary *string
	if key != "" || input.ClearKey || candidate.Provider != current.Provider || candidate.Endpoint != current.Endpoint {
		temporary = &key
	}
	candidate.KeyConfigured = key != "" || (temporary == nil && current.KeyConfigured)
	return candidate, temporary, nil
}

func (a *api) verifySpeech(w http.ResponseWriter, r *http.Request, s State, service, language string, temporary *string) {
	if a.admin != nil && r.Header.Get("Origin") == "" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "请从音箱控制台发起验证"})
		return
	}
	v := a.speechProbe
	if service == "asr" {
		if a.probeCapture == nil || v == nil {
			bad(w, errors.New("没有可用的音箱麦克风"))
			return
		}
		if !asrReady(s.Settings.ASR) {
			bad(w, errors.New("请填写识别服务地址或豆包语音 API Key"))
			return
		}
		if language != "" && !slices.Contains(languageOptions.ASR[s.Settings.ASR.Provider], language) {
			bad(w, errors.New("当前识别服务不支持所选对话语言"))
			return
		}
	} else if !ttsReady(v, s) {
		bad(w, errors.New("请选择可用的音箱播报方式，并填写所需凭证和音色"))
		return
	}
	sample := testSpokenText
	if service == "tts" && language != "" {
		var err error
		sample, err = speechSample(language)
		// System audio has no language selector. An explicit preview requests raw text for testing.
		if err != nil || (s.Settings.TTS.Provider != "system" && !slices.Contains(supportedReplyLanguages(s.Settings.TTS), language)) {
			bad(w, errors.New("当前服务或音色不支持这个试听语种"))
			return
		}
	}
	ctx, finish, err := beginSpeechProbe(r.Context(), v)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	defer finish()
	start := time.Now()
	result := map[string]any{"ok": true}
	if service == "asr" {
		if err := v.yieldRoom(ctx); err != nil {
			bad(w, err)
			return
		}
		if language == "" {
			language = s.Settings.replyLanguage()
		}
		text, err := a.platform.captureAndTranscribe(ctx, s, a.probeCapture, temporary, &voiceResult{Language: language}, true)
		if err != nil {
			bad(w, err)
			return
		}
		result["text"] = text
	} else if err := a.platform.playSpeechWithKey(ctx, v, s, sample, language, temporary, true); err != nil {
		bad(w, err)
		return
	}
	result["duration_ms"] = time.Since(start).Milliseconds()
	writeJSON(w, 200, result)
}

func beginSpeechProbe(parent context.Context, v *voiceRuntime) (context.Context, func(), error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.active {
		return nil, nil, errors.New("音箱正在录音或播报，请稍后再试")
	}
	ctx, cancel := context.WithTimeout(parent, 42*time.Second)
	v.active, v.cancel = true, cancel
	return ctx, func() {
		cancel()
		v.mu.Lock()
		defer v.mu.Unlock()
		v.active, v.cancel = false, nil
	}, nil
}

type dialogueProbeResult struct {
	Stage      string `json:"stage"`
	Language   string `json:"language"`
	Voice      string `json:"voice"`
	Transcript string `json:"transcript,omitempty"`
	Reply      string `json:"reply,omitempty"`
	Error      string `json:"error,omitempty"`
}

func (a *api) verifyDialogue(w http.ResponseWriter, r *http.Request, s State, language string, asr, tts *ServiceInput) {
	if a.admin != nil && r.Header.Get("Origin") == "" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "请从音箱控制台发起试说"})
		return
	}
	if a.probeCapture == nil || a.speechProbe == nil {
		bad(w, errors.New("没有可用的音箱麦克风"))
		return
	}
	if s.Settings.Mode == "external" {
		bad(w, errors.New("仅外部 Agent 模式请使用单独的识别验证和音色试听。试说一句需要已配置的大模型。"))
		return
	}
	var asrKey, ttsKey *string
	services := []struct {
		name   string
		input  *ServiceInput
		config *ServiceConfig
		key    **string
	}{
		{"asr", asr, &s.Settings.ASR, &asrKey},
		{"tts", tts, &s.Settings.TTS, &ttsKey},
	}
	for _, service := range services {
		if service.input != nil {
			cfg, key, err := draftService(service.name, *service.config, *service.input)
			if err != nil {
				bad(w, err)
				return
			}
			*service.config, *service.key = cfg, key
		}
	}
	s.Settings.ReplyLanguage = language
	if err := validateDialogueSettings(s.Settings); err != nil {
		bad(w, err)
		return
	}
	result := dialogueProbeResult{Stage: "asr", Language: language, Voice: voiceDisplayName(s.Settings.TTS)}
	if !asrReady(s.Settings.ASR) {
		result.Error = "请先配置语音识别服务"
		writeJSON(w, 200, result)
		return
	}
	if !llmConfigured(s.Settings.LLM) {
		result.Stage, result.Error = "model", "请先配置大模型"
		writeJSON(w, 200, result)
		return
	}
	ctx, finish, err := beginSpeechProbe(r.Context(), a.speechProbe)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	defer finish()
	result = a.runDialogueProbe(ctx, s, result, asrKey, ttsKey)
	writeJSON(w, 200, result)
}

func (a *api) runDialogueProbe(ctx context.Context, s State, result dialogueProbeResult, asrKey, ttsKey *string) (out dialogueProbeResult) {
	out = result
	var err error
	defer func() {
		if err != nil {
			out.Error = err.Error()
		}
	}()
	if err = a.speechProbe.yieldRoom(ctx); err != nil {
		return
	}
	out.Transcript, err = a.platform.captureAndTranscribe(ctx, s, a.probeCapture, asrKey, &voiceResult{Language: out.Language}, true)
	if err != nil {
		return
	}
	out.Stage = "model"
	s.Settings.Mode = "direct"
	var plan voicePlan
	_, err = a.platform.requestReply(ctx, s, out.Transcript, replyOptions{
		dialogueProbe: true, voicePlan: &plan,
		dialogueInstructions: "\n测试语种为 " + out.Language + "。只用此语种回答和填写 language。",
	})
	if err != nil {
		return
	}
	out.Reply = plan.Speech
	if _, matches := selectSpeechLanguage(s.Settings, plan); !matches || plan.Action != "speak" || strings.TrimSpace(plan.Speech) == "" {
		err = errors.New("模型未返回本轮对话语种的可播报回复")
		return
	}
	out.Stage = "playback"
	out.Reply = speechForLanguage(out.Reply, out.Transcript, out.Language)
	err = a.platform.playSpeechWithKey(ctx, a.speechProbe, s, out.Reply, out.Language, ttsKey, true)
	if err == nil {
		out.Stage = "completed"
	}
	return
}
