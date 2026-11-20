package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"
)

// Public configuration contains credential presence only; ciphertext never leaves the API.
type ServiceConfig struct {
	Provider      string `json:"provider"`
	Endpoint      string `json:"endpoint"`
	Model         string `json:"model"`
	KeyConfigured bool   `json:"key_configured"`
}
type SystemConfig struct {
	Revision  int           `json:"revision"`
	Name      string        `json:"name"`
	WakeWord  string        `json:"wake_word"`
	Timezone  string        `json:"timezone"`
	Mode      string        `json:"mode"`
	ASR       ServiceConfig `json:"asr"`
	LLM       ServiceConfig `json:"llm"`
	TTS       ServiceConfig `json:"tts"`
	External  ServiceConfig `json:"external"`
	UpdatedAt *time.Time    `json:"updated_at,omitempty"`
}
type ServiceInput struct {
	Provider string `json:"provider"`
	Endpoint string `json:"endpoint"`
	Model    string `json:"model"`
	APIKey   string `json:"api_key"`
	ClearKey bool   `json:"clear_key"`
}
type SettingsInput struct {
	Revision int          `json:"revision"`
	Name     string       `json:"name"`
	WakeWord string       `json:"wake_word"`
	Timezone string       `json:"timezone"`
	Mode     string       `json:"mode"`
	ASR      ServiceInput `json:"asr"`
	LLM      ServiceInput `json:"llm"`
	TTS      ServiceInput `json:"tts"`
	External ServiceInput `json:"external"`
}
type Provider struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Endpoint string `json:"endpoint"`
	Model    string `json:"model"`
	Note     string `json:"note"`
}

func providers() map[string][]Provider {
	return map[string][]Provider{
		"llm": {
			{ID: "deepseek", Name: "DeepSeek", Endpoint: "https://api.deepseek.com", Model: "deepseek-flash", Note: "兼容 Chat Completions；使用自己的 API Key。"},
			{ID: "doubao", Name: "豆包 · 火山方舟", Endpoint: "https://ark.cn-beijing.volces.com/api/v3", Note: "填写方舟控制台中的模型 ID / 接入点 ID。"},
			{ID: "qwen", Name: "通义千问 · 阿里云百炼", Endpoint: "https://dashscope.aliyuncs.com/compatible-mode/v1", Note: "按账号地域填写服务地址和模型；可替换为工作空间专属地址。"},
			{ID: "custom", Name: "其他 / 自定义兼容服务", Note: "填写兼容 Chat Completions 的 Base URL 和模型名称。"},
		},
		"asr": {
			{ID: "doubao", Name: "豆包语音", Note: "保存接入信息；豆包流式 ASR 适配尚未接入，本页不会宣称已连接。"},
			{ID: "sensevoice", Name: "自建 SenseVoice", Note: "填写你部署的识别服务地址；录音与识别链路尚未接入。"},
			{ID: "custom", Name: "其他语音识别服务", Note: "保存服务地址与接入凭证，协议适配需另行接入。"},
		},
		"tts": {
			{ID: "system", Name: "音箱系统语音（推荐）", Note: "无需额外密钥；将在连接音箱后调用系统语音服务。"},
			{ID: "doubao", Name: "豆包语音合成", Note: "保存服务地址、音色与凭证；播报适配尚未接入。"},
			{ID: "custom", Name: "其他语音合成服务", Note: "填写服务地址与音色；播报适配尚未接入。"},
		},
		"external": {{ID: "http_json", Name: "HTTP Agent 接口", Note: "填写支持喜悦 HTTP 接口的外部 Agent 地址；普通网页地址不能直接使用。"}},
	}
}
func defaultSettings() SystemConfig {
	return SystemConfig{Revision: 1, Name: "喜悦音箱", WakeWord: "喜悦", Timezone: "Asia/Shanghai", Mode: "direct", LLM: ServiceConfig{Provider: "deepseek", Endpoint: "https://api.deepseek.com", Model: "deepseek-flash"}, ASR: ServiceConfig{Provider: "doubao"}, TTS: ServiceConfig{Provider: "system"}, External: ServiceConfig{Provider: "http_json"}}
}
func (p *Platform) publicState() State { s := p.snapshot(); s.Sealed = nil; return s }

