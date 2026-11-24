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

func selectNetworkResult(data []byte, field string) (string, error) {
	if field == "" {
		return boundedText(string(data), 2500), nil
	}
	var result any
	if err := json.Unmarshal(data, &result); err != nil {
		return "", errors.New("接口没有返回有效 JSON")
	}
	for _, part := range strings.Split(field, ".") {
		object, ok := result.(map[string]any)
		if !ok {
			return "", errors.New("未找到所配置的返回字段")
		}
		result, ok = object[part]
		if !ok {
			return "", errors.New("未找到所配置的返回字段")
		}
	}
	if text, ok := result.(string); ok {
		return boundedText(text, 2500), nil
	}
	out, err := json.Marshal(result)
	return boundedText(string(out), 2500), err
}
