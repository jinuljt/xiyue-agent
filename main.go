package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"
)

type api struct {
	platform *Platform
	assets   map[string][]byte
	voice    *voiceRuntime
	admin    *adminGate
}

func decode(w http.ResponseWriter, r *http.Request, out any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 128<<10)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return errors.New("请求格式不正确或超过128KB")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("请求只能包含一个JSON对象")
	}
	return nil
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func bad(w http.ResponseWriter, err error) {
	writeJSON(w, 400, map[string]string{"error": err.Error()})
}
func (a *api) routes() http.Handler {
	m := http.NewServeMux()
	a.settingsRoutes(m)
	a.voiceRoutes(m)
	a.adminRoutes(m)
	a.externalTaskRoutes(m)
	m.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, a.platform.publicState()) })
	m.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"name": "喜悦", "version": "0.2.0", "status": "running",
			"services": a.serviceStatus(a.platform.publicState())})
	})
	m.HandleFunc("GET /api/tools", func(w http.ResponseWriter, r *http.Request) {
		// The same manifests drive console forms and an eventual model tool adapter.
		tools := []Manifest{}
		for _, ability := range a.platform.snapshot().Abilities {
			if ability.Enabled {
				tools = append(tools, ability.Manifest)
			}
		}
		writeJSON(w, 200, tools)
	})
	m.HandleFunc("POST /api/abilities", func(w http.ResponseWriter, r *http.Request) {
		var manifest Manifest
		if err := decode(w, r, &manifest); err != nil {
			bad(w, err)
			return
		}
		if err := validateManifest(manifest); err != nil {
			bad(w, err)
			return
		}
		err := a.platform.change(func(s *State) error {
			if _, ok := s.Abilities[manifest.ID]; ok {
				return errors.New("同名能力已存在；首版不支持覆盖安装")
			}
			if len(s.Abilities) >= 32 {
				return errors.New("能力数量不能超过32个")
			}
			s.Abilities[manifest.ID] = Ability{Manifest: manifest, Enabled: false, Config: map[string]string{}}
			return nil
		})
		if err != nil {
			bad(w, err)
			return
		}
		writeJSON(w, 201, map[string]string{"id": manifest.ID})
	})
	m.HandleFunc("PUT /api/abilities/{id}", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Enabled      bool              `json:"enabled"`
			Config       map[string]string `json:"config"`
			Secrets      map[string]string `json:"secrets"`
			ClearSecrets []string          `json:"clear_secrets"`
		}
		if err := decode(w, r, &in); err != nil {
			bad(w, err)
			return
		}
		err := a.platform.change(func(s *State) error {
			ability, ok := s.Abilities[r.PathValue("id")]
			if !ok {
				return errors.New("能力不存在")
			}
			if in.Config == nil {
				in.Config = map[string]string{}
			}
			known := map[string]Field{}
			for _, f := range ability.Manifest.Fields {
				known[f.Key] = f
			}
			for k, v := range in.Config {
				f, exists := known[k]
				if !exists || len(v) > 2000 {
					return errors.New("未知配置项或内容过长")
				}
				if f.Kind == "secret" {
					return errors.New("密钥必须通过凭证字段保存，不能写入普通配置")
				}
				if f.Kind == "credential_ref" && v != "" && !credentialRef.MatchString(v) {
					return errors.New("请填写 bitwarden:item:条目UUID，不接受明文密钥")
				}
			}
			if s.Sealed == nil {
				s.Sealed = map[string]string{}
			}
			for _, k := range in.ClearSecrets {
				if known[k].Kind != "secret" {
					return errors.New("未知凭证字段")
				}
				delete(s.Sealed, "ability:"+r.PathValue("id")+":"+k)
			}
			for k, v := range in.Secrets {
				if known[k].Kind != "secret" {
					return errors.New("未知凭证字段")
				}
				if v != "" {
					sealed, err := a.platform.seal("ability:"+r.PathValue("id")+":"+k, v)
					if err != nil {
						return err
					}
					s.Sealed["ability:"+r.PathValue("id")+":"+k] = sealed
				}
			}
			for k, f := range known {
				if f.Kind == "secret" && s.Sealed["ability:"+r.PathValue("id")+":"+k] != "" {
					in.Config[k] = "已配置"
				}
			}
			ability.Config = in.Config
			ability.Enabled = in.Enabled
			// Required task inputs can be supplied at invocation, not necessarily at installation.
			s.Abilities[r.PathValue("id")] = ability
			return nil
		})
		if err != nil {
			bad(w, err)
			return
		}
		writeJSON(w, 200, map[string]bool{"saved": true})
	})
	m.HandleFunc("POST /api/tasks", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			AbilityID string            `json:"ability_id"`
			Inputs    map[string]string `json:"inputs"`
		}
		if err := decode(w, r, &in); err != nil {
			bad(w, err)
			return
		}
		var id string
		err := a.platform.change(func(s *State) error {
			var err error
			id, err = startTask(s, in.AbilityID, in.Inputs, time.Now().UTC())
			return err
		})
		if err != nil {
			bad(w, err)
			return
		}
		writeJSON(w, 201, map[string]string{"id": id})
	})
	m.HandleFunc("POST /api/tasks/{id}/{action}", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Index  *int   `json:"index"`
			Status string `json:"status"`
		}
		if err := decode(w, r, &in); err != nil {
			bad(w, err)
			return
		}
		err := a.platform.change(func(s *State) error {
			for _, t := range s.Tasks {
				if t.ID == r.PathValue("id") && (in.Index == nil || *in.Index != t.Index || in.Status != t.Status) {
					return errors.New("任务状态已变化，请刷新后重试")
				}
			}
			return taskAction(s, r.PathValue("id"), r.PathValue("action"), time.Now().UTC())
		})
		if err != nil {
			bad(w, err)
			return
		}
		writeJSON(w, 200, map[string]bool{"saved": true})
	})
	m.HandleFunc("POST /api/schedules", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			AbilityID string            `json:"ability_id"`
			Inputs    map[string]string `json:"inputs"`
			NextAt    time.Time         `json:"next_at"`
			Repeat    string            `json:"repeat"`
			Timezone  string            `json:"timezone"`
		}
		if err := decode(w, r, &in); err != nil {
			bad(w, err)
			return
		}
		sc := Schedule{ID: newID(), AbilityID: in.AbilityID, Inputs: in.Inputs, NextAt: in.NextAt, Repeat: in.Repeat, Timezone: in.Timezone, Enabled: true}
		err := a.platform.change(func(s *State) error {
			if len(s.Schedules) >= 100 {
				return errors.New("定时计划不能超过100条")
			}
			if err := validateSchedule(s, sc, time.Now()); err != nil {
				return err
			}
			s.Schedules = append(s.Schedules, sc)
			return nil
		})
		if err != nil {
			bad(w, err)
			return
		}
		writeJSON(w, 201, map[string]string{"id": sc.ID})
	})
	m.HandleFunc("POST /api/schedules/{id}/retry", func(w http.ResponseWriter, r *http.Request) {
		err := a.platform.change(func(s *State) error {
			return retrySchedule(s, r.PathValue("id"), time.Now().UTC())
		})
		if err != nil {
			bad(w, err)
			return
		}
		writeJSON(w, 200, map[string]bool{"saved": true})
	})
	m.HandleFunc("DELETE /api/schedules/{id}", func(w http.ResponseWriter, r *http.Request) {
		err := a.platform.change(func(s *State) error {
			for i, sc := range s.Schedules {
				if sc.ID == r.PathValue("id") {
					s.Schedules = append(s.Schedules[:i], s.Schedules[i+1:]...)
					return nil
				}
			}
			return errors.New("计划不存在")
		})
		if err != nil {
			bad(w, err)
			return
		}
		writeJSON(w, 200, map[string]bool{"saved": true})
	})
	m.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "/" {
			path = "/index.html"
		}
		data, ok := a.assets[path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		types := map[string]string{"/index.html": "text/html; charset=utf-8", "/help.html": "text/html; charset=utf-8", "/help.js": "text/javascript; charset=utf-8", "/app.js": "text/javascript; charset=utf-8", "/style.css": "text/css; charset=utf-8"}
		w.Header().Set("Content-Type", types[path])
		_, _ = w.Write(data)
	})
	return a.guard(m)
}

