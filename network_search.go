package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

const searchOrigin = "https://api.deepseek.com"
const searchSecret = "network:search"

type searchSettings struct {
	Enabled bool `json:"enabled"`
	Reuse   bool `json:"reuse"`
}
type searchInput struct {
	searchSettings
	APIKey string `json:"api_key,omitempty"`
	Query  string `json:"query,omitempty"`
}
type searchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Summary string `json:"summary"`
	Snippet string `json:"snippet,omitempty"`
	Content string `json:"content,omitempty"`
}

func searchPublicURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 2000 || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	return strings.Contains(host, ".") && net.ParseIP(host) == nil && !strings.HasSuffix(host, ".local") && !strings.HasSuffix(host, ".internal")
}
func (p *Platform) searchKey(s State, c searchSettings, draft string) (string, error) {
	if c.Reuse {
		if draft != "" {
			return "", errors.New("请选择复用或独立凭证")
		}
		endpoint := strings.TrimRight(s.Settings.LLM.Endpoint, "/")
		if s.Settings.LLM.Provider != "deepseek" || (endpoint != searchOrigin && endpoint != searchOrigin+"/v1") || s.Sealed["service:llm"] == "" {
			return "", errors.New("请先配置官方 DeepSeek 模型凭证，或单独填写搜索 API Key")
		}
		return p.serviceKey(s, "llm", nil)
	}
	if draft != "" {
		return draft, validateServiceKey(draft)
	}
	if s.Sealed[searchSecret] == "" {
		return "", errors.New("请填写搜索 API Key")
	}
	return p.unseal(searchSecret, s.Sealed[searchSecret])
}
func (p *Platform) webTool(ctx context.Context, s State, kind, value, key string, test bool) (string, error) {
	payload := map[string]any{}
	switch kind {
	case "search":
		value = strings.TrimSpace(value)
		if utf8.RuneCountInString(value) < 1 || utf8.RuneCountInString(value) > 128 {
			return "", errors.New("搜索词须为 1 到 128 个字符")
		}
		payload = map[string]any{"query": value, "count": 5}
	case "fetch":
		if !searchPublicURL(value) {
			return "", errors.New("仅支持读取公开网页")
		}
		payload["url"] = value
	default:
		return "", errors.New("未知网页工具")
	}
	start := time.Now().UTC()
	data, err := callNetwork(ctx, searchOrigin+"/v1/tools/"+kind, http.MethodPost, key, payload, 512<<10)
	var result string
	var billed bool
	if err == nil {
		if kind == "search" {
			var response struct {
				Results []searchResult `json:"results"`
			}
			if json.Unmarshal(data, &response) != nil || response.Results == nil {
				err = errors.New("搜索服务返回格式异常")
			} else {
				billed = len(response.Results) > 0
				rows := []searchResult{}
				for _, r := range response.Results[:min(5, len(response.Results))] {
					if !searchPublicURL(r.URL) {
						continue
					}
					if r.Summary == "" {
						r.Summary = r.Snippet
					}
					rows = append(rows, searchResult{Title: boundedText(r.Title, 160), URL: r.URL, Summary: boundedText(r.Summary, 420)})
				}
				b, _ := json.Marshal(rows)
				result = string(b)
			}
		} else {
			var response searchResult
			if json.Unmarshal(data, &response) != nil || strings.TrimSpace(response.Content) == "" {
				err = errors.New("网页抓取未返回正文")
			} else {
				billed = true
				b, _ := json.Marshal(searchResult{URL: value, Title: boundedText(response.Title, 160), Summary: boundedText(response.Content, 4000)})
				result = string(b)
			}
		}
	}
	record := UsageRecord{ID: newID(), At: start, Service: "search", Provider: "deepseek", Model: kind, BillableRequest: &billed, Success: err == nil, Test: test, ConfigRevision: s.Settings.Revision, DurationMS: time.Since(start).Milliseconds()}
	if err != nil {
		record.ErrorCode = "search_service_error"
	}
	p.activity.add(record, s.Settings.Timezone)
	return result, err
}
