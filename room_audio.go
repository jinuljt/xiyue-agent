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
	Name   string `json:"name"`
	Source string `json:"source"`
	Volume int    `json:"volume"`
}

func (c *roomAudioConfig) validate() error {
	c.Name = strings.TrimSpace(c.Name)
	c.Source = strings.TrimSpace(c.Source)
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
	Running   bool       `json:"running"`
	Stage     string     `json:"stage"`
	LastAudio *time.Time `json:"last_audio_at,omitempty"`
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

func (r *roomAudio) open(ctx context.Context, source string) (roomWAV, io.Closer, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return roomWAV{}, nil, errors.New("音频地址无效")
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return roomWAV{}, nil, errors.New("无法连接收音设备")
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
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
	}
}

func (r *roomAudio) relay(ctx context.Context, c roomAudioConfig) error {
	wav, body, err := r.open(ctx, c.Source)
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

func (r *roomAudio) run(ctx context.Context, c roomAudioConfig, done chan struct{}) {
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
			err := r.relay(attempt, c)
			preempted := attempt.Err() != nil
			finish()
			if preempted {
				r.stage("paused")
			} else if err != nil {
				r.stage("reconnecting")
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

func (r *roomAudio) available() bool {
	return r != nil && r.voice != nil && r.voice.playPCM != nil
}

func (a *api) roomAudioRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/room-audio", func(w http.ResponseWriter, req *http.Request) {
		status := roomAudioStatus{Stage: "stopped"}
		if a.room != nil {
			a.room.mu.Lock()
			status = a.room.status
			a.room.mu.Unlock()
		}
		writeJSON(w, 200, map[string]any{"available": a.room.available(), "config": a.platform.roomConfig(), "status": status})
	})
	m.HandleFunc("PUT /api/room-audio", func(w http.ResponseWriter, req *http.Request) {
		var c roomAudioConfig
		if err := decode(w, req, &c); err != nil {
			bad(w, err)
			return
		}
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
		if err := a.platform.change(func(s *State) error { s.RoomAudio = &c; return nil }); err != nil {
			bad(w, err)
			return
		}
		writeJSON(w, 200, c)
	})
	m.HandleFunc("POST /api/room-audio/verify", func(w http.ResponseWriter, req *http.Request) {
		var c roomAudioConfig
		if err := decode(w, req, &c); err != nil {
			bad(w, err)
			return
		}
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
		wav, body, err := a.room.open(ctx, c.Source)
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
		r := a.room
		if !r.available() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "此设备没有可用的本地播放入口"})
			return
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.cancel == nil {
			c := a.platform.roomConfig()
			if err := c.validate(); err != nil {
				bad(w, err)
				return
			}
			if c.Source == "" {
				bad(w, errors.New("请先保存收音地址"))
				return
			}
			ctx, cancel := context.WithCancel(r.parent)
			r.cancel, r.done = cancel, make(chan struct{})
			r.status = roomAudioStatus{Running: true, Stage: "connecting"}
			go r.run(ctx, c, r.done)
		}
		writeJSON(w, 200, r.status)
	})
	m.HandleFunc("POST /api/room-audio/stop", func(w http.ResponseWriter, req *http.Request) {
		r := a.room
		if r == nil {
			writeJSON(w, 200, roomAudioStatus{Stage: "stopped"})
			return
		}
		r.mu.Lock()
		done := r.done
		if r.cancel != nil {
			r.status.Stage = "stopping"
			r.cancel()
		}
		r.mu.Unlock()
		if done != nil {
			ctx, cancel := context.WithTimeout(req.Context(), 3*time.Second)
			defer cancel()
			select {
			case <-done:
			case <-ctx.Done():
			}
		}
		r.mu.Lock()
		status := r.status
		r.mu.Unlock()
		writeJSON(w, 200, status)
	})
}
