package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestCapabilitiesUseTheSameStateAcrossHTTPAndMCP(t *testing.T) {
	p, err := openPlatform(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = p.installRuleExamples(); err != nil {
		t.Fatal(err)
	}
	rule := p.snapshot().Rules[0]
	room := newRoomAudio(context.Background(), &voiceRuntime{playPCM: func(context.Context, io.Reader) error { return nil }})
	room.status.Stage = "reconnecting"
	gate := &adminGate{}
	a := &api{platform: p, room: room, admin: gate}
	grant, token, err := a.grantCapabilityAccess(capabilityGrantInput{Name: "fixture", Operations: []string{
		"audio.monitor.status", "timer.create", "timer.list", "automation.run", "operation.get",
	}, WorkflowIDs: []string{rule.ID}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(a.routes())
	gate.host = server.Listener.Addr().String()
	server.StartTLS()
	defer server.Close()
	request := func(method, path, body, bearer string, want int) []byte {
		t.Helper()
		req, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, err := io.ReadAll(resp.Body)
		if resp.StatusCode != want || err != nil {
			t.Fatalf("%s: %d %s (%v)", path, resp.StatusCode, out, err)
		}
		return out
	}
	request("GET", "/api/v1/capabilities", "", "", 401)
	request("GET", "/api/capability-grants", "", token, 401)
	request("POST", "/api/v1/capabilities/device.status/invoke", `{"arguments":{}}`, token, 403)
	mcpClient := *server.Client()
	transport := mcpClient.Transport
	mcpClient.Transport = networkRoundTrip(func(r *http.Request) (*http.Response, error) {
		copy := r.Clone(r.Context())
		copy.Header.Set("Authorization", "Bearer "+token)
		return transport.RoundTrip(copy)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp", HTTPClient: &mcpClient}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	listed, err := session.ListTools(ctx, nil)
	if err != nil || len(listed.Tools) != len(grant.Operations) {
		t.Fatalf("unexpected discovery: %+v %v", listed, err)
	}
	monitor, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "xiyue_audio_monitor_status"})
	if err != nil || monitor.IsError {
		t.Fatalf("MCP query: %+v %v", monitor, err)
	}
	encoded, _ := json.Marshal(monitor.StructuredContent)
	rest := request("POST", "/api/v1/capabilities/audio.monitor.status/invoke", `{"arguments":{}}`, token, 200)
	if !strings.Contains(string(encoded), `"status":"reconnecting"`) || !strings.Contains(string(rest), `"status":"reconnecting"`) {
		t.Fatal("the same live status was not returned")
	}
	write := `{"arguments":{"seconds":60,"name":"fixture","idempotency_key":"one-timer"}}`
	first := request("POST", "/api/v1/capabilities/timer.create/invoke", write, token, 200)
	second := request("POST", "/api/v1/capabilities/timer.create/invoke", write, token, 200)
	if string(first) != string(second) || len(p.snapshot().Schedules) != 1 {
		t.Fatal("retry created a duplicate timer")
	}
	request("POST", "/api/v1/capabilities/timer.create/invoke", strings.Replace(write, "60", "90", 1), token, 409)
	payload := fmt.Sprintf(`{"arguments":{"workflow_id":%q,"idempotency_key":"first-run"}}`, rule.ID)
	var run capabilityOutcome
	if err := json.Unmarshal(request("POST", "/api/v1/capabilities/automation.run/invoke", payload, token, 202), &run); err != nil || run.OperationID == "" {
		t.Fatal("run ID missing")
	}
	state := request("GET", "/api/v1/operations/"+run.OperationID, "", token, 200)
	if !strings.Contains(string(state), `"status":"running"`) {
		t.Fatal("cannot query accepted run")
	}
	if err := p.change(func(s *State) error { s.CapabilityGrants = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	request("GET", "/api/v1/capabilities", "", token, 401)
}

func TestVoiceLooksUpDeviceMonitorThroughCapability(t *testing.T) {
	var calls int
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var response string
		if calls == 0 {
			if !strings.Contains(string(body), "xiyue_audio_monitor_status") {
				t.Error("device tool missing")
			}
			response = `{"action":"tool","tool":"xiyue_audio_monitor_status","arguments":{}}`
		} else {
			if !strings.Contains(string(body), "before_voice_turn") || !strings.Contains(string(body), "paused") || !strings.Contains(string(body), "playing") {
				t.Error("model did not receive both observations")
			}
			response = `{"action":"speak","speech":"刚才正在播放，这轮对话期间暂时让行。"}`
		}
		calls++
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": response}}}})
	}))
	defer llm.Close()
	p, err := openPlatform(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	p.room = newRoomAudio(context.Background(), &voiceRuntime{playPCM: func(context.Context, io.Reader) error { return nil }})
	p.room.status.Stage = "paused"
	s := p.snapshot()
	s.Settings.LLM = ServiceConfig{Provider: "custom", Endpoint: llm.URL, Model: "fixture"}
	var plan voicePlan
	_, err = p.voiceReply(context.Background(), s, "现在听音怎么样？", replyOptions{voicePlan: &plan, beforeRoom: &roomAudioStatus{Stage: "playing"}})
	if err != nil || calls != 2 || !strings.Contains(plan.Speech, "刚才正在播放") {
		t.Fatalf("voice capability failed: %v %+v", err, plan)
	}
}
