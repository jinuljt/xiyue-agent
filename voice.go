package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	systemTTS   = "/usr/sbin/tts_play.sh"
	maxVoiceWAV = 16_000*2*30 + 4096
)

type voiceRuntime struct {
	speak  func(context.Context, string) error
	mu     sync.Mutex
	active bool
	cancel context.CancelFunc
	last   *voiceResult
}

type voiceResult struct {
	CaptureMS  int64  `json:"capture_ms,omitempty"`
	Revision   int    `json:"revision"`
	Stage      string `json:"stage"`
	ASRMS      int64  `json:"asr_ms"`
	ModelMS    int64  `json:"model_ms"`
	PlaybackMS int64  `json:"playback_ms"`
}

func (v *voiceRuntime) status() any {
	if v == nil {
		return map[string]bool{"available": false}
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return map[string]any{"available": true, "busy": v.active, "last_turn": v.last}
}

// Accept only finished clips from the device's 16 kHz mono PCM capture boundary.
func validVoiceWAV(data []byte) bool {
	if len(data) < 44 || len(data) > maxVoiceWAV || string(data[:4]) != "RIFF" ||
		string(data[8:12]) != "WAVE" || uint64(binary.LittleEndian.Uint32(data[4:8]))+8 != uint64(len(data)) {
		return false
	}
	var formatOK, audioOK bool
	for offset := 12; offset < len(data); {
		if len(data)-offset < 8 {
			return false
		}
		chunk := data[offset:]
		size := uint64(binary.LittleEndian.Uint32(chunk[4:8]))
		padded := size + size%2
		if padded > uint64(len(chunk)-8) {
			return false
		}
		body := chunk[8 : 8+int(size)]
		switch string(chunk[:4]) {
		case "fmt ":
			if formatOK || len(body) < 16 {
				return false
			}
			formatOK = binary.LittleEndian.Uint16(body) == 1 &&
				binary.LittleEndian.Uint16(body[2:]) == 1 && binary.LittleEndian.Uint32(body[4:]) == 16000 &&
				binary.LittleEndian.Uint32(body[8:]) == 32000 && binary.LittleEndian.Uint16(body[12:]) == 2 &&
				binary.LittleEndian.Uint16(body[14:]) == 16
			if !formatOK {
				return false
			}
		case "data":
			if audioOK || size == 0 || size%2 != 0 || size > 16000*2*30 {
				return false
			}
			audioOK = true
		}
		offset += 8 + int(padded)
	}
	return formatOK && audioOK
}

func (p *Platform) transcribe(ctx context.Context, s State, wav []byte) (string, error) {
	cfg := s.Settings.ASR
	if cfg.Provider != "sensevoice" || cfg.Endpoint == "" {
		return "", errors.New("请先配置 FunASR SenseVoice 服务地址")
	}
	if validateEndpoint(cfg.Endpoint) != nil {
		return "", errors.New("语音识别地址无效")
	}
	model := cfg.Model
	if model == "" {
		model = "sensevoice"
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	if err := form.WriteField("model", model); err != nil {
		return "", err
	}
	file, err := form.CreateFormFile("file", "utterance.wav")
	if err != nil {
		return "", err
	}
	if _, err = file.Write(wav); err != nil {
		return "", err
	}
	if err = form.Close(); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.Endpoint+"/audio/transcriptions", &body)
	if err != nil {
		return "", errors.New("语音识别地址无效")
	}
	req.Header.Set("Content-Type", form.FormDataContentType())
	if sealed := s.Sealed["service:asr"]; sealed != "" {
		u, _ := url.Parse(cfg.Endpoint)
		if u.Scheme != "https" && u.Hostname() != "localhost" && !net.ParseIP(u.Hostname()).IsLoopback() {
			return "", errors.New("带凭证的语音识别服务须用 HTTPS")
		}
		key, err := p.unseal("service:asr", sealed)
		if err != nil {
			return "", err
		}
		req.Header.Set("Authorization", "Bearer "+key)
	}
	client := http.Client{Timeout: 12 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return "", errors.New("语音识别服务连接失败或超时")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", errors.New("语音识别请求失败，请检查服务地址与凭证")
	}
	var result struct {
		Text string `json:"text"`
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil || json.Unmarshal(data, &result) != nil {
		return "", errors.New("语音识别返回格式无效")
	}
	result.Text = strings.TrimSpace(result.Text)
	if result.Text == "" || len(result.Text) > 16000 {
		return "", errors.New("没有识别到可用文字")
	}
	return result.Text, nil
}

func speakSystem(ctx context.Context, text string) error {
	// The firmware script embeds the argument in a JSON string and parses leading '-' as flags.
	// Replace characters its parser cannot safely represent. No shell expansion is used here.
	text = strings.Map(func(r rune) rune {
		switch r {
		case '"':
			return '”'
		case '\\':
			return '／'
		default:
			if unicode.IsControl(r) {
				return ' '
			}
			return r
		}
	}, text)
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "-") {
		text = " " + text
	}
	if len(text) == 0 || len(text) > 4096 {
		return errors.New("系统播报内容为空或过长")
	}
	cmd := exec.CommandContext(ctx, systemTTS, "--notify-pns", text)
	if err := cmd.Run(); err != nil {
		return errors.New("音箱系统播报失败或超时")
	}
	return nil
}

func (a *api) voiceRoutes(m *http.ServeMux) {
	m.HandleFunc("POST /api/voice/turn", func(w http.ResponseWriter, r *http.Request) {
		v := a.voice
		if v == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "系统语音入口未启用"})
			return
		}
		contentType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if contentType != "audio/wav" {
			writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "Content-Type 需要为 audio/wav"})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxVoiceWAV)
		wav, err := io.ReadAll(r.Body)
		if err != nil || !validVoiceWAV(wav) {
			bad(w, errors.New("需要不超过 30 秒的 16 kHz 单声道 16 位 PCM WAV"))
			return
		}
		result, err := a.platform.runVoiceTurn(r.Context(), v, func(context.Context) ([]byte, error) { return wav, nil }, nil)
		if errors.Is(err, errVoiceBusy) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		} else if err != nil {
			bad(w, err)
		} else {
			writeJSON(w, 200, result)
		}
	})
}

