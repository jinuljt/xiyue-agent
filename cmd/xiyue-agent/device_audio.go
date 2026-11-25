package main

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"
)

var errUnknownTone = errors.New("未知提示音，可选 cue 或 alarm")

func (a *api) deviceAudioRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/device/audio", func(w http.ResponseWriter, r *http.Request) {
		v := a.speechProbe
		s := a.platform.snapshot()
		model := s.Settings.LLM
		writeJSON(w, http.StatusOK, map[string]bool{
			"speak": ttsReady(v, s),
			"tone":  v != nil && v.playPCM != nil,
			"stop":  v != nil,
			"ask": a.voice != nil && a.voice.follow != nil && ttsReady(a.voice, s) && asrReady(s.Settings.ASR) &&
				s.Settings.Mode != "external" && model.Endpoint != "" && model.Model != "",
			"transcribe": asrReady(s.Settings.ASR),
		})
	})
	m.HandleFunc("POST /api/device/audio/{action}", a.devicePlayback)
	m.HandleFunc("POST /api/device/audio/transcribe", a.transcribeClip)
}

func (a *api) devicePlayback(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Text string `json:"text"`
		Tone string `json:"tone"`
	}
	if err := decode(w, r, &input); err != nil {
		bad(w, err)
		return
	}
	v := a.speechProbe
	if v == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "音箱音频未启用"})
		return
	}
	s := a.platform.snapshot()
	var play func(context.Context) error
	switch r.PathValue("action") {
	case "stop":
		if input.Text != "" || input.Tone != "" {
			bad(w, errors.New("停止播放不接受文本或提示音参数"))
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"cancel_requested": v.stopPlayback()})
		return
	case "speak":
		input.Text = strings.TrimSpace(input.Text)
		if input.Text == "" || len(input.Text) > 4096 || input.Tone != "" {
			bad(w, errors.New("需要不超过 4096 字节的播报文字"))
			return
		}
		play = func(ctx context.Context) error { return a.platform.playConfiguredSpeech(ctx, v, s, input.Text) }
	case "tone":
		clip, ok := deviceTones[input.Tone]
		if !ok || input.Text != "" {
			bad(w, errUnknownTone)
			return
		}
		play = func(ctx context.Context) error { return v.playTone(ctx, clip) }
	default:
		http.NotFound(w, r)
		return
	}

	v.mu.Lock()
	if v.active || v.conversing {
		v.mu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]string{"error": errVoiceBusy.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	v.active, v.cancel = true, cancel
	v.mu.Unlock()
	defer func() {
		cancel()
		v.mu.Lock()
		v.active, v.cancel = false, nil
		v.mu.Unlock()
	}()
	err := play(ctx)
	switch {
	case errors.Is(err, context.Canceled):
		writeJSON(w, http.StatusOK, map[string]string{"status": "interrupted"})
	case err != nil:
		bad(w, err)
	default:
		writeJSON(w, http.StatusOK, map[string]string{"status": "completed"})
	}
}

func (a *api) transcribeClip(w http.ResponseWriter, r *http.Request) {
	contentType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if contentType != "audio/wav" {
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "Content-Type 需要为 audio/wav"})
		return
	}
	wav, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxVoiceWAV))
	pcm := voiceWAVPCM(wav)
	if err != nil || pcm == nil {
		bad(w, errors.New("需要不超过 30 秒的 16 kHz 单声道 16 位 PCM WAV"))
		return
	}
	s := a.platform.snapshot()
	if !asrReady(s.Settings.ASR) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "请先配置语音识别服务"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	text, err := a.platform.captureAndTranscribe(ctx, s, func(_ context.Context, emit func([]byte) error) ([]byte, error) {
		if emit != nil {
			return nil, emit(pcm)
		}
		return wav, nil
	}, nil, &voiceResult{}, false)
	if err != nil {
		bad(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"text": text})
}
