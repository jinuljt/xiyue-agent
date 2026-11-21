package main

import (
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"
	"unicode"
)

const deviceLoginCookie = "__Host-xiyue-device-login"
const deviceLoginWindow = time.Minute

var spokenLoginDigits = map[rune]byte{'零': '0', '〇': '0', '一': '1', '幺': '1', '二': '2', '两': '2', '三': '3', '四': '4', '五': '5', '六': '6', '七': '7', '八': '8', '九': '9'}

type deviceLogin struct {
	tokenDigest string
	method      string
	code        string
	expires     time.Time
	approved    bool
	timer       *time.Timer
}

func (g *adminGate) clearDeviceLogin() {
	if g.pending != nil {
		g.pending.timer.Stop()
		if g.pending.method == "button" && !g.pending.approved {
			g.buttonLED(false)
		}
		g.pending = nil
	}
}

func (g *adminGate) captureLED(on bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if p := g.pending; p == nil || p.method != "button" || p.approved {
		oh2pListeningLED(on)
	}
}

// Called only by device-local capture, after speech recognition and before model routing.
func (g *adminGate) confirmVoiceLogin(text string) (handled bool) {
	text = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || strings.ContainsRune("，,。！!：:", r) {
			return -1
		}
		return r
	}, text)
	code, ok := strings.CutPrefix(text, "确认登录")
	if !ok {
		return false
	}
	var digits strings.Builder
	for _, r := range code {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		} else if digit, ok := spokenLoginDigits[r]; ok {
			digits.WriteByte(digit)
		} else {
			return true
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if p := g.pending; p != nil && p.method == "voice" && time.Now().Before(p.expires) &&
		subtle.ConstantTimeCompare([]byte(p.code), []byte(digits.String())) == 1 {
		p.approved = true
	}
	return true
}

func (g *adminGate) confirmButtonLogin(firstPress time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if p := g.pending; p != nil && p.method == "button" && time.Now().Before(p.expires) &&
		!p.approved && !firstPress.Before(p.expires.Add(-deviceLoginWindow)) {
		p.approved = true
		g.buttonLED(false)
	}
}

func (a *api) deviceLoginAction(w http.ResponseWriter, r *http.Request) {
	g := a.admin
	if g == nil {
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("Origin") == "" {
		writeJSON(w, 403, map[string]string{"error": "请从控制台页面继续"})
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	if g.pending != nil && !now.Before(g.pending.expires) {
		g.clearDeviceLogin()
	}
	if r.Method == http.MethodPost && r.URL.Path == "/api/session/device" {
		var in struct{ Method string }
		if err := decode(w, r, &in); err != nil {
			bad(w, err)
			return
		}
		state := a.platform.snapshot()
		cfg := state.Settings
		if (in.Method != "voice" && in.Method != "button") ||
			(in.Method == "voice" && (!g.voiceLogin || !asrReady(cfg.ASR))) ||
			(in.Method == "button" && !g.buttonReady) {
			writeJSON(w, 409, map[string]string{"error": "音箱当前无法使用该确认方式"})
			return
		}
		if g.pending != nil || now.Sub(g.lastDeviceStart) < time.Second {
			writeJSON(w, 409, map[string]string{"error": "已有登录确认请求，请等待或取消后再试"})
			return
		}
		if state.Owner == nil || len(state.Owner.PasswordHash) != 32 {
			writeJSON(w, 409, map[string]string{"error": "请先完成首次管理员设置"})
			return
		}
		token := rand.Text()
		p := &deviceLogin{tokenDigest: digest(token), method: in.Method}
		var hint string
		if in.Method == "voice" {
			number, err := rand.Int(rand.Reader, big.NewInt(9000))
			if err != nil {
				http.Error(w, "无法生成音箱确认码", 500)
				return
			}
			p.code = fmt.Sprintf("%04d", 1000+number.Int64())
			hint = "喊「" + cfg.WakeWord + "」，再说「确认登录，" + p.code + "」。"
		} else {
			g.buttonLED(true)
			hint = "灯亮后，连续短按两次「" + g.buttonLabel + "」。"
		}
		p.expires = time.Now().Add(deviceLoginWindow)
		g.pending, g.lastDeviceStart = p, now
		p.timer = time.AfterFunc(deviceLoginWindow, func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			if g.pending == p {
				g.clearDeviceLogin()
			}
		})
		cookie := ownerCookie(token, p.expires)
		cookie.Name = deviceLoginCookie
		http.SetCookie(w, cookie)
		writeJSON(w, 202, map[string]any{"hint": hint, "seconds": int(deviceLoginWindow.Seconds())})
		return
	}
	p := g.pending
	cookie, err := r.Cookie(deviceLoginCookie)
	if p == nil || err != nil || len(cookie.Value) != 26 || subtle.ConstantTimeCompare([]byte(digest(cookie.Value)), []byte(p.tokenDigest)) != 1 {
		writeJSON(w, 404, map[string]string{"error": "确认请求已失效，请重新发起"})
		return
	}
	if r.Method == http.MethodDelete {
		g.clearDeviceLogin()
		writeJSON(w, 200, map[string]bool{"cancelled": true})
		return
	}
	if !p.approved {
		writeJSON(w, 202, map[string]bool{"authenticated": false})
		return
	}
	owner := a.platform.snapshot().Owner
	if owner == nil || len(owner.PasswordHash) != 32 {
		bad(w, errors.New("管理员账号不存在"))
		return
	}
	if err := a.issueOwnerSession(w, *owner, ""); err != nil {
		bad(w, err)
		return
	}
	g.clearDeviceLogin()
	writeJSON(w, 200, map[string]bool{"authenticated": true})
}
