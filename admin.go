package main

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
)

const sessionCookie = "__Host-xiyue-owner"

type ownerState struct {
	Username      string    `json:"username,omitempty"`
	PasswordSalt  []byte    `json:"password_salt,omitempty"`
	PasswordHash  []byte    `json:"password_hash,omitempty"`
	PairingDigest string    `json:"pairing_digest"`
	SessionDigest string    `json:"session_digest"`
	ExpiresAt     time.Time `json:"expires_at"`
}

type adminGate struct {
	host            string
	codePath        string
	mu              sync.Mutex
	lastAttempt     time.Time
	lastDeviceStart time.Time
	pending         *deviceLogin
	voiceLogin      bool
	buttonReady     bool
	buttonLED       func(bool)
	buttonLabel     string
}

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func readAdminCode(path string) (string, time.Time, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", time.Time{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 256 {
		return "", time.Time{}, errors.New("恢复码文件不符合权限或格式要求")
	}
	raw := make([]byte, info.Size())
	if _, err = io.ReadFull(f, raw); err != nil {
		return "", time.Time{}, err
	}
	code := strings.TrimSpace(string(raw))
	if len(code) < 32 || strings.ContainsAny(code, " \t\r\n") {
		return "", time.Time{}, errors.New("恢复码文件格式无效")
	}
	return digest(code), info.ModTime().Add(15 * time.Minute), nil
}

func writeAdminCode(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("恢复码文件需要绝对路径")
	}
	info, err := os.Stat(filepath.Dir(path))
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("请将恢复码放在仅所有者可访问的目录")
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".admin-code-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.WriteString(rand.Text() + rand.Text() + "\n")
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}

func passwordHash(password string, salt []byte) ([]byte, error) {
	return pbkdf2.Key(sha256.New, password, salt, 600_000, 32)
}

func (p *Platform) authenticated(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil || len(c.Value) != 43 {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	x := p.state.Owner
	return x != nil && len(x.PasswordHash) == 32 && time.Now().Before(x.ExpiresAt) && subtle.ConstantTimeCompare([]byte(digest(c.Value)), []byte(x.SessionDigest)) == 1
}

func ownerCookie(value string, expires time.Time) *http.Cookie {
	return &http.Cookie{Name: sessionCookie, Value: value, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode, Expires: expires}
}

type ownerRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Code     string `json:"code,omitempty"`
}

