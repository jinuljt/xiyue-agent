package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

type networkParameter struct {
	Name string `json:"name"`
	Type string `json:"type"`
}
type networkTool struct {
	ID          string             `json:"id"`
	Name        string             `json:"name"`
	Description string             `json:"description"`
	Kind        string             `json:"kind"`
	Endpoint    string             `json:"endpoint,omitempty"`
	Method      string             `json:"method"`
	Entity      string             `json:"entity,omitempty"`
	Operation   string             `json:"operation,omitempty"`
	Extract     string             `json:"extract,omitempty"`
	Unit        string             `json:"unit,omitempty"`
	Parameters  []networkParameter `json:"parameters,omitempty"`
	Voice       bool               `json:"voice"`
	Automation  bool               `json:"automation"`
}
type networkInput struct {
	networkTool
	APIKey    string            `json:"api_key,omitempty"`
	ClearKey  bool              `json:"clear_key,omitempty"`
	Arguments map[string]string `json:"arguments,omitempty"`
}

const haSecret = "network:ha"

var entityID = regexp.MustCompile(`^[a-z0-9_]+\.[a-z0-9_]+$`)
var parameterID = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]{0,31}$`)
var resultPath = regexp.MustCompile(`^[a-zA-Z0-9_-]+(?:\.[a-zA-Z0-9_-]+)*$`)

func findNetworkTool(s *State, id string) (networkTool, bool) {
	i := slices.IndexFunc(s.HTTPTools, func(t networkTool) bool { return t.ID == id })
	if i < 0 {
		return networkTool{}, false
	}
	return s.HTTPTools[i], true
}
func (t networkTool) validate() error {
	if strings.TrimSpace(t.Name) == "" || len(t.Name) > 120 || strings.TrimSpace(t.Description) == "" || len(t.Description) > 500 {
		return errors.New("请填写工具名称和用途说明")
	}
	if t.Kind == "ha" {
		if len(t.Entity) > 128 || !entityID.MatchString(t.Entity) {
			return errors.New("请选择 Home Assistant 实体")
		}
		domain, _, _ := strings.Cut(t.Entity, ".")
		if t.Operation != "state" && ((domain != "light" && domain != "switch") || (t.Operation != "turn_on" && t.Operation != "turn_off")) {
			return errors.New("该实体不支持此操作")
		}
		return nil
	}
	if t.Kind != "http" || (t.Method != "GET" && t.Method != "POST") {
		return errors.New("请选择 GET 或 POST")
	}
	u, err := configuredURL(t.Endpoint)
	if err != nil {
		return err
	}
	if u.RawQuery != "" {
		if t.Method != "GET" {
			return errors.New("固定查询参数只适用于 GET 工具")
		}
		query, err := url.ParseQuery(u.RawQuery)
		if err != nil || len(query) > 16 {
			return errors.New("固定查询参数无效")
		}
		for name := range query {
			switch strings.ToLower(name) {
			case "key", "api_key", "apikey", "api-key", "auth", "authorization", "token", "access_token", "password", "secret", "signature":
				return errors.New("请使用凭证字段保存密钥")
			}
		}
	}
	if len(t.Unit) > 24 || strings.ContainsAny(t.Unit, "\r\n") {
		return errors.New("返回值单位过长或包含换行")
	}
	if len(t.Extract) > 100 || (t.Extract != "" && !resultPath.MatchString(t.Extract)) {
		return errors.New("返回字段请填写 JSON 路径，例如 data.value")
	}
	if len(t.Parameters) > 8 {
		return errors.New("最多配置 8 个参数")
	}
	seen := map[string]bool{}
	for _, v := range t.Parameters {
		if !parameterID.MatchString(v.Name) || seen[v.Name] || (v.Type != "string" && v.Type != "number" && v.Type != "boolean") {
			return errors.New("参数名须为唯一标识符，类型须为文本、数字或布尔值")
		}
		seen[v.Name] = true
	}
	return nil
}
func (t networkTool) values(args map[string]string) (map[string]any, error) {
	if len(args) != len(t.Parameters) {
		return nil, errors.New("工具参数不齐全或包含未知项")
	}
	values := make(map[string]any, len(args))
	for _, v := range t.Parameters {
		raw, ok := args[v.Name]
		if !ok || len(raw) > 1000 {
			return nil, errors.New("无效的工具参数：" + v.Name)
		}
		switch v.Type {
		case "number":
			if !json.Valid([]byte(raw)) {
				return nil, errors.New("请输入数字：" + v.Name)
			}
			if _, err := strconv.ParseFloat(raw, 64); err != nil {
				return nil, errors.New("请输入有效数字：" + v.Name)
			}
			values[v.Name] = json.Number(raw)
		case "boolean":
			if raw != "true" && raw != "false" {
				return nil, errors.New("请输入 true 或 false：" + v.Name)
			}
			values[v.Name] = raw == "true"
		default:
			values[v.Name] = raw
		}
	}
	return values, nil
}
func (p *Platform) networkKey(s State, name string) (string, error) {
	if s.Sealed[name] == "" {
		return "", nil
	}
	return p.unseal(name, s.Sealed[name])
}
func (p *Platform) putNetworkKey(s *State, name, key string, clear bool) error {
	if clear && key != "" {
		return errors.New("请只选择替换或清除凭证")
	}
	if clear {
		delete(s.Sealed, name)
	}
	if key == "" {
		return nil
	}
	if err := validateServiceKey(key); err != nil {
		return err
	}
	sealed, err := p.seal(name, key)
	if err != nil {
		return err
	}
	if s.Sealed == nil {
		s.Sealed = map[string]string{}
	}
	s.Sealed[name] = sealed
	return nil
}
func (p *Platform) executeNetworkTool(ctx context.Context, s State, t networkTool, args map[string]string, key string) (string, error) {
	values, err := t.values(args)
	if err != nil {
		return "", err
	}
	endpoint, method, extract := t.Endpoint, t.Method, t.Extract
	var payload any
	if t.Kind == "ha" {
		if s.HomeAssistant == "" || key == "" {
			return "", errors.New("请先连接 Home Assistant")
		}
		endpoint, method, extract = s.HomeAssistant+"/api/states/"+t.Entity, http.MethodGet, "state"
		if t.Operation != "state" {
			domain, _, _ := strings.Cut(t.Entity, ".")
			endpoint, method, extract = s.HomeAssistant+"/api/services/"+domain+"/"+t.Operation, http.MethodPost, ""
			payload = map[string]string{"entity_id": t.Entity}
		}
	} else if method == http.MethodGet {
		u, err := configuredURL(endpoint)
		if err != nil {
			return "", err
		}
		query := u.Query()
		for key, val := range values {
			query.Set(key, strings.TrimSpace(formatNetworkValue(val)))
		}
		u.RawQuery = query.Encode()
		endpoint = u.String()
	} else {
		payload = values
	}
	data, err := callNetwork(ctx, endpoint, method, key, payload, 128<<10)
	if err != nil {
		return "", err
	}
	if t.Kind == "ha" && t.Operation != "state" {
		return "Home Assistant 已接受操作。", nil
	}
	result, err := selectNetworkResult(data, extract)
	if err == nil && t.Kind == "ha" {
		if unit, e := selectNetworkResult(data, "attributes.unit_of_measurement"); e == nil && unit != "null" {
			result += " " + boundedText(unit, 40)
		}
	}
	if err == nil && t.Kind == "http" && result != "" && t.Unit != "" {
		result += " " + t.Unit
	}
	return result, err
}
func formatNetworkValue(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}
func (p *Platform) runNetworkTool(ctx context.Context, s State, t networkTool, args map[string]string) (string, error) {
	name := "network:" + t.ID
	if t.Kind == "ha" {
		name = haSecret
	}
	key, err := p.networkKey(s, name)
	if err != nil {
		return "", err
	}
	return p.executeNetworkTool(ctx, s, t, args, key)
}

type haEntity struct {
	ID         string `json:"entity_id"`
	Attributes struct {
		Name string `json:"friendly_name"`
	} `json:"attributes"`
}

type haEntityState struct {
	haEntity
	State string `json:"state"`
}

func (p *Platform) haEntities(ctx context.Context, s State) ([]haEntityState, error) {
	key, err := p.networkKey(s, haSecret)
	if err != nil {
		return nil, err
	}
	if s.HomeAssistant == "" || key == "" {
		return nil, errors.New("请先保存 Home Assistant 地址和访问令牌")
	}
	data, err := callNetwork(ctx, s.HomeAssistant+"/api/states", http.MethodGet, key, nil, 2<<20)
	if err != nil {
		return nil, err
	}
	entities := []haEntityState{}
	if json.Unmarshal(data, &entities) != nil {
		return nil, errors.New("Home Assistant 实体列表格式异常")
	}
	entities = slices.DeleteFunc(entities, func(e haEntityState) bool { return len(e.ID) > 128 || !entityID.MatchString(e.ID) })
	for i := range entities {
		entities[i].Attributes.Name = boundedText(entities[i].Attributes.Name, 100)
	}
	return entities, nil
}
func (a *api) networkToolRoutes(m *http.ServeMux) {
	p := a.platform
	m.HandleFunc("GET /api/network-tools/example", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]string{"message": "喜悦 Voice 测试响应"})
	})
	m.HandleFunc("GET /api/network-tools", func(w http.ResponseWriter, r *http.Request) {
		s := p.snapshot()
		keyStatus := map[string]bool{}
		for _, t := range s.HTTPTools {
			keyStatus[t.ID] = s.Sealed["network:"+t.ID] != ""
		}
		writeJSON(w, 200, map[string]any{"search": s.Search, "search_key": s.Sealed[searchSecret] != "", "home_assistant": s.HomeAssistant, "ha_key": s.Sealed[haSecret] != "", "tools": s.HTTPTools, "tool_keys": keyStatus})
	})
	m.HandleFunc("PUT /api/network-tools/search", func(w http.ResponseWriter, r *http.Request) {
		var in searchInput
		if err := decode(w, r, &in); err != nil {
			bad(w, err)
			return
		}
		err := p.change(func(s *State) error {
			if in.Enabled || in.APIKey != "" {
				if _, err := p.searchKey(*s, in.searchSettings, in.APIKey); err != nil {
					return err
				}
			}
			if err := p.putNetworkKey(s, searchSecret, in.APIKey, false); err != nil {
				return err
			}
			s.Search = in.searchSettings
			return nil
		})
		if err != nil {
			bad(w, err)
			return
		}
		writeJSON(w, 200, in.searchSettings)
	})
	m.HandleFunc("POST /api/network-tools/search/test", func(w http.ResponseWriter, r *http.Request) {
		var in searchInput
		if err := decode(w, r, &in); err != nil {
			bad(w, err)
			return
		}
		s := p.snapshot()
		key, err := p.searchKey(s, in.searchSettings, in.APIKey)
		if err != nil {
			bad(w, err)
			return
		}
		out, err := p.webTool(r.Context(), s, "search", in.Query, key, true)
		if err != nil {
			bad(w, err)
			return
		}
		writeJSON(w, 200, map[string]string{"result": out})
	})
	m.HandleFunc("PUT /api/network-tools/home-assistant", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Endpoint string `json:"endpoint"`
			APIKey   string `json:"api_key"`
		}
		if err := decode(w, r, &in); err != nil {
			bad(w, err)
			return
		}
		in.Endpoint = strings.TrimRight(strings.TrimSpace(in.Endpoint), "/")
		u, err := configuredURL(in.Endpoint)
		if err != nil || u.RawQuery != "" || u.Path != "" {
			bad(w, errors.New("请输入 Home Assistant 根地址，例如 http://192.168.1.20:8123"))
			return
		}
		err = p.change(func(s *State) error {
			if s.HomeAssistant != in.Endpoint && in.APIKey == "" {
				return errors.New("请填写此地址的访问令牌")
			}
			if s.HomeAssistant != "" && s.HomeAssistant != in.Endpoint && slices.ContainsFunc(s.HTTPTools, func(t networkTool) bool { return t.Kind == "ha" }) {
				return errors.New("请先移除旧连接的实体工具，再更换地址")
			}
			if err := p.putNetworkKey(s, haSecret, in.APIKey, false); err != nil {
				return err
			}
			s.HomeAssistant = in.Endpoint
			return nil
		})
		if err != nil {
			bad(w, err)
			return
		}
		writeJSON(w, 200, map[string]bool{"saved": true})
	})
	m.HandleFunc("GET /api/network-tools/home-assistant/entities", func(w http.ResponseWriter, r *http.Request) {
		res, err := p.haEntities(r.Context(), p.snapshot())
		if err != nil {
			bad(w, err)
			return
		}
		items := make([]haEntity, 0, len(res))
		for _, entity := range res {
			items = append(items, entity.haEntity)
		}
		writeJSON(w, 200, items)
	})
	build := func(in networkInput, id string) (networkTool, error) {
		t := in.networkTool
		t.ID = id
		t.Name, t.Description, t.Endpoint, t.Extract = strings.TrimSpace(t.Name), strings.TrimSpace(t.Description), strings.TrimSpace(t.Endpoint), strings.TrimSpace(t.Extract)
		if t.Kind == "ha" {
			t.Endpoint, t.Extract, t.Unit, t.Parameters = "", "", "", nil
			t.Method = http.MethodGet
			if t.Operation != "state" {
				t.Method = http.MethodPost
			}
			if in.APIKey != "" || in.ClearKey {
				return t, errors.New("请在 Home Assistant 连接中管理令牌")
			}
		} else {
			t.Entity, t.Operation = "", ""
		}
		if t.Method != http.MethodGet && t.Voice {
			return t, errors.New("写入工具可由自动化执行，请关闭语音授权")
		}
		return t, t.validate()
	}
	save := func(w http.ResponseWriter, r *http.Request) {
		var in networkInput
		if err := decode(w, r, &in); err != nil {
			bad(w, err)
			return
		}
		id := r.PathValue("id")
		t, err := build(in, id)
		if err != nil {
			bad(w, err)
			return
		}
		snapshot := p.snapshot()
		if t.Kind == "ha" {
			entities, err := p.haEntities(r.Context(), snapshot)
			if err != nil {
				bad(w, err)
				return
			}
			if !slices.ContainsFunc(entities, func(e haEntityState) bool { return e.ID == t.Entity }) {
				bad(w, errors.New("该实体已不存在"))
				return
			}
		}
		err = p.change(func(s *State) error {
			if t.Kind == "ha" && s.HomeAssistant != snapshot.HomeAssistant {
				return errors.New("连接已变化，请重新选择实体")
			}
			index := slices.IndexFunc(s.HTTPTools, func(item networkTool) bool { return item.ID == id })
			if id != "" && index < 0 {
				return errors.New("工具不存在")
			}
			if index >= 0 {
				old := s.HTTPTools[index]
				if old.Kind != t.Kind {
					return errors.New("无法更改工具类型")
				}
				if old.Endpoint != t.Endpoint && s.Sealed["network:"+id] != "" && in.APIKey == "" && !in.ClearKey {
					return errors.New("地址已更改，请替换或清除旧凭证")
				}
				if old.Endpoint != t.Endpoint || old.Entity != t.Entity || old.Operation != t.Operation || old.Method != t.Method || !slices.Equal(old.Parameters, t.Parameters) {
					for _, rule := range s.Rules {
						for _, step := range rule.Actions {
							if step.Action == "http_tool" && step.Value == id {
								return errors.New("工具已被自动化使用，请先移除规则中的对应动作")
							}
						}
					}
				}
			} else {
				if len(s.HTTPTools) >= 32 {
					return errors.New("最多保存 32 个工具")
				}
				t.ID = "net_" + newID()
			}
			if err := p.putNetworkKey(s, "network:"+t.ID, in.APIKey, in.ClearKey); err != nil {
				return err
			}
			if index >= 0 {
				s.HTTPTools[index] = t
			} else {
				s.HTTPTools = append(s.HTTPTools, t)
			}
			return nil
		})
		if err != nil {
			bad(w, err)
			return
		}
		writeJSON(w, 200, t)
	}
	m.HandleFunc("POST /api/network-tools", save)
	m.HandleFunc("PUT /api/network-tools/{id}", save)
	m.HandleFunc("DELETE /api/network-tools/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		err := p.change(func(s *State) error {
			if _, ok := findNetworkTool(s, id); !ok {
				return errors.New("工具不存在")
			}
			for _, rule := range s.Rules {
				for _, step := range rule.Actions {
					if step.Action == "http_tool" && step.Value == id {
						return errors.New("请先移除自动化中的对应动作")
					}
				}
			}
			s.HTTPTools = slices.DeleteFunc(s.HTTPTools, func(t networkTool) bool { return t.ID == id })
			delete(s.Sealed, "network:"+id)
			return nil
		})
		if err != nil {
			bad(w, err)
			return
		}
		writeJSON(w, 200, map[string]bool{"deleted": true})
	})
	m.HandleFunc("POST /api/network-tools/test", func(w http.ResponseWriter, r *http.Request) {
		var in networkInput
		if err := decode(w, r, &in); err != nil {
			bad(w, err)
			return
		}
		t, err := build(in, in.ID)
		if err != nil {
			bad(w, err)
			return
		}
		s := p.snapshot()
		key := in.APIKey
		if key != "" {
			err = validateServiceKey(key)
		}
		if t.Kind == "ha" {
			key, err = p.networkKey(s, haSecret)
		}
		if t.Kind == "http" && key == "" && !in.ClearKey && t.ID != "" {
			if old, ok := findNetworkTool(&s, t.ID); ok && old.Endpoint == t.Endpoint {
				key, err = p.networkKey(s, "network:"+t.ID)
			}
		}
		if err != nil {
			bad(w, err)
			return
		}
		out, err := p.executeNetworkTool(r.Context(), s, t, in.Arguments, key)
		if err != nil {
			bad(w, err)
			return
		}
		writeJSON(w, 200, map[string]string{"result": out})
	})
}