var errVoiceBusy = errors.New("音箱正在处理另一段语音")

func (v *voiceRuntime) stopTurn() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.cancel != nil {
		v.cancel()
	}
}
func (p *Platform) runVoiceTurn(parent context.Context, v *voiceRuntime, capture func(context.Context) ([]byte, error), captureLED func(bool)) (result voiceResult, err error) {
	v.mu.Lock()
	if v.active {
		v.mu.Unlock()
		return result, errVoiceBusy
	}
	ctx, cancel := context.WithTimeout(parent, 50*time.Second)
	v.active, v.cancel = true, cancel
	v.mu.Unlock()
	defer func() {
		if ctx.Err() != nil {
			result.Stage = "cancelled"
			if err == nil {
				err = ctx.Err()
			}
		}
		cancel()
		v.mu.Lock()
		defer v.mu.Unlock()
		v.active = false
		v.cancel = nil
		v.last = &result
	}()
	s := p.snapshot()
	result.Revision = s.Settings.Revision
	result.Stage = "configuration"
	if s.Settings.TTS.Provider != "system" {
		return result, errors.New("语音入口目前仅支持系统语音")
	}
	if s.Settings.ASR.Provider != "sensevoice" || s.Settings.ASR.Endpoint == "" {
		return result, errors.New("请先配置 FunASR SenseVoice 服务地址")
	}
	result.Stage = "capture"
	start := time.Now()
	if captureLED != nil {
		captureLED(true)
	}
	wav, err := capture(ctx)
	if captureLED != nil {
		captureLED(false)
	}
	result.CaptureMS = time.Since(start).Milliseconds()
	if err != nil {
		return result, err
	}
	result.Stage = "asr"
	start = time.Now()
	text, err := p.transcribe(ctx, s, wav)
	result.ASRMS = time.Since(start).Milliseconds()
	if err != nil {
		return result, err
	}
	result.Stage = "model"
	start = time.Now()
	reply, err := p.respond(ctx, s, text, false)
	result.ModelMS = time.Since(start).Milliseconds()
	if err != nil {
		return result, err
	}
	result.Stage = "playback"
	start = time.Now()
	err = v.speak(ctx, reply)
	result.PlaybackMS = time.Since(start).Milliseconds()
	if err != nil {
		return result, err
	}
	result.Stage = "completed"
	return result, nil
}
