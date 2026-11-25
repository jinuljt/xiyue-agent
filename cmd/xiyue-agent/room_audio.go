package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type roomAudioConfig struct {
	Name     string `json:"name"`
	Source   string `json:"source"`
	Volume   int    `json:"volume"`
	Username string `json:"username,omitempty"`
}

const roomPasswordKey = "room-audio:password"

var errRoomAuth = errors.New("IP Webcam 拒绝访问，请核对用户名和密码")

type roomAudioInput struct {
	roomAudioConfig
	Password string `json:"password,omitempty"`
}

func (p *Platform) roomPassword(s State, c roomAudioConfig, password string) (string, error) {
	if c.Username == "" {
		if password != "" {
			return "", errors.New("请填写 IP Webcam 用户名")
		}
		return "", nil
	}
	if c.Source == "" {
		return "", errors.New("请填写收音地址")
	}
	if password != "" {
		return password, validateServiceKey(password)
	}
	prev := s.RoomAudio
	if prev == nil || prev.Source != c.Source || prev.Username != c.Username || s.Sealed[roomPasswordKey] == "" {
		return "", errors.New("请填写此收音地址和用户名对应的密码")
	}
	return p.unseal(roomPasswordKey, s.Sealed[roomPasswordKey])
}

func (c *roomAudioConfig) validate() error {
	c.Name = strings.TrimSpace(c.Name)
	c.Source = strings.TrimSpace(c.Source)
	c.Username = strings.TrimSpace(c.Username)
	if len(c.Username) > 128 || strings.ContainsAny(c.Username, ":\r\n\x00") {
		return errors.New("IP Webcam 用户名无效")
	}
	if len([]rune(c.Name)) > 60 {
		return errors.New("收音名称最多 60 个字")
	}
	if c.Volume < 0 || c.Volume > 100 {
		return errors.New("音量须在 0 到 100 之间")
	}
	if c.Source == "" {
		return nil
	}
	u, err := url.Parse(c.Source)
	if err != nil || len(c.Source) > 500 || u.Scheme != "http" || u.User != nil || u.Fragment != "" || u.RawQuery != "" || u.ForceQuery {
		return errors.New("请填写局域网 HTTP 音频地址，地址中请勿包含凭证")
	}
	ip, err := netip.ParseAddr(u.Hostname())
	if err != nil || !ip.Is4() || !ip.IsPrivate() || u.Path == "" {
		return errors.New("请使用收音设备的局域网 IPv4 地址和音频路径")
	}
	if u.Port() != "" {
		port, err := strconv.ParseUint(u.Port(), 10, 16)
		if err != nil || port == 0 {
			return errors.New("音频地址端口无效")
		}
	}
	return nil
}

type roomAudioStatus struct {
	Name      string     `json:"name,omitempty"`
	Running   bool       `json:"running"`
	Stage     string     `json:"stage"`
	LastAudio *time.Time `json:"last_audio_at,omitempty"`
	Problem   string     `json:"problem,omitempty"`
}

type roomAudio struct {
	mu     sync.Mutex
	parent context.Context
	voice  *voiceRuntime
	client *http.Client
	cancel context.CancelFunc
	done   chan struct{}
	status roomAudioStatus
}

type roomReadDeadline struct{ net.Conn }

func (c roomReadDeadline) Read(p []byte) (int, error) {
	if err := c.SetReadDeadline(time.Now().Add(8 * time.Second)); err != nil {
		return 0, err
	}
	return c.Conn.Read(p)
}

func newRoomAudio(ctx context.Context, voice *voiceRuntime) *roomAudio {
	dialer := net.Dialer{Timeout: 3 * time.Second}
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				conn, err := dialer.DialContext(ctx, network, address)
				if err != nil {
					return nil, err
				}
				return roomReadDeadline{conn}, nil
			},
			DisableKeepAlives:      true,
			DisableCompression:     true,
			ResponseHeaderTimeout:  5 * time.Second,
			MaxResponseHeaderBytes: 16 << 10,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return &roomAudio{parent: ctx, voice: voice, client: client, status: roomAudioStatus{Stage: "stopped"}}
}