func validateEndpoint(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || len(raw) > 2000 {
		return errors.New("服务地址必须是完整 URL，不能包含账号、查询参数或片段")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return errors.New("服务地址仅支持 HTTP 或 HTTPS")
	}
	return nil
}
func (p *Platform) secretCipher(create bool) (cipher.AEAD, error) {
	path := p.path + ".key"
	key, err := os.ReadFile(path)
	if os.IsNotExist(err) && create {
		key = make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return nil, errors.New("无法初始化凭证存储")
		}
		f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return nil, errors.New("无法初始化凭证存储")
		}
		_, err = f.Write(key)
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
	}
	if err != nil || len(key) != 32 {
		return nil, errors.New("凭证存储不可用；原凭证保持不变")
	}
	if info, e := os.Stat(path); e != nil || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("凭证存储权限不正确")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errors.New("凭证存储不可用")
	}
	return cipher.NewGCM(block)
}
func (p *Platform) seal(name, secret string) (string, error) {
	if len(secret) > 8192 || strings.TrimSpace(secret) == "" || strings.ContainsAny(secret, "\r\n") {
		return "", errors.New("密钥为空、过长或包含换行")
	}
	aead, err := p.secretCipher(true)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return "", errors.New("无法保存凭证")
	}
	return base64.StdEncoding.EncodeToString(aead.Seal(nonce, nonce, []byte(secret), []byte(name))), nil
}
func (p *Platform) unseal(name, sealed string) (string, error) {
	aead, err := p.secretCipher(false)
	if err != nil {
		return "", err
	}
	data, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil || len(data) < aead.NonceSize() {
		return "", errors.New("凭证无法读取")
	}
	plain, err := aead.Open(nil, data[:aead.NonceSize()], data[aead.NonceSize():], []byte(name))
	if err != nil {
		return "", errors.New("凭证无法读取")
	}
	return string(plain), nil
}
func (p *Platform) saveSettings(in SettingsInput) error {
	if strings.TrimSpace(in.Name) == "" || len(in.Name) > 120 || strings.TrimSpace(in.WakeWord) == "" || len(in.WakeWord) > 48 {
		return errors.New("请填写有效的设备名称与唤醒词")
	}
	if _, err := time.LoadLocation(in.Timezone); err != nil {
		return errors.New("时区无效")
	}
	if in.Mode != "direct" && in.Mode != "external" && in.Mode != "hybrid" {
		return errors.New("运行模式无效")
	}
	return p.change(func(s *State) error {
		if in.Revision != s.Settings.Revision {
			return errors.New("配置已被其他页面更新，请重新加载后保存")
		}
		if s.Sealed == nil {
			s.Sealed = map[string]string{}
		}
		next := SystemConfig{Revision: in.Revision + 1, Name: strings.TrimSpace(in.Name), WakeWord: strings.TrimSpace(in.WakeWord), Timezone: in.Timezone, Mode: in.Mode}
		for name, input := range map[string]ServiceInput{"asr": in.ASR, "llm": in.LLM, "tts": in.TTS, "external": in.External} {
			known := false
			for _, option := range providers()[name] {
				if option.ID == input.Provider {
					known = true
				}
			}
			if !known || len(input.Model) > 200 {
				return errors.New("服务商或模型配置无效")
			}
			input.Endpoint = strings.TrimRight(strings.TrimSpace(input.Endpoint), "/")
			if err := validateEndpoint(input.Endpoint); err != nil {
				return err
			}
			old := map[string]ServiceConfig{"asr": s.Settings.ASR, "llm": s.Settings.LLM, "tts": s.Settings.TTS, "external": s.Settings.External}[name]
			secretName := "service:" + name
			if input.ClearKey {
				delete(s.Sealed, secretName)
			}
			if (input.Endpoint != old.Endpoint || input.Provider != old.Provider) && s.Sealed[secretName] != "" && input.APIKey == "" {
				return errors.New("更换服务商或地址时，请重新填写密钥，或勾选清除旧密钥")
			}
			if input.APIKey != "" {
				sealed, err := p.seal(secretName, input.APIKey)
				if err != nil {
					return err
				}
				s.Sealed[secretName] = sealed
			}
			config := ServiceConfig{Provider: input.Provider, Endpoint: input.Endpoint, Model: strings.TrimSpace(input.Model), KeyConfigured: s.Sealed[secretName] != ""}
			switch name {
			case "asr":
				next.ASR = config
			case "llm":
				next.LLM = config
			case "tts":
				next.TTS = config
			case "external":
				next.External = config
			}
		}
		now := time.Now().UTC()
		next.UpdatedAt = &now
		s.Settings = next
		return nil
	})
}

type UsageRecord struct {
	ConfigRevision int       `json:"config_revision"`
	ID             string    `json:"id"`
	Service        string    `json:"service"`
	Provider       string    `json:"provider"`
	Model          string    `json:"model"`
	At             time.Time `json:"at"`
	Success        bool      `json:"success"`
	DurationMS     int64     `json:"duration_ms"`
	InputTokens    *int64    `json:"input_tokens,omitempty"`
	OutputTokens   *int64    `json:"output_tokens,omitempty"`
	ErrorCode      string    `json:"error_code,omitempty"`
}