var credentialRef = regexpCredential()

func regexpCredential() *regexp.Regexp {
	return regexp.MustCompile(`^bitwarden:item:[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
}

// The development listener stays on loopback. Paired management uses an explicit HTTPS origin.
func (a *api) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		scheme := "http"
		if a.admin != nil {
			scheme = "https"
			if r.TLS == nil || !strings.EqualFold(r.Host, a.admin.host) {
				http.Error(w, "管理服务拒绝此访问地址", 403)
				return
			}
		} else if host != "localhost" && host != "127.0.0.1" && host != "::1" {
			http.Error(w, "本地控制台拒绝此 Host", 403)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			u, e := url.Parse(origin)
			if e != nil || u.Scheme != scheme || !strings.EqualFold(u.Host, r.Host) {
				http.Error(w, "拒绝跨站请求", 403)
				return
			}
		}
		if r.Method != "GET" && r.Method != "HEAD" {
			contentType := strings.TrimSpace(strings.SplitN(r.Header.Get("Content-Type"), ";", 2)[0])
			if r.Header.Get("X-Xiyue-Request") != "console" ||
				(contentType != "application/json" && !(r.Method == http.MethodPost && r.URL.Path == "/api/voice/turn" && contentType == "audio/wav")) {
				http.Error(w, "请求来源或格式无效", 403)
				return
			}
		}
		if a.admin != nil && (r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/")) {
			switch {
			case r.URL.Path == "/api/voice/turn":
				// Real capture arrives on the local, owner-only wake socket.
				http.NotFound(w, r)
				return
			case r.Method == http.MethodGet && r.URL.Path == "/api/session":
			case r.Method == http.MethodPost && (r.URL.Path == "/api/session/pair" || r.URL.Path == "/api/session/login" ||
				r.URL.Path == "/api/session/device" || r.URL.Path == "/api/session/device/claim"):
			case r.Method == http.MethodDelete && r.URL.Path == "/api/session/device":
			case !a.platform.authenticated(r):
				writeJSON(w, 401, map[string]string{"error": "请先配对并登录音箱"})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func main() {
	listen := flag.String("listen", "127.0.0.1:8765", "本地开发监听地址")
	statePath := flag.String("state", "data/state.json", "持久化状态文件")
	consoleDir := flag.String("console", "../xiyue-console", "控制台静态文件目录")
	abilityDir := flag.String("abilities", "../xiyue-abilities/capabilities", "可选的声明式能力目录")
	systemVoice := flag.Bool("system-voice", false, "启用本机原生播报和 WAV 语音入口")
	wakeSocket := flag.String("oh2p-wake-socket", "", "OH2P 本地唤醒事件套接字")
	notifyWake := flag.String("notify-wake", "", "向正在运行的 OH2P Agent 通知唤醒词后退出")
	cancelVoice := flag.Bool("cancel-voice", false, "向正在运行的 OH2P Agent 请求取消当前语音")
	vad := flag.Float64("oh2p-vad-threshold", 1100000, "OH2P 录音 S32 RMS 声音阈值")
	adminHost := flag.String("admin-host", "", "管理 HTTPS 地址中的主机和端口")
	tlsCert := flag.String("admin-tls-cert", "", "受手机信任的 HTTPS 证书路径")
	tlsKey := flag.String("admin-tls-key", "", "HTTPS 私钥路径")
	pairFile := flag.String("admin-pairing-file", "", "仅本机所有者可读写的配对或恢复码文件")
	newAdminCode := flag.Bool("new-admin-code", false, "只为指定的管理码文件生成一次性设置或恢复码")
	buttonInput := flag.String("admin-button-device", "", "音箱确认按键的 Linux evdev 路径")
	buttonKey := flag.Uint("admin-button-key", 0, "已验证用于登录的实体键 EV_KEY 编号")
	buttonLabel := flag.String("admin-button-label", "", "控制台显示的实体按键名称")
	flag.Parse()
	if *newAdminCode {
		if err := writeAdminCode(*pairFile); err != nil {
			log.Fatal(err)
		}
		return
	}
	if *notifyWake != "" || *cancelVoice {
		if *wakeSocket == "" || (*notifyWake != "" && *cancelVoice) || strings.ContainsAny(*notifyWake, "\r\n") {
			log.Fatal("请指定一个本地语音动作及唤醒套接字")
		}
		packet := "wake " + *notifyWake
		if *cancelVoice {
			packet = "cancel"
		}
		if err := sendWakePacket(*wakeSocket, packet); err != nil {
			log.Fatal("无法连接本地喜悦 Agent")
		}
		return
	}
	var voice *voiceRuntime
	if *systemVoice {
		info, e := os.Stat(systemTTS)
		if e != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
			log.Fatal("未找到可执行的音箱系统语音服务")
		}
		voice = &voiceRuntime{speak: speakSystem}
		if _, err := exec.LookPath("aplay"); err == nil {
			voice.playPCM = playALSA
		}
	}
	host, _, err := net.SplitHostPort(*listen)
	if err != nil {
		log.Fatal("监听地址无效")
	}
	var admin *adminGate
	if *adminHost != "" || *tlsCert != "" || *tlsKey != "" || *pairFile != "" {
		name, e := url.Parse("https://" + *adminHost)
		if *tlsCert == "" || *tlsKey == "" || *pairFile == "" || e != nil || name.Host != *adminHost || name.Hostname() == "" || name.User != nil {
			log.Fatal("配对模式需要明确的管理主机、HTTPS 证书、私钥和配对码文件")
		}
		admin = &adminGate{host: *adminHost, codePath: *pairFile}
	} else if host != "127.0.0.1" && host != "::1" {
		log.Fatal("非回环监听必须启用 HTTPS 配对")
	}
	assets := map[string][]byte{}
	for _, name := range []string{"index.html", "app.js", "style.css", "help.html", "help.js"} {
		data, e := os.ReadFile(filepath.Join(*consoleDir, name))
		if errors.Is(e, os.ErrNotExist) && (name == "help.html" || name == "help.js") {
			continue // Older console bundles did not include the guide.
		}
		if e != nil {
			log.Fatalf("控制台资源缺失: %s", name)
		}
		assets["/"+name] = data
	}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	p, err := openPlatform(*statePath)
	if err != nil {
		log.Fatal(err)
	}
	// Install packaged capabilities only when absent; preserve user edits and enablement.
	files, err := filepath.Glob(filepath.Join(*abilityDir, "*.json"))
	if err != nil {
		log.Fatal(err)
	}
	for _, file := range files {
		data, e := os.ReadFile(file)
		if e != nil {
			log.Fatal(e)
		}
		var m Manifest
		if e = json.Unmarshal(data, &m); e != nil {
			log.Fatalf("能力文件格式错误: %s", filepath.Base(file))
		}
		if e = validateManifest(m); e != nil {
			log.Fatalf("能力文件无效: %s: %s", filepath.Base(file), e)
		}
		if _, ok := p.snapshot().Abilities[m.ID]; ok {
			continue
		}
		if e = p.change(func(s *State) error {
			s.Abilities[m.ID] = Ability{Manifest: m, Config: map[string]string{}, Enabled: false}
			return nil
		}); e != nil {
			log.Fatal(e)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if voice != nil {
		p.queueSpeech = true
		if err := p.recoverDeliveries(); err != nil {
			log.Fatal("无法恢复提醒播报状态")
		}
		go p.serveDeliveries(ctx, voice)
	}
	if *buttonInput != "" || *buttonKey != 0 || *buttonLabel != "" {
		if admin == nil || runtime.GOOS != "linux" || !filepath.IsAbs(*buttonInput) ||
			*buttonKey == 0 || *buttonKey > 767 || strings.TrimSpace(*buttonLabel) == "" {
			log.Fatal("实体键登录需要 HTTPS、已验证的 evdev 路径、键码和按键名称")
		}
		button, err := os.Open(*buttonInput)
		if err != nil {
			log.Fatal("无法打开实体按键设备")
		}
		info, err := button.Stat()
		if err != nil || info.Mode()&os.ModeCharDevice == 0 {
			button.Close()
			log.Fatal("实体按键路径必须是字符设备")
		}
		admin.buttonReady, admin.buttonLabel, admin.buttonLED = true, *buttonLabel, oh2pListeningLED
		defer func() {
			admin.mu.Lock()
			defer admin.mu.Unlock()
			admin.clearDeviceLogin()
		}()
		go serveAdminButton(ctx, button, uint16(*buttonKey), admin)
	}
	if *wakeSocket != "" {
		if voice == nil || !(*vad > 0) || math.IsInf(*vad, 0) {
			log.Fatal("OH2P 唤醒套接字需要系统语音及有效录音阈值")
		}
		arecord, e := exec.LookPath("arecord")
		if e != nil {
			log.Fatal("未找到 OH2P 录音命令")
		}
		wakes, e := openWakeSocket(*wakeSocket)
		if e != nil {
			log.Fatalf("无法启动 OH2P 唤醒套接字: %v", e)
		}
		defer wakes.Close()
		captureLED := oh2pListeningLED
		if admin != nil {
			admin.voiceLogin = true
			voice.confirmLogin = admin.confirmVoiceLogin
			captureLED = admin.captureLED
		}
		go p.serveWakeSocket(ctx, wakes, voice, func(ctx context.Context) ([]byte, error) { return captureOH2P(ctx, arecord, *vad) }, captureLED)
	}
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				if e := p.tick(now.UTC()); e != nil {
					log.Printf("任务调度未保存: %v", e)
				}
			}
		}
	}()
	server := &http.Server{Handler: (&api{platform: p, assets: assets, voice: voice, admin: admin}).routes(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 55 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	if admin != nil {
		fmt.Printf("喜悦 · https://%s · 配对管理\n", admin.host)
		err = server.ServeTLS(listener, *tlsCert, *tlsKey)
	} else {
		fmt.Printf("喜悦 · http://%s · 系统语音入口启用：%t\n", listener.Addr(), voice != nil)
		err = server.Serve(listener)
	}
	if err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