func (r *roomAudio) open(ctx context.Context, c roomAudioConfig, password string) (roomWAV, io.Closer, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Source, nil)
	if err != nil {
		return roomWAV{}, nil, errors.New("音频地址无效")
	}
	if c.Username != "" {
		req.SetBasicAuth(c.Username, password)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return roomWAV{}, nil, errors.New("无法连接收音设备")
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return roomWAV{}, nil, errRoomAuth
		}
		return roomWAV{}, nil, errors.New("收音设备没有返回音频，请检查地址和访问设置")
	}
	wav, err := openRoomWAV(resp.Body)
	if err != nil {
		resp.Body.Close()
		return roomWAV{}, nil, err
	}
	return wav, resp.Body, nil
}

func (r *roomAudio) stage(stage string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.status.Stage != "stopping" {
		r.status.Stage = stage
		if stage == "connected" {
			r.status.Problem = ""
		}
	}
}

func (r *roomAudio) relay(ctx context.Context, c roomAudioConfig, password string) error {
	wav, body, err := r.open(ctx, c, password)
	if err != nil {
		return err
	}
	defer body.Close()
	r.stage("connected")
	// Retain up to 240 ms so a normal burst of TCP data does not drop speech.
	frames := make(chan roomFrame, 12)
	finished := make(chan error, 1)
	go func() {
		finished <- wav.stream(ctx, c.Volume, frames, func() {
			now := time.Now().UTC()
			r.mu.Lock()
			r.status.LastAudio = &now
			r.mu.Unlock()
		})
	}()
	player := &roomPCMReader{ctx: ctx, frames: frames, onPlay: func() { r.stage("playing") }}
	playErr := r.voice.playPCM(ctx, player)
	body.Close()
	streamErr := <-finished
	if playErr != nil {
		return playErr
	}
	return streamErr
}

