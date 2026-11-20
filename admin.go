package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const sessionCookie = "__Host-xiyue-owner"

type ownerState struct {
	PairingDigest string    `json:"pairing_digest"`
	SessionDigest string    `json:"session_digest"`
	ExpiresAt     time.Time `json:"expires_at"`
}

type adminGate struct {
	host           string
	pairingDigest  string
	pairingExpires time.Time
	mu             sync.Mutex
	lastAttempt    time.Time
}

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func readAdminPairing(host, path string) (*adminGate, error) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 256 {
		return nil, errors.New("配对码文件必须是仅所有者可读写的普通文件")
	}
	f, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("无法读取配对码文件")
	}
	code := strings.TrimSpace(string(f))
	if len(code) < 32 || strings.ContainsAny(code, " \t\r\n") {
		return nil, errors.New("配对码须为至少 32 字符的随机字符串")
	}
	return &adminGate{host: host, pairingDigest: digest(code), pairingExpires: info.ModTime().Add(15 * time.Minute)}, nil
}

func (p *Platform) authenticated(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil || len(c.Value) != 43 {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	x := p.state.Owner
	return x != nil && time.Now().Before(x.ExpiresAt) && subtle.ConstantTimeCompare([]byte(digest(c.Value)), []byte(x.SessionDigest)) == 1
}

func ownerCookie(value string, expires time.Time) *http.Cookie {
	return &http.Cookie{Name: sessionCookie, Value: value, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode, Expires: expires}
}

func (a *api) adminRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/session", func(w http.ResponseWriter, r *http.Request) {
		required := a.admin != nil
		writeJSON(w, 200, map[string]bool{"required": required, "authenticated": !required || a.platform.authenticated(r)})
	})
	m.HandleFunc("POST /api/session/pair", func(w http.ResponseWriter, r *http.Request) {
		g := a.admin
		if g == nil {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Origin") == "" {
			writeJSON(w, 403, map[string]string{"error": "请从控制台页面配对"})
			return
		}
		var request struct {
			Code string `json:"code"`
		}
		if err := decode(w, r, &request); err != nil {
			bad(w, err)
			return
		}
		now := time.Now()
		g.mu.Lock()
		defer g.mu.Unlock()
		if now.Sub(g.lastAttempt) < time.Second {
			writeJSON(w, 429, map[string]string{"error": "请稍候再试"})
			return
		}
		g.lastAttempt = now
		if !now.Before(g.pairingExpires) || len(request.Code) > 256 || subtle.ConstantTimeCompare([]byte(digest(strings.TrimSpace(request.Code))), []byte(g.pairingDigest)) != 1 {
			writeJSON(w, 401, map[string]string{"error": "配对码无效或已过期"})
			return
		}
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			writeJSON(w, 500, map[string]string{"error": "无法创建管理会话"})
			return
		}
		session := base64.RawURLEncoding.EncodeToString(raw)
		expires := now.Add(30 * 24 * time.Hour)
		err := a.platform.change(func(s *State) error {
			if s.Owner != nil && s.Owner.PairingDigest == g.pairingDigest {
				return errors.New("配对码已使用，请从设备重新取得配对码")
			}
			s.Owner = &ownerState{PairingDigest: g.pairingDigest, SessionDigest: digest(session), ExpiresAt: expires}
			return nil
		})
		if err != nil {
			bad(w, err)
			return
		}
		http.SetCookie(w, ownerCookie(session, expires))
		writeJSON(w, 200, map[string]bool{"authenticated": true})
	})
	m.HandleFunc("POST /api/session/logout", func(w http.ResponseWriter, r *http.Request) {
		if a.admin == nil {
			http.NotFound(w, r)
			return
		}
		if err := a.platform.change(func(s *State) error {
			c, err := r.Cookie(sessionCookie)
			if err == nil && s.Owner != nil && s.Owner.SessionDigest == digest(c.Value) {
				s.Owner.SessionDigest = ""
				s.Owner.ExpiresAt = time.Time{}
			}
			return nil
		}); err != nil {
			bad(w, err)
			return
		}
		c := ownerCookie("", time.Unix(1, 0))
		c.MaxAge = -1
		http.SetCookie(w, c)
		writeJSON(w, 200, map[string]bool{"authenticated": false})
	})
}
