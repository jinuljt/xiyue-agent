package main

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"
)

const testSpokenText = "你好，这是一条喜悦的语音测试。"

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
	v.mu.Lock()
	if v.active {
		v.mu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]string{"error": "音箱正在录音或播报，请稍后再试"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	v.active, v.cancel = true, cancel
	v.mu.Unlock()
	defer func() {
		cancel()
		v.mu.Lock()
		defer v.mu.Unlock()
		v.active, v.cancel = false, nil
	}()
	start := time.Now()
	result := map[string]any{"ok": true}
	if service == "asr" {
		if err := v.yieldRoom(ctx); err != nil {
			bad(w, err)
			return
		}
		text, err := a.platform.captureAndTranscribe(ctx, s, a.probeCapture, temporary, &voiceResult{}, true)
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
