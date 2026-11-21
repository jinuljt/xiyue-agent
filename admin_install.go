package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"time"
)

const ownerSetupLimit = 4096

func (p *Platform) provisionOwner(r ownerRequest) error {
	if !r.valid() {
		return errors.New("请输入有效的管理员用户名和密码")
	}
	return p.change(func(s *State) error {
		if current := s.Owner; current != nil && len(current.PasswordHash) == 32 {
			if current.Username == r.Username && current.checkPassword(r.Password) {
				return nil
			}
			return errors.New("音箱已设置管理员，请直接登录控制台")
		}
		owner := &ownerState{Username: r.Username}
		if err := owner.setPassword(r.Password); err != nil {
			return errors.New("无法保存管理员凭据")
		}
		s.Owner = owner
		return nil
	})
}

func (p *Platform) serveOwnerSetup(ctx context.Context, listener *net.UnixListener) {
	stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stop()
	for {
		c, err := listener.AcceptUnix()
		if err != nil {
			return
		}
		_ = c.SetDeadline(time.Now().Add(8 * time.Second))
		raw, err := io.ReadAll(io.LimitReader(c, ownerSetupLimit+1))
		var request ownerRequest
		response := "安装请求无效"
		if err == nil && len(raw) <= ownerSetupLimit {
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.DisallowUnknownFields()
			if decoder.Decode(&request) == nil && decoder.Decode(new(any)) == io.EOF {
				if err = p.provisionOwner(request); err == nil {
					response = "ok"
				} else {
					response = err.Error()
				}
			}
		}
		_, _ = io.WriteString(c, response)
		_ = c.Close()
	}
}

func sendOwnerSetup(path string, stdin io.Reader) error {
	if !filepath.IsAbs(path) {
		return errors.New("本机安装入口必须是绝对路径")
	}
	raw, err := io.ReadAll(io.LimitReader(stdin, ownerSetupLimit+1))
	if err != nil || len(raw) == 0 || len(raw) > ownerSetupLimit {
		return errors.New("安装请求无效")
	}
	c, err := net.DialTimeout("unix", path, 3*time.Second)
	if err != nil {
		return errors.New("无法连接音箱安装入口")
	}
	conn := c.(*net.UnixConn)
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(8 * time.Second))
	if _, err := conn.Write(raw); err != nil {
		return errors.New("无法提交管理员设置")
	}
	if err = conn.CloseWrite(); err != nil {
		return errors.New("无法提交管理员设置")
	}
	result, err := io.ReadAll(io.LimitReader(conn, 1024))
	if err != nil || len(result) == 0 {
		return errors.New("未收到音箱的安装确认")
	}
	if string(result) != "ok" {
		return errors.New(string(result))
	}
	return nil
}

// Check the TCP peers directly. Forwarded headers are never an installation credential.
func sameSetupNetwork(r *http.Request) bool {
	local, ok := r.Context().Value(http.LocalAddrContextKey).(*net.TCPAddr)
	if !ok {
		return false
	}
	localIP, ok := netip.AddrFromSlice(local.IP)
	if !ok {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	localIP, peer = localIP.Unmap(), peer.Unmap().WithZone("")
	if localIP.IsLoopback() {
		return peer.IsLoopback()
	}
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, address := range addresses {
		ip, network, err := net.ParseCIDR(address.String())
		if err == nil && ip.Equal(localIP.AsSlice()) && network.Contains(peer.AsSlice()) {
			return true
		}
	}
	return false
}

func (a *api) setupOwnerOnConsole(w http.ResponseWriter, r *http.Request) {
	g := a.admin
	if g == nil {
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("Origin") == "" || !sameSetupNetwork(r) {
		writeJSON(w, 403, map[string]string{"error": "请在音箱所在的局域网打开控制台"})
		return
	}
	var request ownerRequest
	if err := decode(w, r, &request); err != nil {
		bad(w, err)
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if owner := a.platform.snapshot().Owner; owner != nil && len(owner.PasswordHash) == 32 {
		writeJSON(w, 409, map[string]string{"error": "管理员已设置，请直接登录"})
		return
	}
	if err := a.platform.provisionOwner(request); err != nil {
		bad(w, err)
		return
	}
	owner := a.platform.snapshot().Owner
	if err := a.issueOwnerSession(w, *owner, ""); err != nil {
		bad(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"authenticated": true})
}