func (r *roomAudio) run(ctx context.Context, c roomAudioConfig, password string, done chan struct{}) {
	defer func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.status = roomAudioStatus{Stage: "stopped"}
		r.cancel, r.done = nil, nil
		close(done)
	}()
	for ctx.Err() == nil {
		attempt, finish, ok := r.voice.claimRoomPlayback(ctx)
		delay := 200 * time.Millisecond
		if ok {
			r.stage("connecting")
			err := r.relay(attempt, c, password)
			preempted := attempt.Err() != nil
			finish()
			if preempted {
				r.stage("paused")
			} else if err != nil {
				r.stage("reconnecting")
				r.mu.Lock()
				if errors.Is(err, errRoomAuth) {
					r.status.Problem = errRoomAuth.Error()
				}
				r.mu.Unlock()
				delay = 2 * time.Second
			}
		} else {
			r.stage("paused")
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (p *Platform) roomConfig() roomAudioConfig {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state.RoomAudio == nil {
		return roomAudioConfig{Volume: 70}
	}
	return *p.state.RoomAudio
}

func (p *Platform) saveRoomConfig(s *State, c roomAudioConfig, supplied string) error {
	password, err := p.roomPassword(*s, c, supplied)
	if err != nil {
		return err
	}
	if c.Username == "" {
		delete(s.Sealed, roomPasswordKey)
	} else if supplied != "" {
		sealed, err := p.seal(roomPasswordKey, password)
		if err != nil {
			return err
		}
		if s.Sealed == nil {
			s.Sealed = map[string]string{}
		}
		s.Sealed[roomPasswordKey] = sealed
	}
	s.RoomAudio = &c
	if c.Source == "" {
		s.RoomListening = false
	}
	return nil
}

func (r *roomAudio) available() bool {
	return r != nil && r.voice != nil && r.voice.playPCM != nil
}

func (r *roomAudio) runtimeSnapshot() roomAudioStatus {
	if r == nil {
		return roomAudioStatus{Stage: "stopped"}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status
}

func (a *api) roomAudioRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/room-audio", func(w http.ResponseWriter, req *http.Request) {
		status := a.room.runtimeSnapshot()
		s := a.platform.snapshot()
		writeJSON(w, 200, map[string]any{"available": a.room.available(), "config": a.platform.roomConfig(), "status": status,
			"listening": s.RoomListening, "password_configured": s.Sealed[roomPasswordKey] != ""})
	})
	m.HandleFunc("PUT /api/room-audio", func(w http.ResponseWriter, req *http.Request) {
		var in roomAudioInput
		if err := decode(w, req, &in); err != nil {
			bad(w, err)
			return
		}
		c := in.roomAudioConfig
		if err := c.validate(); err != nil {
			bad(w, err)
			return
		}
		if a.room != nil {
			a.room.mu.Lock()
			defer a.room.mu.Unlock()
			if a.room.cancel != nil {
				writeJSON(w, http.StatusConflict, map[string]string{"error": "请先停止听音，再修改配置"})
				return
			}
		}
		if err := a.platform.change(func(s *State) error {
			return a.platform.saveRoomConfig(s, c, in.Password)
		}); err != nil {
			bad(w, err)
			return
		}
		writeJSON(w, 200, c)
	})
	m.HandleFunc("POST /api/room-audio/verify", func(w http.ResponseWriter, req *http.Request) {
		var in roomAudioInput
		if err := decode(w, req, &in); err != nil {
			bad(w, err)
			return
		}
		c := in.roomAudioConfig
		if err := c.validate(); err != nil {
			bad(w, err)
			return
		}
		if c.Source == "" || a.room == nil {
			bad(w, errors.New("请先填写收音地址"))
			return
		}
		ctx, cancel := context.WithTimeout(req.Context(), 8*time.Second)
		defer cancel()
		password, err := a.platform.roomPassword(a.platform.snapshot(), c, in.Password)
		if err != nil {
			bad(w, err)
			return
		}
		wav, body, err := a.room.open(ctx, c, password)
		if err != nil {
			bad(w, err)
			return
		}
		defer body.Close()
		var sample [4]byte
		if _, err = io.ReadFull(wav.pcm, sample[:wav.channels*2]); err != nil {
			bad(w, errors.New("已识别 WAV 格式，但没有收到采样数据"))
			return
		}
		writeJSON(w, 200, map[string]int{"sample_rate": wav.rate, "channels": wav.channels})
	})
	m.HandleFunc("POST /api/room-audio/start", func(w http.ResponseWriter, req *http.Request) {
		if !a.room.available() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "此设备没有可用的本地播放入口"})
			return
		}
		var in struct {
			Config *roomAudioInput `json:"config"`
		}
		if req.ContentLength != 0 {
			if err := decode(w, req, &in); err != nil {
				bad(w, err)
				return
			}
		}
		status, err := a.room.startWithConfig(a.platform, in.Config)
		if err != nil {
			bad(w, err)
			return
		}
		writeJSON(w, 200, status)
	})
	m.HandleFunc("POST /api/room-audio/stop", func(w http.ResponseWriter, req *http.Request) {
		status, err := a.room.stop(req.Context(), a.platform)
		if err != nil {
			bad(w, err)
			return
		}
		writeJSON(w, 200, status)
	})
}

func (r *roomAudio) restore(p *Platform) error {
	p.mu.Lock()
	active := p.state.RoomListening
	p.mu.Unlock()
	if !active || !r.available() {
		return nil
	}
	_, err := r.start(p)
	return err
}

func (r *roomAudio) start(p *Platform) (roomAudioStatus, error) {
	return r.startWithConfig(p, nil)
}