// No provider requests are made automatically: preview is an explicit user action.
func (p *Platform) chat(ctx context.Context, message string) (string, error) {
	if strings.TrimSpace(message) == "" || len(message) > 16000 {
		return "", errors.New("请输入不超过16000字节的文字")
	}
	s := p.snapshot()
	config := s.Settings.LLM
	service := "llm"
	if s.Settings.Mode == "hybrid" {
		return "", errors.New("混合模式编排尚未接入，请选择直连模型或外部 Agent 进行文字验证")
	}
	if s.Settings.Mode == "external" {
		config = s.Settings.External
		service = "external"
	}
	if config.Endpoint == "" || (service == "llm" && config.Model == "") {
		return "", errors.New("请先保存完整的服务地址与模型配置")
	}
	endpoint := config.Endpoint
	var payload any = map[string]string{"message": message}
	if service == "llm" {
		endpoint += "/chat/completions"
		payload = map[string]any{"model": config.Model, "stream": false, "max_tokens": 512, "messages": []map[string]string{{"role": "user", "content": message}}}
	}
	var secret string
	if sealed := s.Sealed["service:"+service]; sealed != "" {
		var err error
		secret, err = p.unseal("service:"+service, sealed)
		if err != nil {
			return "", err
		}
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", errors.New("服务地址无效")
	}
	if secret != "" && u.Scheme != "https" && u.Hostname() != "localhost" && !net.ParseIP(u.Hostname()).IsLoopback() {
		return "", errors.New("带密钥的连接需要 HTTPS；HTTP 仅允许本机开发服务")
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return "", errors.New("无法创建请求")
	}
	req.Header.Set("Content-Type", "application/json")
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	usage := UsageRecord{ConfigRevision: s.Settings.Revision, ID: newID(), Service: service, Provider: config.Provider, Model: config.Model, At: time.Now().UTC()}
	start := time.Now()
	client := &http.Client{Timeout: 45 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, requestErr := client.Do(req)
	var reply string
	if requestErr != nil {
		usage.ErrorCode = "connection_failed"
	} else {
		defer response.Body.Close()
		if response.StatusCode != 200 {
			usage.ErrorCode = fmt.Sprintf("http_%d", response.StatusCode)
		} else {
			data, e := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
			if e != nil || len(data) > 1<<20 {
				usage.ErrorCode = "invalid_response"
			} else {
				var result struct {
					Reply   string `json:"reply"`
					Choices []struct {
						Message struct {
							Content string `json:"content"`
						} `json:"message"`
					} `json:"choices"`
					Usage *struct {
						Input  *int64 `json:"prompt_tokens"`
						Output *int64 `json:"completion_tokens"`
					} `json:"usage"`
				}
				if e = json.Unmarshal(data, &result); e != nil {
					usage.ErrorCode = "invalid_response"
				} else {
					if service == "external" {
						reply = result.Reply
					} else if len(result.Choices) > 0 {
						reply = result.Choices[0].Message.Content
					}
					if result.Usage != nil {
						usage.InputTokens = result.Usage.Input
						usage.OutputTokens = result.Usage.Output
					}
					if strings.TrimSpace(reply) == "" {
						usage.ErrorCode = "empty_response"
					}
				}
			}
		}
	}
	usage.DurationMS = time.Since(start).Milliseconds()
	usage.Success = usage.ErrorCode == ""
	if err = p.change(func(s *State) error { s.Usage = append(s.Usage, usage); return nil }); err != nil {
		return "", errors.New("调用结束，但用量记录未能保存，请检查存储")
	}
	if !usage.Success {
		return "", fmt.Errorf("服务调用失败（%s）；请检查地址、密钥、模型和服务额度", usage.ErrorCode)
	}
	return reply, nil
}

var bootTime = time.Now()

func (a *api) settingsRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/settings", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, a.platform.snapshot().Settings) })
	m.HandleFunc("GET /api/providers", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, providers()) })
	m.HandleFunc("PUT /api/settings", func(w http.ResponseWriter, r *http.Request) {
		var in SettingsInput
		if err := decode(w, r, &in); err != nil {
			bad(w, err)
			return
		}
		if err := a.platform.saveSettings(in); err != nil {
			bad(w, err)
			return
		}
		writeJSON(w, 200, a.platform.snapshot().Settings)
	})
	m.HandleFunc("POST /api/preview/chat", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Message string `json:"message"`
		}
		if err := decode(w, r, &in); err != nil {
			bad(w, err)
			return
		}
		reply, err := a.platform.chat(r.Context(), in.Message)
		if err != nil {
			bad(w, err)
			return
		}
		writeJSON(w, 200, map[string]string{"reply": reply})
	})
	m.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		s := a.platform.publicState()
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		writeJSON(w, 200, map[string]any{"version": "0.2.0", "environment": "development", "device_connected": false, "voice_connected": false, "uptime_seconds": int(time.Since(bootTime).Seconds()), "memory_bytes": mem.Alloc, "platform": runtime.GOOS + "/" + runtime.GOARCH, "config_revision": s.Settings.Revision, "services": map[string]any{"asr": map[string]string{"runtime": "not_integrated"}, "llm": map[string]string{"runtime": "text_preview"}, "tts": map[string]string{"runtime": "not_integrated"}, "external": map[string]string{"runtime": "http_json_preview"}}, "usage": s.Usage})
	})
}
