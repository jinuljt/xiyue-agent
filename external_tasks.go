package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var connectorTaskID = regexp.MustCompile("^[a-f0-9]{32}$")

type externalTaskRef struct {
	ID        string    `json:"id"`
	Binding   string    `json:"binding"`
	CreatedAt time.Time `json:"created_at"`
}
type externalTaskStatus struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Phase  string `json:"phase,omitempty"`
	Reply  string `json:"reply,omitempty"`
	Error  string `json:"error,omitempty"`
}

func connectorTasks(s State) (string, string, error) {
	cfg := s.Settings.External
	if (s.Settings.Mode != "external" && s.Settings.Mode != "hybrid") || cfg.Provider != "http_json" {
		return "", "", errors.New("请先启用外部 Agent 或组合模式")
	}
	if validateEndpoint(cfg.Endpoint) != nil || !strings.HasSuffix(cfg.Endpoint, "/v1/chat") {
		return "", "", errors.New("后台任务需要以 /v1/chat 结尾的喜悦连接器地址")
	}
	return strings.TrimSuffix(cfg.Endpoint, "/chat") + "/tasks",
		digest(cfg.Endpoint + "\n" + s.Sealed["service:external"]), nil
}

func (p *Platform) callExternalTask(ctx context.Context, s State, method, endpoint, idempotency, message string) (externalTaskStatus, error) {
	var result externalTaskStatus
	var body io.Reader
	if method == http.MethodPost {
		payload, _ := json.Marshal(map[string]string{"message": message})
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return result, errors.New("连接器地址无效")
	}
	if secret := s.Sealed["service:external"]; secret != "" {
		u, _ := url.Parse(endpoint)
		if u.Scheme != "https" && u.Hostname() != "localhost" && !net.ParseIP(u.Hostname()).IsLoopback() {
			return result, errors.New("带凭证的连接器需要 HTTPS")
		}
		key, err := p.unseal("service:external", secret)
		if err != nil {
			return result, err
		}
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if method == http.MethodPost {
		req.Header.Set("Idempotency-Key", idempotency)
		req.Header.Set("Content-Type", "application/json")
	}
	client := http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		return result, errors.New("无法连接外部 Agent")
	}
	defer res.Body.Close()
	if res.StatusCode == 404 {
		return result, errors.New("连接器未开放后台任务，或记录已过期")
	}
	if res.StatusCode == 409 {
		return result, errors.New("连接器正在处理另一任务，或提交编号冲突")
	}
	expected := http.StatusOK
	if method == http.MethodPost {
		expected = http.StatusAccepted
	}
	if res.StatusCode != expected {
		return result, fmt.Errorf("连接器请求失败（HTTP %d）", res.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, (256<<10)+1))
	if err != nil || len(data) > 256<<10 || json.Unmarshal(data, &result) != nil || !connectorTaskID.MatchString(result.ID) {
		return result, errors.New("连接器返回格式无效")
	}
	switch result.Status {
	case "running", "cancelling", "cancelled", "completed", "failed":
	default:
		return result, errors.New("连接器返回了未知任务状态")
	}
	return result, nil
}

func (a *api) externalTaskRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/external/tasks", func(w http.ResponseWriter, r *http.Request) {
		s := a.platform.snapshot()
		_, binding, err := connectorTasks(s)
		type summary struct {
			ID        string    `json:"id"`
			CreatedAt time.Time `json:"created_at"`
		}
		out := []summary{}
		if err == nil {
			for i := len(s.ExternalTasks) - 1; i >= 0; i-- {
				ref := s.ExternalTasks[i]
				if ref.Binding == binding {
					out = append(out, summary{ref.ID, ref.CreatedAt})
				}
			}
		}
		writeJSON(w, 200, map[string]any{"available": err == nil, "tasks": out})
	})
	m.HandleFunc("POST /api/external/tasks", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Message   string `json:"message"`
			RequestID string `json:"request_id"`
		}
		if err := decode(w, r, &in); err != nil {
			bad(w, err)
			return
		}
		if len(in.Message) > 16000 || strings.TrimSpace(in.Message) == "" || !connectorTaskID.MatchString(in.RequestID) {
			bad(w, errors.New("请确认任务说明及请求编号"))
			return
		}
		s := a.platform.snapshot()
		endpoint, binding, err := connectorTasks(s)
		if err != nil {
			bad(w, err)
			return
		}
		result, err := a.platform.callExternalTask(r.Context(), s, http.MethodPost, endpoint, in.RequestID, in.Message)
		if err != nil {
			bad(w, err)
			return
		}
		err = a.platform.change(func(next *State) error {
			for _, ref := range next.ExternalTasks {
				if ref.ID == result.ID && ref.Binding == binding {
					return nil
				}
			}
			next.ExternalTasks = append(next.ExternalTasks, externalTaskRef{ID: result.ID, Binding: binding, CreatedAt: time.Now().UTC()})
			if len(next.ExternalTasks) > 32 {
				next.ExternalTasks = next.ExternalTasks[len(next.ExternalTasks)-32:]
			}
			return nil
		})
		if err != nil {
			bad(w, errors.New("外部任务已提交，但记录未保存，可重试同一请求"))
			return
		}
		writeJSON(w, 202, result)
	})
	status := func(w http.ResponseWriter, r *http.Request) {
		s := a.platform.snapshot()
		endpoint, binding, err := connectorTasks(s)
		if err != nil {
			bad(w, err)
			return
		}
		id := r.PathValue("id")
		known := false
		for _, ref := range s.ExternalTasks {
			if ref.ID == id && ref.Binding == binding {
				known = true
				break
			}
		}
		if !known || !connectorTaskID.MatchString(id) {
			http.NotFound(w, r)
			return
		}
		result, err := a.platform.callExternalTask(r.Context(), s, r.Method, endpoint+"/"+id, "", "")
		if err != nil {
			bad(w, err)
			return
		}
		if result.ID != id {
			bad(w, errors.New("连接器返回了不同任务"))
			return
		}
		writeJSON(w, 200, result)
	}
	m.HandleFunc("GET /api/external/tasks/{id}", status)
	m.HandleFunc("DELETE /api/external/tasks/{id}", status)
}
