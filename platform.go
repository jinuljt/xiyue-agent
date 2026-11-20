package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"
)

// Extensions are declarative: they cannot execute shell commands or access files.
type Field struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Kind     string `json:"kind"` // text, number, credential_ref
	Required bool   `json:"required"`
	Default  string `json:"default,omitempty"`
}
type Step struct {
	Kind    string `json:"kind"` // say, confirm, timer
	Text    string `json:"text"`
	Seconds int    `json:"seconds,omitempty"`
}
type Manifest struct {
	ID          string  `json:"id"`
	Version     string  `json:"version"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Fields      []Field `json:"fields"`
	Steps       []Step  `json:"steps"`
}
type Ability struct {
	Manifest Manifest          `json:"manifest"`
	Enabled  bool              `json:"enabled"`
	Config   map[string]string `json:"config"`
}
type Task struct {
	ID           string     `json:"id"`
	AbilityID    string     `json:"ability_id"`
	Title        string     `json:"title"`
	Steps        []Step     `json:"steps"`
	Index        int        `json:"index"`
	Status       string     `json:"status"`
	CreatedAt    time.Time  `json:"created_at"`
	DueAt        *time.Time `json:"due_at,omitempty"`
	Remaining    float64    `json:"remaining_seconds,omitempty"`
	ResumeStatus string     `json:"resume_status,omitempty"`
}
type Event struct {
	ID     string    `json:"id"`
	TaskID string    `json:"task_id"`
	Text   string    `json:"text"`
	Kind   string    `json:"kind"`
	At     time.Time `json:"at"`
}
type Schedule struct {
	ID        string            `json:"id"`
	AbilityID string            `json:"ability_id"`
	Inputs    map[string]string `json:"inputs"`
	NextAt    time.Time         `json:"next_at"`
	Repeat    string            `json:"repeat"` // once, daily, weekdays
	Timezone  string            `json:"timezone"`
	Enabled   bool              `json:"enabled"`
}
type State struct {
	Version   int                `json:"version"`
	Abilities map[string]Ability `json:"abilities"`
	Tasks     []Task             `json:"tasks"`
	Schedules []Schedule         `json:"schedules"`
	Events    []Event            `json:"events"`
	Settings  SystemConfig       `json:"settings"`
	Sealed    map[string]string  `json:"sealed,omitempty"`
	Usage     []UsageRecord      `json:"usage"`
}
type Platform struct {
	mu    sync.Mutex
	path  string
	state State
}

var identifier = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,47}$`)
var placeholder = regexp.MustCompile(`\{\{([a-z][a-z0-9_-]*)\}\}`)

func newID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func validateManifest(m Manifest) error {
	if !identifier.MatchString(m.ID) || len(m.Name) == 0 || len(m.Name) > 120 || len(m.Version) == 0 || len(m.Version) > 32 || len(m.Description) > 2000 || len(m.Steps) == 0 || len(m.Steps) > 64 || len(m.Fields) > 32 {
		return errors.New("能力标识、名称、版本或步骤数量不合法")
	}
	fields := map[string]Field{}
	for _, f := range m.Fields {
		if !identifier.MatchString(f.Key) || f.Label == "" || len(f.Label) > 120 || len(f.Default) > 2000 {
			return errors.New("配置字段不合法")
		}
		if _, ok := fields[f.Key]; ok {
			return errors.New("配置字段重复")
		}
		if f.Kind != "text" && f.Kind != "number" && f.Kind != "credential_ref" && f.Kind != "secret" {
			return errors.New("不支持的配置字段类型")
		}
		if (f.Kind == "credential_ref" || f.Kind == "secret") && f.Default != "" {
			return errors.New("凭证引用不能提供默认值")
		}
		fields[f.Key] = f
	}
	for _, s := range m.Steps {
		if s.Kind != "say" && s.Kind != "confirm" && s.Kind != "timer" {
			return errors.New("首版仅支持播报、确认和计时步骤")
		}
		if s.Text == "" || len(s.Text) > 4000 || (s.Kind == "timer" && (s.Seconds < 1 || s.Seconds > 604800)) || (s.Kind != "timer" && s.Seconds != 0) {
			return errors.New("步骤文字或计时时长不合法")
		}
		for _, match := range placeholder.FindAllStringSubmatch(s.Text, -1) {
			f, ok := fields[match[1]]
			if !ok || f.Kind == "credential_ref" || f.Kind == "secret" {
				return errors.New("步骤引用了未知字段或凭证；凭证不能用于播报")
			}
		}
		if strings.Contains(placeholder.ReplaceAllString(s.Text, ""), "{{") {
			return errors.New("不合法的变量表达式")
		}
	}
	return nil
}

