package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

type api struct {
	platform *Platform
	assets   map[string][]byte
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
	m.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, a.platform.publicState()) })
	m.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"name": "喜悦", "version": "0.2.0", "speech": "preview", "agent": "not_connected", "asr": "not_connected", "llm": "not_connected"})
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
		types := map[string]string{"/index.html": "text/html; charset=utf-8", "/help.html": "text/html; charset=utf-8", "/app.js": "text/javascript; charset=utf-8", "/style.css": "text/css; charset=utf-8"}
		w.Header().Set("Content-Type", types[path])
		_, _ = w.Write(data)
	})
	return guard(m)
}

var credentialRef = regexpCredential()

func regexpCredential() *regexp.Regexp {
	return regexp.MustCompile(`^bitwarden:item:[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
}

// Loopback-only until device pairing and authenticated LAN access are implemented.
func guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if host != "localhost" && host != "127.0.0.1" && host != "::1" {
			http.Error(w, "本地控制台拒绝此 Host", 403)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			u, e := url.Parse(origin)
			if e != nil || u.Scheme != "http" || u.Host != r.Host {
				http.Error(w, "拒绝跨站请求", 403)
				return
			}
		}
		if r.Method != "GET" && r.Method != "HEAD" {
			if r.Header.Get("X-Xiyue-Request") != "console" || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
				http.Error(w, "需要同源 JSON 请求", 403)
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
	flag.Parse()
	host, _, err := net.SplitHostPort(*listen)
	if err != nil || (host != "127.0.0.1" && host != "::1") {
		log.Fatal("首版仅监听回环地址；局域网设备配对尚未实现")
	}
	assets := map[string][]byte{}
	for _, name := range []string{"index.html", "app.js", "style.css", "help.html"} {
		data, e := os.ReadFile(filepath.Join(*consoleDir, name))
		if errors.Is(e, os.ErrNotExist) && name == "help.html" {
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
	server := &http.Server{Handler: (&api{platform: p, assets: assets}).routes(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 55 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	fmt.Printf("喜悦 · http://%s · 仅文字预览，不连接音箱\n", listener.Addr())
	if err = server.Serve(listener); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
