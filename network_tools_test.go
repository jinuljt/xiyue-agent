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
)

func TestNetworkToolsAPIAndAutomation(t *testing.T) {
	const token = "test-ha-bearer"
	actions := make(chan string, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", 401)
			return
		}
		switch r.URL.Path {
		case "/api/states":
			fmt.Fprint(w, `[{"entity_id":"sensor.test","state":"private","attributes":{"friendly_name":"温度"}},{"entity_id":"light.test","attributes":{}}]`)
		case "/api/states/sensor.test":
			fmt.Fprint(w, `{"state":"23","attributes":{"unit_of_measurement":"°C"}}`)
		case "/api/services/light/turn_on":
			if r.Method != "POST" {
				t.Error("expected POST")
			}
			b, _ := io.ReadAll(r.Body)
			actions <- string(b)
			w.WriteHeader(204)
		case "/air":
			if r.URL.Query().Get("city") != "北京" || r.URL.Query().Get("limit") != "3" {
				t.Error("wrong query")
			}
			fmt.Fprint(w, `{"data":{"aqi":35},"other":"unused"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	p, err := openPlatform(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := (&api{platform: p}).routes()
	call := func(method, path, body string, code int) string {
		t.Helper()
		r := httptest.NewRequest(method, "/api/network-tools"+path, strings.NewReader(body))
		r.Host = "localhost"
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Xiyue-Request", "console")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != code {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), token) {
			t.Fatal("credential exposed")
		}
		return w.Body.String()
	}
	if out := call("GET", "", "", 200); !strings.Contains(out, "net_example") {
		t.Fatal(out)
	}
	call("PUT", "/home-assistant", fmt.Sprintf(`{"endpoint":%q,"api_key":%q}`, upstream.URL, token), 200)
	entities := call("GET", "/home-assistant/entities", "", 200)
	if !strings.Contains(entities, "sensor.test") || strings.Contains(entities, "private") {
		t.Fatal(entities)
	}
	makeTool := func(body string) networkTool {
		t.Helper()
		var tool networkTool
		if err := json.Unmarshal([]byte(call("POST", "", body, 200)), &tool); err != nil {
			t.Fatal(err)
		}
		return tool
	}
	sensor := makeTool(`{"kind":"ha","name":"温度","description":"读温度","entity":"sensor.test","operation":"state","voice":true}`)
	s := p.snapshot()
	if out, err := p.runNetworkTool(context.Background(), s, sensor, nil); err != nil || out != "23 °C" {
		t.Fatalf("%q %v", out, err)
	}
	lamp := makeTool(`{"kind":"ha","name":"灯","description":"开灯","entity":"light.test","operation":"turn_on","automation":true}`)
	if err := p.runAutomationStep(nil, automationStep{Action: "http_tool", Value: lamp.ID}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := <-actions; got != `{"entity_id":"light.test"}` {
		t.Fatal(got)
	}
	body := fmt.Sprintf(`{"kind":"http","name":"空气质量","description":"查询空气质量","endpoint":%q,"method":"GET","api_key":%q,"extract":"data.aqi","parameters":[{"name":"city","type":"string"},{"name":"limit","type":"number"}]`, upstream.URL+"/air", token)
	if out := call("POST", "/test", body+`,"arguments":{"city":"北京","limit":"3"}}`, 200); !strings.Contains(out, `"35"`) {
		t.Fatal(out)
	}
	call("POST", "/test", body+`,"arguments":{"city":"北京","other":"3"}}`, 400)
	call("POST", "", strings.Replace(body, `"method":"GET"`, `"method":"POST","voice":true`, 1)+"}", 400)
	if out := call("GET", "", "", 200); !strings.Contains(out, `"ha_key":true`) {
		t.Fatal(out)
	}
	call("PUT", "/net_example", body+"}", 200)
	if tool, _ := findNetworkTool(&p.state, "net_example"); tool.Name != "空气质量" {
		t.Fatal("example did not use normal edit route")
	}
	call("DELETE", "/net_example", "", 200)
	reopened, err := openPlatform(p.path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := findNetworkTool(&reopened.state, "net_example"); ok {
		t.Fatal("deleted example was restored after restart")
	}
}

func TestVoiceSearchDispatchAndCost(t *testing.T) {
	oldClient := networkClient
	defer func() { networkClient = oldClient }()
	var searches int
	networkClient = &http.Client{Transport: networkRoundTrip(func(r *http.Request) (*http.Response, error) {
		searches++
		if r.URL.String() != searchOrigin+"/v1/tools/search" || r.Header.Get("Authorization") != "Bearer test-deepseek" {
			t.Error("invalid search request")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"results":[{"url":"https://example.com/news","title":"今天的公告","summary":"已发布新品"}]}`))}, nil
	})}
	var modelCalls int
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modelCalls++
		var req struct{ Messages []voiceMessage }
		_ = json.NewDecoder(r.Body).Decode(&req)
		reply := `{"action":"tool","tool":"web_search","arguments":{"query":"今天的公告"}}`
		if modelCalls == 1 && !strings.Contains(req.Messages[0].Content, "web_search") {
			t.Error("search missing from tools")
		}
		if modelCalls == 2 {
			if !strings.Contains(req.Messages[len(req.Messages)-1].Content, "已发布新品") {
				t.Error("search result missing")
			}
			reply = `{"action":"speak","speech":"今天的公告确认已发布新品。"}`
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": reply}}}})
	}))
	defer model.Close()
	p, err := openPlatform(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.change(func(s *State) error {
		s.Settings.LLM = ServiceConfig{Provider: "custom", Endpoint: model.URL, Model: "test"}
		s.Search.Enabled = true
		return p.putNetworkKey(s, searchSecret, "test-deepseek", false)
	}); err != nil {
		t.Fatal(err)
	}
	var plan voicePlan
	if _, err := p.voiceReply(context.Background(), p.snapshot(), "今天的公告是什么？", replyOptions{voicePlan: &plan}); err != nil || plan.Speech != "今天的公告确认已发布新品。" {
		t.Fatalf("%+v %v", plan, err)
	}
	if modelCalls != 2 || searches != 1 {
		t.Fatalf("model=%d search=%d", modelCalls, searches)
	}
	for _, row := range p.activity.recent() {
		if row.Service == "search" {
			if row.Estimated == nil || *row.Estimated != 0.005 || row.BillableRequest == nil || !*row.BillableRequest {
				t.Fatalf("unexpected cost %+v", row)
			}
			return
		}
	}
	t.Fatal("search usage missing")
}

type networkRoundTrip func(*http.Request) (*http.Response, error)

func (fn networkRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

func TestWeatherTemplateFixedQueryAndUnits(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("latitude") != "31.23" || r.URL.Query().Get("current") != "temperature_2m" {
			t.Errorf("unexpected query %v", r.URL.Query())
		}
		io.WriteString(w, `{"current":{"temperature_2m":19}}`)
	}))
	defer upstream.Close()
	tool := networkTool{Kind: "http", Name: "上海当前气温", Description: "查询气温", Method: "GET",
		Endpoint: upstream.URL + "?latitude=31.23&current=temperature_2m", Extract: "current.temperature_2m", Unit: "°C"}
	if err := tool.validate(); err != nil {
		t.Fatal(err)
	}
	if got, err := (&Platform{}).executeNetworkTool(context.Background(), State{}, tool, nil, ""); err != nil || got.text != "19 °C" || string(got.Value) != "19" {
		t.Fatalf("%q %v", got, err)
	}
	tool.Endpoint += "&api_key=fixture"
	if tool.validate() == nil {
		t.Fatal("secret in URL accepted")
	}
}
