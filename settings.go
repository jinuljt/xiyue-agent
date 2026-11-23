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
	DeviceSetupComplete bool          `json:"device_setup_complete"`
	Revision            int           `json:"revision"`
	SetupComplete       bool          `json:"setup_complete"`
	Name                string        `json:"name"`
	WakeWord            string        `json:"wake_word"`
	Timezone            string        `json:"timezone"`
	Mode                string        `json:"mode"`
	ASR                 ServiceConfig `json:"asr"`
	LLM                 ServiceConfig `json:"llm"`
	TTS                 ServiceConfig `json:"tts"`
	External            ServiceConfig `json:"external"`
	UpdatedAt           *time.Time    `json:"updated_at,omitempty"`
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
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Endpoint string   `json:"endpoint"`
	Model    string   `json:"model"`
	Note     string   `json:"note"`
	Models   []string `json:"models,omitempty"`
}

func providers() map[string][]Provider {
	return map[string][]Provider{
		"llm": {
			{ID: "deepseek", Name: "DeepSeek", Endpoint: "https://api.deepseek.com", Model: "deepseek-flash", Models: []string{"deepseek-flash", "deepseek-v4-pro"}, Note: "在 DeepSeek 开放平台创建 API Key。"},
			{ID: "doubao", Name: "火山方舟（豆包）", Endpoint: "https://ark.cn-beijing.volces.com/api/v3", Models: []string{"doubao-seed-2-1-lite-260915", "doubao-seed-2-1-pro-260915", "doubao-seed-2-1-turbo-260628", "doubao-seed-evolving", "doubao-seed-2-0-lite-260428"}, Note: "在方舟开通模型并创建 API Key。可填模型 ID 或接入点 ID。"},
			{ID: "qwen", Name: "阿里云百炼（通义千问）", Endpoint: "https://dashscope.aliyuncs.com/compatible-mode/v1", Models: []string{"qwen3.8-flash", "qwen3.7-plus", "qwen4-max"}, Note: "在百炼创建 API Key，并确认服务地址与密钥所属地域一致。"},
			{ID: "kimi", Name: "月之暗面（Kimi）", Endpoint: "https://api.moonshot.cn/v1", Models: []string{"kimi-k3", "kimi-k3.1-pro"}, Note: "在 Kimi 开放平台创建 API Key。"},
			{ID: "glm", Name: "智谱（GLM）", Endpoint: "https://open.bigmodel.cn/api/paas/v4", Models: []string{"glm-5.3", "glm-5.3-flash", "glm-5.3-flashx", "glm-5.3-flashx-legal"}, Note: "在智谱开放平台创建 API Key。编程套餐使用专属地址，可用模型以套餐为准。"},
			{ID: "minimax", Name: "MiniMax", Endpoint: "https://api.minimax.cn/v1", Models: []string{"MiniMax-M3.1-Flash", "MiniMax-M3"}, Note: "在 MiniMax 开放平台创建 API Key，使用兼容 Chat Completions 的地址。"},
			{ID: "hunyuan", Name: "腾讯混元（TokenHub）", Endpoint: "https://api.hunyuan.cloud.tencent.com/v1", Models: []string{"hy4-flash", "hy4", "hy3"}, Note: "在腾讯云 TokenHub 创建 API Key，并从模型广场复制调用名。"},
			{ID: "openai", Name: "OpenAI", Endpoint: "https://api.openai.com/v1", Models: []string{"gpt-6.2-sol", "gpt-6.2-astra", "gpt-6.1-terra", "gpt-6-luna"}, Note: "在 OpenAI API 平台为项目创建 API Key，并确认模型访问权限。"},
			{ID: "anthropic", Name: "Anthropic（Claude）", Endpoint: "https://api.anthropic.com/v1", Models: []string{"claude-sonnet-5-5", "claude-haiku-5-5", "claude-opus-5-5", "claude-fable-5-2"}, Note: "在 Claude Console 创建 API Key。喜悦调用 Messages 接口。"},
			{ID: "gemini", Name: "Google（Gemini）", Endpoint: "https://generativelanguage.googleapis.com/v1beta/openai", Models: []string{"gemini-4-hydrogen", "gemini-4-neon", "gemini-4-carbon"}, Note: "在 Google AI Studio 创建 API Key。喜悦使用 Gemini 的 OpenAI 兼容接口。"},
			{ID: "custom", Name: "自定义 · OpenAI Chat Completions", Note: "填写兼容接口的 Base URL 和模型 ID。"},
			{ID: "custom_anthropic", Name: "自定义 · Anthropic Messages", Note: "填写兼容 Messages 的 Base URL 和模型 ID。"},
		},
		"asr": {
			{ID: "doubao", Name: "豆包实时语音识别", Model: doubaoASRResource, Models: []string{doubaoASRResource}, Note: "开通豆包流式语音识别 2.0 小时版并填写火山引擎 API Key。"},
			{ID: "sensevoice", Name: "自建 SenseVoice", Note: "填写你部署的 SenseVoice 服务地址。"},
			{ID: "custom", Name: "其他语音识别服务", Note: "保存服务地址与接入凭证，协议适配需另行接入。"},
		},
		"tts": {
			{ID: "system", Name: "音箱系统语音（推荐）", Note: "使用音箱系统语音服务，无需额外密钥。"},
			{ID: "doubao", Name: "豆包语音合成", Model: doubaoDefaultVoice, Note: "输入音色 ID 和豆包语音 API Key。"},
			{ID: "custom", Name: "其他语音合成服务", Note: "填写服务地址与音色；播报适配尚未接入。"},
		},
		"external": {{ID: "http_json", Name: "HTTP Agent 接口", Note: "填写支持喜悦 HTTP 接口的外部 Agent 地址；普通网页地址不能直接使用。"}},
	}
}
func defaultSettings() SystemConfig {
	return SystemConfig{Revision: 1, Name: "喜悦音箱", WakeWord: "喜悦", Timezone: "Asia/Shanghai", Mode: "direct", LLM: ServiceConfig{Provider: "deepseek", Endpoint: "https://api.deepseek.com", Model: "deepseek-flash"}, ASR: ServiceConfig{Provider: "doubao"}, TTS: ServiceConfig{Provider: "system"}, External: ServiceConfig{Provider: "http_json"}}
}
func (p *Platform) publicState() State {
	s := p.snapshot()
	s.Sealed = nil
	s.Owner = nil
	s.ExternalTasks = nil
	s.RoomAudio = nil
	return s
}

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
func validateServiceKey(secret string) error {
	if len(secret) > 8192 || strings.TrimSpace(secret) == "" || strings.ContainsAny(secret, "\r\n") {
		return errors.New("密钥为空、过长或包含换行")
	}
	return nil
}

