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
	confirmLogin func(string) bool
	speak        func(context.Context, string) error
	playPCM      pcmPlayer
	mu           sync.Mutex
	active       bool
	cancel       context.CancelFunc
	last         *voiceResult
	wakeReady    bool
}

type voiceCapture func(context.Context, func([]byte) error) ([]byte, error)

type voiceResult struct {
	modelUsed  bool
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
	return map[string]any{"available": true, "cloud_tts_available": v.playPCM != nil, "busy": v.active, "last_turn": v.last, "wake_detector_ready": v.wakeReady}
}

func (v *voiceRuntime) setWakeReady(ready bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.wakeReady = ready
}

// Accept only finished clips from the device's 16 kHz mono PCM capture boundary.
func validVoiceWAV(data []byte) bool {
	return voiceWAVPCM(data) != nil
}

func voiceWAVPCM(data []byte) []byte {
	if len(data) < 44 || len(data) > maxVoiceWAV || string(data[:4]) != "RIFF" ||
		string(data[8:12]) != "WAVE" || uint64(binary.LittleEndian.Uint32(data[4:8]))+8 != uint64(len(data)) {
		return nil
	}
	var formatOK bool
	var pcm []byte
	for offset := 12; offset < len(data); {
		if len(data)-offset < 8 {
			return nil
		}
		chunk := data[offset:]
		size := uint64(binary.LittleEndian.Uint32(chunk[4:8]))
		padded := size + size%2
		if padded > uint64(len(chunk)-8) {
			return nil
		}
		body := chunk[8 : 8+int(size)]
		switch string(chunk[:4]) {
		case "fmt ":
			if formatOK || len(body) < 16 {
				return nil
			}
			formatOK = binary.LittleEndian.Uint16(body) == 1 &&
				binary.LittleEndian.Uint16(body[2:]) == 1 && binary.LittleEndian.Uint32(body[4:]) == 16000 &&
				binary.LittleEndian.Uint32(body[8:]) == 32000 && binary.LittleEndian.Uint16(body[12:]) == 2 &&
				binary.LittleEndian.Uint16(body[14:]) == 16
			if !formatOK {
				return nil
			}
		case "data":
			if pcm != nil || size == 0 || size%2 != 0 || size > 16000*2*30 {
				return nil
			}
			pcm = body
		}
		offset += 8 + int(padded)
	}
	if !formatOK {
		return nil
	}
	return pcm
}

func asrReady(asr ServiceConfig) bool {
	return (asr.Provider == "sensevoice" && asr.Endpoint != "") ||
		(asr.Provider == "doubao" && asr.Endpoint == "" && asr.KeyConfigured)
}

func (p *Platform) captureAndTranscribe(ctx context.Context, s State, capture voiceCapture, temporary *string, result *voiceResult) (string, error) {
	cfg := s.Settings.ASR
	key, err := p.serviceKey(s, "asr", temporary)
	if err != nil {
		return "", err
	}
	start := time.Now()
	if cfg.Provider == "doubao" {
		var captureTime time.Duration
		text, err := transcribeDoubao(ctx, cfg, key, func(ctx context.Context, emit func([]byte) error) error {
			_, err := capture(ctx, emit)
			captureTime = time.Since(start)
			return err
		})
		result.CaptureMS = captureTime.Milliseconds()
		result.Stage = "asr"
		result.ASRMS = (time.Since(start) - captureTime).Milliseconds()
		return text, err
	}
	wav, err := capture(ctx, nil)
	result.CaptureMS = time.Since(start).Milliseconds()
	if err != nil {
		return "", err
	}
	if !validVoiceWAV(wav) {
		return "", errors.New("音箱没有收到可识别的录音")
	}
	result.Stage = "asr"
	start = time.Now()
	text, err := transcribeSenseVoice(ctx, cfg, key, wav)
	result.ASRMS = time.Since(start).Milliseconds()
	return text, err
}

func transcribeSenseVoice(ctx context.Context, cfg ServiceConfig, key string, wav []byte) (string, error) {
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
	if key != "" {
		u, _ := url.Parse(cfg.Endpoint)
		if u.Scheme != "https" && u.Hostname() != "localhost" && !net.ParseIP(u.Hostname()).IsLoopback() {
			return "", errors.New("带凭证的语音识别服务须用 HTTPS")
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
		pcm := voiceWAVPCM(wav)
		if err != nil || pcm == nil {
			bad(w, errors.New("需要不超过 30 秒的 16 kHz 单声道 16 位 PCM WAV"))
			return
		}
		result, err := a.platform.runVoiceTurn(r.Context(), v, func(_ context.Context, emit func([]byte) error) ([]byte, error) {
			if emit != nil {
				return nil, emit(pcm)
			}
			return wav, nil
		}, nil)
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
func (p *Platform) runVoiceTurn(parent context.Context, v *voiceRuntime, capture voiceCapture, captureLED func(bool)) (result voiceResult, err error) {
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
	if !asrReady(s.Settings.ASR) {
		return result, errors.New("请先配置自建 SenseVoice 或豆包语音识别")
	}
	result.Stage = "capture"
	text, err := p.captureAndTranscribe(ctx, s, func(ctx context.Context, emit func([]byte) error) ([]byte, error) {
		if captureLED != nil {
			captureLED(true)
			defer captureLED(false)
		}
		return capture(ctx, emit)
	}, nil, &result)
	if err != nil {
		return result, err
	}
	if v.confirmLogin != nil && v.confirmLogin(text) {
		result.Stage = "confirmed_on_device"
		return result, nil
	}
	if !ttsReady(v, s) {
		result.Stage = "configuration"
		return result, errors.New("请配置系统语音或可用的豆包语音合成服务")
	}
	result.Stage = "local_task"
	command := text
	if suffix, found := strings.CutPrefix(text, s.Settings.WakeWord); found {
		command = strings.TrimLeft(suffix, "，,。！!：: ")
	}
	reply, handled, err := p.voiceTask(command, time.Now().UTC())
	if err != nil {
		return result, err
	}
	if !handled {
		result.modelUsed = true
		result.Stage = "model"
		start := time.Now()
		reply, err = p.respond(ctx, s, text, false)
		result.ModelMS = time.Since(start).Milliseconds()
		if err != nil {
			return result, err
		}
	}
	result.Stage = "playback"
	start := time.Now()
	err = p.playConfiguredSpeech(ctx, v, s, reply)
	result.PlaybackMS = time.Since(start).Milliseconds()
	if err != nil {
		return result, err
	}
	result.Stage = "completed"
	return result, nil
}
