package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net"
	"path/filepath"
	"time"
)

const ownerSetupLimit = 4096

func (p *Platform) provisionOwner(r ownerRequest) error {
	if !r.valid() || len([]rune(r.Password)) < 12 {
		return errors.New("请输入有效用户名和至少 12 个字符的密码")
	}
	return p.change(func(s *State) error {
		if current := s.Owner; current != nil && len(current.PasswordHash) == 32 {
			if current.Username == r.Username {
				hash, err := passwordHash(r.Password, current.PasswordSalt)
				if err == nil && subtle.ConstantTimeCompare(hash, current.PasswordHash) == 1 {
					return nil
				}
			}
			return errors.New("音箱已设置管理员，请直接登录控制台")
		}
		owner := &ownerState{Username: r.Username, PasswordSalt: make([]byte, 16)}
		if _, err := rand.Read(owner.PasswordSalt); err != nil {
			return errors.New("无法生成管理员凭据")
		}
		hash, err := passwordHash(r.Password, owner.PasswordSalt)
		if err != nil {
			return errors.New("无法保存管理员凭据")
		}
		owner.PasswordHash = hash
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