func openPlatform(path string) (*Platform, error) {
	p := &Platform{path: path}
	data, err := os.ReadFile(path)
	if err == nil {
		if err = json.Unmarshal(data, &p.state); err != nil {
			return nil, fmt.Errorf("状态文件损坏，停止启动以保护数据: %w", err)
		}
		if p.state.Version != 1 || p.state.Abilities == nil {
			return nil, errors.New("不支持的状态格式")
		}
		if p.state.Settings.Revision == 0 {
			p.state.Settings = defaultSettings()
		}
		return p, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	p.state = State{Version: 1, Abilities: map[string]Ability{}, Tasks: []Task{}, Schedules: []Schedule{}, Events: []Event{}}
	p.state.Settings = defaultSettings()
	for _, m := range builtinManifests() {
		if err = validateManifest(m); err != nil {
			return nil, err
		}
		p.state.Abilities[m.ID] = Ability{Manifest: m, Enabled: true, Config: map[string]string{}}
	}
	err = p.change(func(s *State) error { return nil })
	return p, err
}

// ponytail: one small state document under one lock; use a database when history exceeds these limits.
// Write a copy first so a failed disk write never advances the in-memory task.
func (p *Platform) change(fn func(*State) error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	data, _ := json.Marshal(p.state)
	var next State
	if err := json.Unmarshal(data, &next); err != nil {
		return err
	}
	if err := fn(&next); err != nil {
		return err
	}
	if len(next.Events) > 200 {
		next.Events = next.Events[len(next.Events)-200:]
	}
	if len(next.Usage) > 500 {
		next.Usage = next.Usage[len(next.Usage)-500:]
	}
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(p.path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(p.path), ".state-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, p.path); err != nil {
		return err
	}
	p.state = next
	return nil
}
func (p *Platform) snapshot() State {
	p.mu.Lock()
	defer p.mu.Unlock()
	data, _ := json.Marshal(p.state)
	var result State
	_ = json.Unmarshal(data, &result)
	return result
}
func configValues(a Ability, overrides map[string]string) (map[string]string, error) {
	values := map[string]string{}
	known := map[string]Field{}
	for _, f := range a.Manifest.Fields {
		known[f.Key] = f
		values[f.Key] = f.Default
	}
	for k, v := range a.Config {
		values[k] = v
	}
	for k, v := range overrides {
		f, ok := known[k]
		if !ok || f.Kind == "credential_ref" || f.Kind == "secret" {
			return nil, errors.New("未知参数或不可覆盖的凭证引用")
		}
		values[k] = v
	}
	for k, v := range values {
		f, ok := known[k]
		if !ok || len(v) > 2000 {
			return nil, errors.New("配置项不合法")
		}
		if f.Required && strings.TrimSpace(v) == "" {
			return nil, fmt.Errorf("请配置%s", f.Label)
		}
		if f.Kind == "credential_ref" && v != "" && !strings.HasPrefix(v, "bitwarden:item:") {
			return nil, errors.New("只能保存 Bitwarden 条目引用，不能填写明文密钥")
		}
		if f.Kind == "number" && v != "" {
			var n json.Number
			if err := json.Unmarshal([]byte(v), &n); err != nil {
				return nil, errors.New("数字配置项不合法")
			}
			if _, err := n.Float64(); err != nil {
				return nil, errors.New("数字配置项不合法")
			}
		}
	}
	return values, nil
}
func emit(s *State, t *Task, kind, text string, now time.Time) {
	s.Events = append(s.Events, Event{ID: newID(), TaskID: t.ID, Kind: kind, Text: text, At: now})
}
func advance(s *State, t *Task, now time.Time) {
	t.DueAt = nil
	t.ResumeStatus = ""
	t.Remaining = 0
	for t.Index < len(t.Steps) {
		step := t.Steps[t.Index]
		emit(s, t, "speech", step.Text, now)
		switch step.Kind {
		case "confirm":
			t.Status = "waiting"
			return
		case "timer":
			due := now.Add(time.Duration(step.Seconds) * time.Second)
			t.DueAt = &due
			t.Status = "timer"
			return
		}
		t.Index++
	}
	t.Status = "done"
	emit(s, t, "status", "任务已完成", now)
}
func startTask(s *State, abilityID string, inputs map[string]string, now time.Time) (string, error) {
	a, ok := s.Abilities[abilityID]
	if !ok || !a.Enabled {
		return "", errors.New("能力不存在或未启用")
	}
	values, err := configValues(a, inputs)
	if err != nil {
		return "", err
	}
	active := 0
	for _, t := range s.Tasks {
		if t.Status != "done" && t.Status != "cancelled" {
			active++
		}
	}
	if active >= 32 {
		return "", errors.New("同时进行的任务不能超过32个")
	}
	if len(s.Tasks) >= 500 { // Keep active work; discard only the oldest finished record.
		removed := false
		for i, t := range s.Tasks {
			if t.Status == "done" || t.Status == "cancelled" {
				s.Tasks = append(s.Tasks[:i], s.Tasks[i+1:]...)
				removed = true
				break
			}
		}
		if !removed {
			return "", errors.New("任务记录已满")
		}
	}
	t := Task{ID: newID(), AbilityID: abilityID, Title: a.Manifest.Name, CreatedAt: now, Status: "running", Steps: append([]Step(nil), a.Manifest.Steps...)}
	for i := range t.Steps {
		t.Steps[i].Text = placeholder.ReplaceAllStringFunc(t.Steps[i].Text, func(match string) string { return values[match[2:len(match)-2]] })
	}
	advance(s, &t, now)
	s.Tasks = append(s.Tasks, t)
	return t.ID, nil
}
func taskAction(s *State, id, action string, now time.Time) error {
	for i := range s.Tasks {
		t := &s.Tasks[i]
		if t.ID != id {
			continue
		}
		switch action {
		case "confirm":
			if t.Status != "waiting" {
				return errors.New("此任务当前不在等待确认")
			}
			t.Index++
			advance(s, t, now)
		case "pause":
			if t.Status != "waiting" && t.Status != "timer" {
				return errors.New("当前状态不能暂停")
			}
			t.ResumeStatus = t.Status
			if t.DueAt != nil {
				t.Remaining = t.DueAt.Sub(now).Seconds()
				if t.Remaining < 0 {
					t.Remaining = 0
				}
			}
			t.DueAt = nil
			t.Status = "paused"
		case "resume":
			if t.Status != "paused" {
				return errors.New("任务未暂停")
			}
			t.Status = t.ResumeStatus
			t.ResumeStatus = ""
			if t.Status == "timer" {
				due := now.Add(time.Duration(t.Remaining * float64(time.Second)))
				t.DueAt = &due
			}
			t.Remaining = 0
		case "cancel":
			if t.Status == "done" || t.Status == "cancelled" {
				return errors.New("任务已经结束")
			}
			t.Status = "cancelled"
			t.DueAt = nil
			t.Remaining = 0
			t.ResumeStatus = ""
		case "repeat":
			if t.Status == "done" || t.Status == "cancelled" {
				return errors.New("任务已经结束")
			}
			emit(s, t, "speech", t.Steps[t.Index].Text, now)
		default:
			return errors.New("不支持的任务操作")
		}
		return nil
	}
	return errors.New("任务不存在")
}
func validateSchedule(s *State, sc Schedule, now time.Time) error {
	a, ok := s.Abilities[sc.AbilityID]
	if !ok || !a.Enabled {
		return errors.New("能力不存在或未启用")
	}
	if _, err := configValues(a, sc.Inputs); err != nil {
		return err
	}
	if sc.NextAt.Before(now) || sc.NextAt.After(now.AddDate(2, 0, 0)) {
		return errors.New("请选择未来两年内的时间")
	}
	if sc.Repeat != "once" && sc.Repeat != "daily" && sc.Repeat != "weekdays" {
		return errors.New("不支持的重复规则")
	}
	loc, err := time.LoadLocation(sc.Timezone)
	if err != nil {
		return errors.New("时区无效")
	}
	day := sc.NextAt.In(loc).Weekday()
	if sc.Repeat == "weekdays" && (day == time.Saturday || day == time.Sunday) {
		return errors.New("工作日计划的首次执行时间应为周一至周五")
	}
	return nil
}
func tick(s *State, now time.Time) {
	for i := range s.Tasks {
		t := &s.Tasks[i]
		if t.Status == "timer" && t.DueAt != nil && !now.Before(*t.DueAt) {
			emit(s, t, "speech", "计时结束："+t.Title, now)
			t.Index++
			advance(s, t, now)
		}
	}
	for i := range s.Schedules {
		sc := &s.Schedules[i]
		if !sc.Enabled || now.Before(sc.NextAt) {
			continue
		}
		_, err := startTask(s, sc.AbilityID, sc.Inputs, now)
		if err != nil {
			s.Events = append(s.Events, Event{ID: newID(), Kind: "error", Text: "定时任务未执行：" + err.Error(), At: now})
		}
		if sc.Repeat == "once" {
			sc.Enabled = false
			continue
		}
		loc, _ := time.LoadLocation(sc.Timezone)
		next := sc.NextAt.In(loc)
		for !next.After(now) || (sc.Repeat == "weekdays" && (next.Weekday() == time.Saturday || next.Weekday() == time.Sunday)) {
			next = next.AddDate(0, 0, 1)
		}
		sc.NextAt = next
	}
}
func (p *Platform) tick(now time.Time) error {
	// Avoid flash writes on every timer tick: persist only actual transitions.
	p.mu.Lock()
	due := false
	for _, t := range p.state.Tasks {
		if t.Status == "timer" && t.DueAt != nil && !now.Before(*t.DueAt) {
			due = true
		}
	}
	for _, sc := range p.state.Schedules {
		if sc.Enabled && !now.Before(sc.NextAt) {
			due = true
		}
	}
	p.mu.Unlock()
	if !due {
		return nil
	}
	return p.change(func(s *State) error { tick(s, now); return nil })
}

func builtinManifests() []Manifest {
	return []Manifest{
		{ID: "todo", Version: "0.1.0", Name: "语音待办", Description: "记录一件事，确认完成后才结束。", Fields: []Field{{Key: "content", Label: "待办内容", Kind: "text", Required: true}}, Steps: []Step{{Kind: "confirm", Text: "待办：{{content}}。完成后请确认。"}, {Kind: "say", Text: "已完成：{{content}}。"}}},
		{ID: "reminder", Version: "0.1.0", Name: "定时提醒", Description: "到指定时间显示提醒，可单次、每天或工作日执行。", Fields: []Field{{Key: "content", Label: "提醒内容", Kind: "text", Required: true}}, Steps: []Step{{Kind: "confirm", Text: "提醒：{{content}}。请确认已处理。"}}},
		{ID: "guided-flow", Version: "0.1.0", Name: "分步引导示例", Description: "演示确认、倒计时和下一步；仅在控制台预览，不会发出声音。", Fields: []Field{{Key: "topic", Label: "引导主题", Kind: "text", Required: true, Default: "准备一杯茶"}}, Steps: []Step{{Kind: "confirm", Text: "开始{{topic}}。准备好了请确认，随后开始十秒计时。"}, {Kind: "timer", Text: "计时十秒，现在开始。", Seconds: 10}, {Kind: "confirm", Text: "时间到了，完成后请确认。"}, {Kind: "say", Text: "{{topic}}已完成。"}}},
	}
}