func (a *api) authenticateOwner(w http.ResponseWriter, r *http.Request, useCode bool) {
	g := a.admin
	if g == nil {
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("Origin") == "" {
		writeJSON(w, 403, map[string]string{"error": "请从控制台页面继续"})
		return
	}
	var request ownerRequest
	if err := decode(w, r, &request); err != nil {
		bad(w, err)
		return
	}
	request.Username = strings.TrimSpace(request.Username)
	if request.Username == "" || len(request.Username) > 64 ||
		strings.ContainsFunc(request.Username, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) ||
		len(request.Password) > 256 {
		bad(w, errors.New("请输入有效的管理员用户名和密码"))
		return
	}
	if useCode && len([]rune(request.Password)) < 12 {
		bad(w, errors.New("请使用至少 12 个字符的密码"))
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
	current := a.platform.snapshot().Owner
	var owner ownerState
	if useCode {
		code, expires, err := readAdminCode(g.codePath)
		if err != nil || !now.Before(expires) || len(request.Code) > 256 ||
			subtle.ConstantTimeCompare([]byte(digest(strings.TrimSpace(request.Code))), []byte(code)) != 1 ||
			(current != nil && current.PairingDigest == code) {
			writeJSON(w, 401, map[string]string{"error": "设置或恢复码无效、已过期或已使用"})
			return
		}
		owner = ownerState{Username: request.Username, PairingDigest: code, PasswordSalt: make([]byte, 16)}
		if _, err := rand.Read(owner.PasswordSalt); err != nil {
			http.Error(w, "无法设置管理员", 500)
			return
		}
		owner.PasswordHash, err = passwordHash(request.Password, owner.PasswordSalt)
		if err != nil {
			http.Error(w, "无法设置管理员", 500)
			return
		}
	} else {
		if current == nil || len(current.PasswordSalt) != 16 || len(current.PasswordHash) != 32 {
			writeJSON(w, 401, map[string]string{"error": "请先设置管理员"})
			return
		}
		candidate, err := passwordHash(request.Password, current.PasswordSalt)
		if err != nil || subtle.ConstantTimeCompare(candidate, current.PasswordHash) != 1 || request.Username != current.Username {
			writeJSON(w, 401, map[string]string{"error": "用户名或密码不正确"})
			return
		}
		owner = *current
	}
	if err := a.issueOwnerSession(w, owner, ""); err != nil {
		bad(w, err)
		return
	}
	g.clearDeviceLogin()
	writeJSON(w, 200, map[string]bool{"authenticated": true})
}

func (a *api) issueOwnerSession(w http.ResponseWriter, owner ownerState, previous string) error {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return errors.New("无法创建管理会话")
	}
	session := base64.RawURLEncoding.EncodeToString(raw)
	owner.ExpiresAt = time.Now().Add(30 * 24 * time.Hour)
	owner.SessionDigest = digest(session)
	if err := a.platform.change(func(s *State) error {
		if previous != "" && (s.Owner == nil || s.Owner.SessionDigest != previous || !time.Now().Before(s.Owner.ExpiresAt)) {
			return errors.New("登录已失效，请重新登录")
		}
		s.Owner = &owner
		return nil
	}); err != nil {
		return err
	}
	http.SetCookie(w, ownerCookie(session, owner.ExpiresAt))
	return nil
}

func (a *api) changeOwnerPassword(w http.ResponseWriter, r *http.Request) {
	g := a.admin
	if g == nil {
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("Origin") == "" {
		writeJSON(w, 403, map[string]string{"error": "请从控制台页面继续"})
		return
	}
	var in struct{ Password string }
	if err := decode(w, r, &in); err != nil {
		bad(w, err)
		return
	}
	if len([]rune(in.Password)) < 12 || len(in.Password) > 256 {
		bad(w, errors.New("请使用至少 12 个字符的密码"))
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	cookie, err := r.Cookie(sessionCookie)
	owner := a.platform.snapshot().Owner
	if err != nil || owner == nil || owner.SessionDigest != digest(cookie.Value) || !time.Now().Before(owner.ExpiresAt) {
		writeJSON(w, 401, map[string]string{"error": "登录已失效，请重新登录"})
		return
	}
	previous := owner.SessionDigest
	owner.PasswordSalt = make([]byte, 16)
	if _, err := rand.Read(owner.PasswordSalt); err != nil {
		http.Error(w, "无法修改密码", 500)
		return
	}
	owner.PasswordHash, err = passwordHash(in.Password, owner.PasswordSalt)
	if err != nil {
		http.Error(w, "无法修改密码", 500)
		return
	}
	if err := a.issueOwnerSession(w, *owner, previous); err != nil {
		bad(w, err)
		return
	}
	g.clearDeviceLogin()
	writeJSON(w, 200, map[string]bool{"authenticated": true})
}

func (a *api) adminRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/session", func(w http.ResponseWriter, r *http.Request) {
		required := a.admin != nil
		a.platform.mu.Lock()
		owner := a.platform.state.Owner
		configured := owner != nil && len(owner.PasswordHash) == 32
		a.platform.mu.Unlock()
		status := map[string]bool{"required": required, "authenticated": !required || a.platform.authenticated(r),
			"configured": configured}
		if g := a.admin; g != nil && configured {
			g.mu.Lock()
			status["device_button"] = g.buttonReady
			status["device_voice"] = g.voiceLogin && asrReady(a.platform.snapshot().Settings.ASR)
			g.mu.Unlock()
		}
		writeJSON(w, 200, status)
	})
	m.HandleFunc("POST /api/session/device", a.deviceLoginAction)
	m.HandleFunc("POST /api/session/device/claim", a.deviceLoginAction)
	m.HandleFunc("DELETE /api/session/device", a.deviceLoginAction)
	m.HandleFunc("POST /api/session/password", a.changeOwnerPassword)
	m.HandleFunc("POST /api/session/pair", func(w http.ResponseWriter, r *http.Request) {
		a.authenticateOwner(w, r, true)
	})
	m.HandleFunc("POST /api/session/login", func(w http.ResponseWriter, r *http.Request) {
		a.authenticateOwner(w, r, false)
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
