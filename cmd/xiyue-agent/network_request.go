package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var networkClient = &http.Client{Timeout: 9 * time.Second, Transport: &http.Transport{DisableKeepAlives: true, MaxResponseHeaderBytes: 32 << 10}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func configuredURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 1000 || u.Hostname() == "" || u.User != nil || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, errors.New("请输入不含凭证的 HTTP 或 HTTPS 地址")
	}
	host := strings.ToLower(u.Hostname())
	if ip, e := netip.ParseAddr(host); e == nil {
		if ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsMulticast() {
			return nil, errors.New("不支持该目标地址")
		}
		if u.Scheme == "http" && !ip.IsPrivate() && !ip.IsLoopback() {
			return nil, errors.New("公网工具请使用 HTTPS")
		}
	} else if u.Scheme == "http" && host != "localhost" && !strings.HasSuffix(host, ".local") {
		return nil, errors.New("公网工具请使用 HTTPS；局域网可填写私有 IP 或 .local 地址")
	}
	return u, nil
}

func callNetwork(ctx context.Context, endpoint, method, token string, payload any, limit int64) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, errors.New("请求参数无效")
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, errors.New("无法创建工具请求")
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := networkClient.Do(req)
	if err != nil {
		return nil, errors.New("连接工具服务失败或超时")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("工具服务返回 HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("工具响应过大或无法读取")
	}
	return data, nil
}

func boundedText(text string, limit int) string {
	rs := []rune(strings.TrimSpace(text))
	return string(rs[:min(limit, len(rs))])
}

func selectNetworkValue(data []byte, field string) (json.RawMessage, error) {
	data = bytes.TrimSpace(data)
	if !json.Valid(data) {
		if field != "" {
			return nil, errors.New("接口没有返回有效 JSON")
		}
		return json.Marshal(string(data))
	}
	value := json.RawMessage(data)
	if field == "" {
		return value, nil
	}
	parts := strings.Split(field, ".")
	if strings.HasPrefix(field, "/") {
		parts = strings.Split(field[1:], "/")
		for i, part := range parts {
			parts[i] = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		}
	}
	for _, part := range parts {
		next := json.RawMessage(nil)
		switch value[0] {
		case '{':
			var object map[string]json.RawMessage
			if json.Unmarshal(value, &object) == nil {
				next = object[part]
			}
		case '[':
			var list []json.RawMessage
			i, err := strconv.Atoi(part)
			if err == nil && i >= 0 && json.Unmarshal(value, &list) == nil && i < len(list) {
				next = list[i]
			}
		}
		if len(next) == 0 {
			return nil, errors.New("未找到所配置的返回字段")
		}
		value = next
	}
	return value, nil
}

func networkValueText(value json.RawMessage) string {
	if len(value) > 0 && value[0] == '"' {
		var text string
		if json.Unmarshal(value, &text) == nil {
			return boundedText(text, 2500)
		}
	}
	var compact bytes.Buffer
	if json.Compact(&compact, value) == nil {
		return boundedText(compact.String(), 2500)
	}
	return ""
}