func (p *Platform) serviceKey(s State, service string, temporary *string) (string, error) {
	if temporary != nil {
		return *temporary, nil
	}
	name := "service:" + service
	if sealed := s.Sealed[name]; sealed != "" {
		return p.unseal(name, sealed)
	}
	return "", nil
}

func normalizeServiceInput(name string, input ServiceInput) (ServiceConfig, error) {
	known := false
	for _, option := range providers()[name] {
		if option.ID == input.Provider {
			known = true
			break
		}
	}
	if !known || len(input.Model) > 200 {
		return ServiceConfig{}, errors.New("服务商或模型配置无效")
	}
	endpoint := strings.TrimRight(strings.TrimSpace(input.Endpoint), "/")
	if err := validateEndpoint(endpoint); err != nil {
		return ServiceConfig{}, err
	}
	model := strings.TrimSpace(input.Model)
	if name == "asr" && input.Provider == "doubao" {
		model = doubaoASRResource
	}
	return ServiceConfig{Provider: input.Provider, Endpoint: endpoint, Model: model}, nil
}

func (p *Platform) seal(name, secret string) (string, error) {
	if err := validateServiceKey(secret); err != nil {
		return "", err
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
	if p.wakeWords != nil && !p.wakeWords[strings.TrimSpace(in.WakeWord)] {
		return errors.New("本机还没有这个唤醒词的识别配置，请保留当前唤醒词")
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
		next := SystemConfig{SetupComplete: s.Settings.SetupComplete, DeviceSetupComplete: s.Settings.DeviceSetupComplete, Revision: in.Revision + 1, Name: strings.TrimSpace(in.Name), WakeWord: strings.TrimSpace(in.WakeWord), Timezone: in.Timezone, Mode: in.Mode}
		for name, input := range map[string]ServiceInput{"asr": in.ASR, "llm": in.LLM, "tts": in.TTS, "external": in.External} {
			config, err := normalizeServiceInput(name, input)
			if err != nil {
				return err
			}
			old := map[string]ServiceConfig{"asr": s.Settings.ASR, "llm": s.Settings.LLM, "tts": s.Settings.TTS, "external": s.Settings.External}[name]
			secretName := "service:" + name
			if input.ClearKey {
				delete(s.Sealed, secretName)
			}
			if (config.Endpoint != old.Endpoint || config.Provider != old.Provider) && s.Sealed[secretName] != "" && input.APIKey == "" {
				return errors.New("更换服务商或地址时，请重新填写密钥，或勾选清除旧密钥")
			}
			if input.APIKey != "" {
				sealed, err := p.seal(secretName, input.APIKey)
				if err != nil {
					return err
				}
				s.Sealed[secretName] = sealed
			}
			config.KeyConfigured = s.Sealed[secretName] != ""
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
	ConfigRevision    int        `json:"config_revision"`
	ID                string     `json:"id"`
	Service           string     `json:"service"`
	Provider          string     `json:"provider"`
	Model             string     `json:"model"`
	At                time.Time  `json:"at"`
	Success           bool       `json:"success"`
	DurationMS        int64      `json:"duration_ms"`
	InputTokens       *int64     `json:"input_tokens,omitempty"`
	OutputTokens      *int64     `json:"output_tokens,omitempty"`
	ErrorCode         string     `json:"error_code,omitempty"`
	Day               string     `json:"day,omitempty"`
	AudioMS           *int64     `json:"audio_ms,omitempty"`
	Characters        *int64     `json:"characters,omitempty"`
	CachedInputTokens *int64     `json:"cached_input_tokens,omitempty"`
	CacheWriteTokens  *int64     `json:"cache_write_tokens,omitempty"`
	Test              bool       `json:"test,omitempty"`
	Estimated         *float64   `json:"estimated,omitempty"`
	Currency          string     `json:"currency,omitempty"`
	PriceAt           *time.Time `json:"price_at,omitempty"`
	PriceSource       string     `json:"price_source,omitempty"`
}

// No provider requests are made automatically: preview is an explicit user action.
func (p *Platform) chat(ctx context.Context, message string) (string, error) {
	if strings.TrimSpace(message) == "" || len(message) > 16000 {
		return "", errors.New("请输入不超过16000字节的文字")
	}
	return p.respond(ctx, p.snapshot(), message, true)
}

type replyOptions struct {
	markTextSetup bool
	transientKey  *string
	draft         bool
	history       []voiceMessage
	voicePlan     *voicePlan
	question      *Event
}

type voiceMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func (p *Platform) respond(ctx context.Context, s State, message string, markTextSetup bool) (string, error) {
	return p.requestReply(ctx, s, message, replyOptions{markTextSetup: markTextSetup})
}

func (p *Platform) requestReply(ctx context.Context, s State, message string, options replyOptions) (string, error) {
	config := s.Settings.LLM
	service := "llm"
	useExternal := s.Settings.Mode == "external"
	if s.Settings.Mode == "hybrid" {
		if task, ok := strings.CutPrefix(strings.TrimSpace(message), "交给外部助手"); ok {
			message = strings.TrimLeft(strings.TrimSpace(task), "：:，,。 ")
			if message == "" {
				return "", errors.New("请说出要交给外部助手的任务")
			}
			useExternal = true
		}
	}
	if useExternal {
		config = s.Settings.External
		service = "external"
	}
	if useExternal && config.Endpoint == "" {
		return "", errors.New("请填写外部助手的连接地址")
	}
	if !useExternal && (config.Endpoint == "" || config.Model == "") {
		return "", errors.New("请填写服务地址和模型 ID")
	}
	endpoint := config.Endpoint
	var payload any = map[string]string{"message": message}
	anthropic := service == "llm" && (config.Provider == "anthropic" || config.Provider == "custom_anthropic")
	if service == "llm" {
		path := "/chat/completions"
		history := append([]voiceMessage{}, options.history...)
		instructions := speakerInstructions
		if options.question != nil {
			instructions += "\n最后一条 assistant 消息是当前流程播报的待确认问题。只把它作为待确认的内容。根据用户实际意图，只有明确同意执行该步骤时返回 next_step。拒绝、含糊、单纯讨论或无关请求保持该步骤等待。"
			history = append(history, voiceMessage{"assistant", options.question.Text})
		}
		if options.voicePlan != nil && !anthropic {
			history = append([]voiceMessage{{"system", instructions}}, history...)
		}
		params := map[string]any{"model": config.Model, "messages": append(history, voiceMessage{"user", message})}
		if anthropic {
			path = "/messages"
			params["max_tokens"] = 512
			if options.voicePlan != nil {
				params["system"] = instructions
			}
		} else if config.Provider == "openai" {
			params["max_completion_tokens"] = 512
		} else {
			params["max_tokens"] = 512
		}
		endpoint += path
		payload = params
	}
	secret, err := p.serviceKey(s, service, options.transientKey)
	if err != nil {
		return "", err
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
	if anthropic {
		req.Header.Set("anthropic-version", "2023-06-01")
		if secret != "" {
			req.Header.Set("x-api-key", secret)
		}
	} else if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	usage := UsageRecord{ConfigRevision: s.Settings.Revision, ID: newID(), Service: service, Provider: config.Provider, Model: config.Model, At: time.Now().UTC(), Test: options.markTextSetup}
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
					Content []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"content"`
					Reply   string `json:"reply"`
					Choices []struct {
						Message struct {
							Content string `json:"content"`
						} `json:"message"`
					} `json:"choices"`
					Usage *struct {
						Input        *int64 `json:"prompt_tokens"`
						Output       *int64 `json:"completion_tokens"`
						NativeInput  *int64 `json:"input_tokens"`
						NativeOutput *int64 `json:"output_tokens"`
						Cached       *int64 `json:"prompt_cache_hit_tokens"`
						CacheRead    *int64 `json:"cache_read_input_tokens"`
						CacheWrite   *int64 `json:"cache_creation_input_tokens"`
						Details      struct {
							Cached *int64 `json:"cached_tokens"`
						} `json:"prompt_tokens_details"`
					} `json:"usage"`
				}
				if e = json.Unmarshal(data, &result); e != nil {
					usage.ErrorCode = "invalid_response"
				} else {
					if service == "external" {
						reply = result.Reply
					} else if anthropic {
						for _, part := range result.Content {
							if part.Type == "text" {
								reply += part.Text
							}
						}
					} else if len(result.Choices) > 0 {
						reply = result.Choices[0].Message.Content
					}
					if result.Usage != nil {
						usage.InputTokens = result.Usage.Input
						usage.OutputTokens = result.Usage.Output
						usage.CachedInputTokens = result.Usage.Cached
						if usage.CachedInputTokens == nil {
							usage.CachedInputTokens = result.Usage.Details.Cached
						}
						if anthropic {
							usage.InputTokens, usage.OutputTokens = result.Usage.NativeInput, result.Usage.NativeOutput
							usage.CachedInputTokens, usage.CacheWriteTokens = result.Usage.CacheRead, result.Usage.CacheWrite
							if usage.InputTokens != nil {
								total := *usage.InputTokens
								if usage.CachedInputTokens != nil {
									total += *usage.CachedInputTokens
								}
								if usage.CacheWriteTokens != nil {
									total += *usage.CacheWriteTokens
								}
								usage.InputTokens = &total
							}
						}
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
	p.activity.add(usage, s.Settings.Timezone)
	if !options.draft && options.markTextSetup && usage.Success {
		if err = p.change(func(s *State) error {
			if s.Settings.Revision == usage.ConfigRevision {
				s.Settings.SetupComplete = true
			}
			return nil
		}); err != nil {
			return "", errors.New("调用结束，但用量记录未能保存，请检查存储")
		}
	}
	if !usage.Success {
		return "", fmt.Errorf("服务调用失败（%s）；请检查地址、密钥、模型和服务额度", usage.ErrorCode)
	}
	if service == "llm" && options.voicePlan != nil {
		plan, err := parseVoicePlan(reply)
		if err != nil {
			plan = voicePlan{Action: "speak", Speech: "我没听明白，请再说一遍。"}
		}
		*options.voicePlan = plan
	}
	return reply, nil
}

type serviceStatus struct {
	Provider string `json:"provider"`
	State    string `json:"state"`
}

func (a *api) serviceStatus(s State) map[string]serviceStatus {
	config := s.Settings
	status := func(provider string, ready bool) serviceStatus {
		state := "not_configured"
		if ready {
			state = "configured"
		}
		return serviceStatus{Provider: provider, State: state}
	}
	services := map[string]serviceStatus{
		"asr": status(config.ASR.Provider, asrReady(config.ASR)),
		"tts": status(config.TTS.Provider, ttsReady(a.voice, s)),
		"llm": status(config.LLM.Provider, config.LLM.Endpoint != "" && config.LLM.Model != "" &&
			(config.LLM.KeyConfigured || strings.HasPrefix(config.LLM.Provider, "custom"))),
		"external": status(config.External.Provider, config.External.Endpoint != ""),
	}
	if a.voice == nil {
		services["asr"] = serviceStatus{Provider: config.ASR.Provider, State: "disabled"}
		services["tts"] = serviceStatus{Provider: config.TTS.Provider, State: "disabled"}
	} else {
		a.voice.mu.Lock()
		if turn := a.voice.last; turn != nil && turn.Revision == config.Revision {
			switch turn.Stage {
			case "completed", "playback", "model", "local_task", "confirmed_on_device":
				services["asr"] = serviceStatus{Provider: config.ASR.Provider, State: "succeeded"}
			case "asr":
				services["asr"] = serviceStatus{Provider: config.ASR.Provider, State: "failed"}
			}
			switch turn.Stage {
			case "completed":
				services["tts"] = serviceStatus{Provider: config.TTS.Provider, State: "succeeded"}
			case "playback":
				services["tts"] = serviceStatus{Provider: config.TTS.Provider, State: "failed"}
			}
		}
		a.voice.mu.Unlock()
	}
	for i := len(s.Usage) - 1; i >= 0; i-- {
		entry := s.Usage[i]
		current := services[entry.Service]
		if entry.ConfigRevision != config.Revision || (entry.Service != "llm" && entry.Service != "external") ||
			(current.State != "configured" && current.State != "not_configured") {
			continue
		}
		if entry.Success {
			current.State = "succeeded"
		} else {
			current.State = "failed"
		}
		services[entry.Service] = current
	}
	return services
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
	m.HandleFunc("POST /api/settings/verify", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Service string        `json:"service"`
			Config  *ServiceInput `json:"config,omitempty"`
		}
		if err := decode(w, r, &in); err != nil {
			bad(w, err)
			return
		}
		s := a.platform.snapshot()
		var selected *ServiceConfig
		switch in.Service {
		case "llm":
			s.Settings.Mode = "direct"
			selected = &s.Settings.LLM
		case "external":
			s.Settings.Mode = "external"
			selected = &s.Settings.External
		case "asr":
			selected = &s.Settings.ASR
		case "tts":
			selected = &s.Settings.TTS
		default:
			bad(w, errors.New("未知验证服务"))
			return
		}
		options := replyOptions{markTextSetup: true}
		if in.Config != nil {
			current := *selected
			candidate, err := normalizeServiceInput(in.Service, *in.Config)
			if err != nil {
				bad(w, err)
				return
			}
			key := in.Config.APIKey
			if key != "" {
				if err := validateServiceKey(key); err != nil {
					bad(w, err)
					return
				}
			}
			sameTarget := candidate.Provider == current.Provider && candidate.Endpoint == current.Endpoint
			if key != "" || in.Config.ClearKey || !sameTarget {
				options.transientKey = &key
			}
			candidate.KeyConfigured = key != "" || (options.transientKey == nil && current.KeyConfigured)
			options.draft = candidate != current || options.transientKey != nil
			*selected = candidate
		}
		if in.Service == "asr" || in.Service == "tts" {
			a.verifySpeech(w, r, s, in.Service, options.transientKey)
			return
		}
		start := time.Now()
		if _, err := a.platform.requestReply(r.Context(), s, "这是连接验证，请只回复 OK。", options); err != nil {
			bad(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "duration_ms": time.Since(start).Milliseconds()})
	})
	m.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		s := a.platform.publicState()
		s.Usage = a.platform.activity.recent()
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		writeJSON(w, 200, map[string]any{
			"version":        "0.2.0",
			"uptime_seconds": int(time.Since(bootTime).Seconds()), "memory_bytes": mem.Alloc,
			"platform": runtime.GOOS + "/" + runtime.GOARCH, "config_revision": s.Settings.Revision,
			"device": map[string]string{"model": a.deviceModel, "firmware": a.deviceFirmware,
				"name": map[string]string{"OH2P": "Xiaomi 智能音箱 Pro"}[strings.ToUpper(a.deviceModel)]},
			"services": a.serviceStatus(s), "voice": a.voice.status(),
		})
	})
}