func (r *roomAudio) startWithConfig(p *Platform, draft *roomAudioInput) (roomAudioStatus, error) {
	if !r.available() {
		return roomAudioStatus{}, errors.New("此设备没有可用的本地播放入口")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.status.Stage == "stopping" {
		return r.status, errors.New("听音正在停止")
	}
	if r.cancel != nil && draft != nil {
		return r.status, errors.New("请先停止听音，再修改配置")
	}
	if r.cancel == nil {
		s := p.snapshot()
		c := p.roomConfig()
		var temporary string
		if draft != nil {
			c, temporary = draft.roomAudioConfig, draft.Password
		}
		if err := c.validate(); err != nil {
			return r.status, err
		}
		if c.Source == "" {
			return r.status, errors.New("请填写收音地址")
		}
		password, err := p.roomPassword(s, c, temporary)
		if err != nil {
			return r.status, err
		}
		if draft != nil || !s.RoomListening {
			if err := p.change(func(next *State) error {
				if draft != nil {
					if err := p.saveRoomConfig(next, c, temporary); err != nil {
						return err
					}
				}
				next.RoomListening = true
				return nil
			}); err != nil {
				return r.status, err
			}
		}
		ctx, cancel := context.WithCancel(r.parent)
		r.cancel, r.done = cancel, make(chan struct{})
		r.status = roomAudioStatus{Name: c.Name, Running: true, Stage: "connecting"}
		go r.run(ctx, c, password, r.done)
	}
	return r.status, nil
}

func (r *roomAudio) stop(parent context.Context, p *Platform) (roomAudioStatus, error) {
	if r == nil {
		return roomAudioStatus{Stage: "stopped"}, errors.New("远程听音不可用")
	}
	r.mu.Lock()
	if err := p.change(func(s *State) error {
		if !s.RoomListening {
			return errNoVoiceChange
		}
		s.RoomListening = false
		return nil
	}); err != nil && !errors.Is(err, errNoVoiceChange) {
		status := r.status
		r.mu.Unlock()
		return status, err
	}
	done := r.done
	if r.cancel != nil {
		r.status.Stage = "stopping"
		r.cancel()
	}
	r.mu.Unlock()
	if done != nil {
		ctx, cancel := context.WithTimeout(parent, 3*time.Second)
		defer cancel()
		select {
		case <-done:
		case <-ctx.Done():
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status, nil
}

const (
	roomStartToolID = "remote-listen.start"
	roomStopToolID  = "remote-listen.stop"
	roomStateID     = "remote-listen.running"
)

func roomAutomationTools(room *roomAudio) []automationToolSpec {
	available := func(_ *State) error {
		if !room.available() {
			return errors.New("此设备没有可用的本地播放入口")
		}
		return nil
	}
	return []automationToolSpec{
		{
			ID: roomStartToolID, Name: "远程听音 · 开始",
			Hint: "流程会连接收音设备并在音箱播放，直到手动或通过流程停止。",
			available: func(s *State) error {
				if err := available(s); err != nil {
					return err
				}
				if s.RoomAudio == nil || s.RoomAudio.Source == "" {
					return errors.New("请先在能力中心填写远程听音的收音地址")
				}
				return nil
			},
			run: func(p *Platform, _ map[string]string, _ time.Time) error {
				_, err := room.start(p)
				return err
			},
		},
		{
			ID: roomStopToolID, Name: "远程听音 · 停止", available: available,
			run: func(p *Platform, _ map[string]string, _ time.Time) error {
				status, err := room.stop(context.Background(), p)
				if err == nil && status.Running {
					return errors.New("在限定时间内未能停止听音")
				}
				return err
			},
		},
	}
}

func roomAutomationState(room *roomAudio) automationStateSpec {
	return automationStateSpec{
		ID: roomStateID, Name: "远程听音 · 运行状态",
		Options: map[string]string{"on": "已开启", "off": "已停止"}, group: "room", key: roomStateID,
		read: func(_ context.Context, _ *Platform, _ State) map[string]string {
			if !room.available() {
				return nil
			}
			room.mu.Lock()
			defer room.mu.Unlock()
			value := "off"
			if room.status.Running {
				value = "on"
			}
			return map[string]string{roomStateID: value}
		},
	}
}
