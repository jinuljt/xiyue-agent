package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
)

type capabilityError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *capabilityError) Error() string { return e.Message }

func capError(code, message string) error { return &capabilityError{code, message} }

type capabilityContext struct {
	platform   *Platform
	room       *roomAudio
	model      string
	firmware   string
	beforeRoom *roomAudioStatus
	grant      *capabilityGrant
}

type capability struct {
	ID          string             `json:"id"`
	Description string             `json:"description"`
	Input       *jsonschema.Schema `json:"input_schema"`
	Output      *jsonschema.Schema `json:"output_schema"`
	Write       bool               `json:"writes"`
	External    bool               `json:"-"`
	Voice       bool               `json:"-"`
	invoke      func(context.Context, capabilityContext, json.RawMessage) (any, error)
}

func capabilitySchema[T any]() *jsonschema.Schema {
	schema, err := jsonschema.For[T](nil)
	if err != nil {
		panic(err)
	}
	return schema
}

func deviceCapability[In, Out any](id, description string, write, external, voice bool, run func(context.Context, capabilityContext, In) (Out, error)) capability {
	return capability{ID: id, Description: description, Input: capabilitySchema[In](), Output: capabilitySchema[Out](), Write: write, External: external, Voice: voice,
		invoke: func(ctx context.Context, c capabilityContext, raw json.RawMessage) (any, error) {
			var in In
			if len(raw) == 0 {
				raw = json.RawMessage("{}")
			}
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.DisallowUnknownFields()
			if raw[0] != '{' || decoder.Decode(&in) != nil || decoder.Decode(new(any)) != io.EOF {
				return nil, capError("invalid_arguments", "工具参数格式不正确")
			}
			if err := ctx.Err(); err != nil {
				return nil, capError("cancelled", "调用已取消")
			}
			return run(ctx, c, in)
		},
	}
}

func (op capability) name() string { return "xiyue_" + strings.ReplaceAll(op.ID, ".", "_") }

type statusResult struct {
	Status     string    `json:"status"`
	ObservedAt time.Time `json:"observed_at"`
}

type deviceStatusResult struct {
	statusResult
	Model    string `json:"model,omitempty"`
	Firmware string `json:"firmware,omitempty"`
}

type monitorStatusResult struct {
	statusResult
	LastAudio  *time.Time `json:"last_audio_at,omitempty"`
	BeforeTurn string     `json:"before_voice_turn,omitempty"`
}

type capabilityPageInput struct {
	Limit  int    `json:"limit,omitempty"`
	Cursor string `json:"cursor,omitempty"`
}

type capabilityItem struct {
	ID     string     `json:"id"`
	Name   string     `json:"name"`
	Status string     `json:"status"`
	DueAt  *time.Time `json:"due_at,omitempty"`
}

type capabilityPage struct {
	Items      []capabilityItem `json:"items"`
	NextCursor string           `json:"next_cursor,omitempty"`
	ObservedAt time.Time        `json:"observed_at"`
}

type timerCapabilityInput struct {
	Seconds        int    `json:"seconds"`
	Name           string `json:"name,omitempty"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

type workflowCapabilityInput struct {
	WorkflowID     string `json:"workflow_id"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

type operationCapabilityInput struct {
	OperationID string `json:"operation_id"`
}

type capabilityOutcome struct {
	Status      string `json:"status"`
	OperationID string `json:"operation_id"`
	Step        int    `json:"step,omitempty"`
	Phase       string `json:"phase,omitempty"`
}

func paginateCapabilities(items []capabilityItem, in capabilityPageInput) (capabilityPage, error) {
	limit := in.Limit
	if limit == 0 {
		limit = 20
	}
	if limit < 1 || limit > 50 || len(in.Cursor) > 64 {
		return capabilityPage{}, capError("invalid_arguments", "每页可读取 1 至 50 条")
	}
	slices.SortFunc(items, func(a, b capabilityItem) int { return strings.Compare(a.ID, b.ID) })
	page := capabilityPage{Items: []capabilityItem{}, ObservedAt: time.Now().UTC()}
	for _, item := range items {
		if item.ID <= in.Cursor {
			continue
		}
		if len(page.Items) == limit {
			page.NextCursor = page.Items[len(page.Items)-1].ID
			break
		}
		page.Items = append(page.Items, item)
	}
	return page, nil
}

func listClocks(s State, alarms bool, in capabilityPageInput) (capabilityPage, error) {
	feature := "timers"
	if alarms {
		feature = "alarms"
	}
	if !featureEnabled(&s, feature) {
		return capabilityPage{}, capError("disabled", "该能力已停用")
	}
	var items []capabilityItem
	for _, sc := range s.Schedules {
		if (alarms && sc.AlarmName == "") || (!alarms && sc.AbilityID != quickTimer.ID) {
			continue
		}
		status, name, when := "disabled", sc.AlarmName, sc.NextAt
		if name == "" {
			name = strings.TrimSuffix(sc.Inputs["content"], "，计时结束。")
		}
		if sc.Enabled {
			status = "scheduled"
		}
		if sc.SnoozeAt != nil {
			status, when = "snoozed", *sc.SnoozeAt
		}
		items = append(items, capabilityItem{ID: sc.ID, Name: name, Status: status, DueAt: &when})
	}
	return paginateCapabilities(items, in)
}

var deviceCapabilities = []capability{
	deviceCapability("device.status", "查询音箱的运行状态、型号和固件", false, true, true,
		func(_ context.Context, c capabilityContext, _ struct{}) (deviceStatusResult, error) {
			return deviceStatusResult{statusResult{"running", time.Now().UTC()}, c.model, c.firmware}, nil
		}),
	deviceCapability("audio.monitor.status", "查询远程听音、监听或收音的实时状态，区分正在播放、暂停让行、重连与停止", false, true, true,
		func(_ context.Context, c capabilityContext, _ struct{}) (monitorStatusResult, error) {
			status := c.room.runtimeSnapshot()
			if !c.room.available() {
				return monitorStatusResult{}, capError("unavailable", "远程听音运行时不可用")
			}
			result := monitorStatusResult{statusResult: statusResult{status.Stage, time.Now().UTC()}, LastAudio: status.LastAudio}
			if c.beforeRoom != nil {
				result.BeforeTurn = c.beforeRoom.Stage
			}
			return result, nil
		}),
	deviceCapability("timer.list", "查询音箱上的倒计时及预计结束时间", false, true, true,
		func(_ context.Context, c capabilityContext, in capabilityPageInput) (capabilityPage, error) {
			return listClocks(c.platform.snapshot(), false, in)
		}),
	deviceCapability("alarm.list", "查询音箱闹钟和下次响铃或稍后提醒时间", false, true, true,
		func(_ context.Context, c capabilityContext, in capabilityPageInput) (capabilityPage, error) {
			return listClocks(c.platform.snapshot(), true, in)
		}),
	deviceCapability("todo.list", "查询音箱待办事项和完成状态", false, true, true,
		func(_ context.Context, c capabilityContext, in capabilityPageInput) (capabilityPage, error) {
			var items []capabilityItem
			for _, task := range c.platform.snapshot().Tasks {
				if task.AbilityID == "todo" {
					items = append(items, capabilityItem{ID: task.ID, Name: task.Title, Status: task.Status, DueAt: task.DueAt})
				}
			}
			return paginateCapabilities(items, in)
		}),
	deviceCapability("automation.list", "查询音箱自动化流程和启用状态", false, true, true,
		func(_ context.Context, c capabilityContext, in capabilityPageInput) (capabilityPage, error) {
			var items []capabilityItem
			for _, rule := range c.platform.snapshot().Rules {
				status := "disabled"
				if rule.Schedule.Enabled {
					status = "enabled"
				}
				items = append(items, capabilityItem{ID: rule.ID, Name: rule.Name, Status: status})
			}
			return paginateCapabilities(items, in)
		}),
	deviceCapability("timer.create", "创建音箱倒计时，seconds 为 1 至 86400，外部调用须带幂等键", true, true, false,
		func(_ context.Context, c capabilityContext, in timerCapabilityInput) (capabilityOutcome, error) {
			name := strings.TrimSpace(in.Name)
			if in.Seconds < 1 || in.Seconds > 86400 || len(name) > 120 {
				return capabilityOutcome{}, capError("invalid_arguments", "倒计时须为 1 至 86400 秒，名称最多 120 字节")
			}
			return c.mutate("timer.create", in.IdempotencyKey, struct {
				Seconds int
				Name    string
			}{in.Seconds, name},
				func(s *State, now time.Time, id string) (string, error) {
					if !featureEnabled(s, "timers") {
						return "", capError("disabled", "倒计时已停用")
					}
					return "completed", addVoiceSchedule(s, quickTimer.ID, timerAnnouncement(name), time.Duration(in.Seconds)*time.Second, now, id)
				})
		}),
	deviceCapability("automation.run", "启动一条已启用流程，外部调用须授予该流程并提供幂等键", true, true, false,
		func(ctx context.Context, c capabilityContext, in workflowCapabilityInput) (capabilityOutcome, error) {
			if !validCapabilityID(in.WorkflowID) {
				return capabilityOutcome{}, capError("invalid_arguments", "请提供有效流程 ID")
			}
			s := c.platform.snapshot()
			i := slices.IndexFunc(s.Rules, func(rule automationRule) bool { return rule.ID == in.WorkflowID })
			if i < 0 {
				return capabilityOutcome{}, capError("not_found", "流程不存在")
			}
			if c.grant != nil && c.grant.Workflows[in.WorkflowID] != workflowFingerprint(s.Rules[i]) {
				return capabilityOutcome{}, capError("forbidden", "流程未获授权或已改动")
			}
			observations := c.platform.observeAutomationStates(ctx, s, c.room, automationConditionSources(s.Rules[i]))
			return c.mutate("automation.run", in.IdempotencyKey, in.WorkflowID,
				func(next *State, now time.Time, id string) (string, error) {
					current := slices.IndexFunc(next.Rules, func(rule automationRule) bool { return rule.ID == in.WorkflowID })
					if current < 0 || workflowFingerprint(next.Rules[current]) != workflowFingerprint(s.Rules[i]) {
						return "", capError("conflict", "流程已修改，请重新读取后再试")
					}
					if !next.Rules[current].Schedule.Enabled {
						return "", capError("disabled", "请先启用此流程")
					}
					run := automationRun{ID: id, At: now, Source: "manual", Status: "running", Step: 1}
					if err := appendAutomationRun(next, in.WorkflowID, &run, observations, nil); err != nil {
						return "", err
					}
					if run.Status == "skipped" {
						return "skipped", nil
					}
					return "accepted", nil
				})
		}),
	deviceCapability("operation.get", "根据操作 ID 查询自动化运行状态或已创建的倒计时", false, true, true,
		func(_ context.Context, c capabilityContext, in operationCapabilityInput) (capabilityOutcome, error) {
			if !validCapabilityID(in.OperationID) {
				return capabilityOutcome{}, capError("invalid_arguments", "请提供有效操作 ID")
			}
			s := c.platform.snapshot()
			if c.grant != nil && !slices.ContainsFunc(s.CapabilityReceipts, func(receipt capabilityReceipt) bool {
				return receipt.Grant == c.grant.ID && receipt.Outcome.OperationID == in.OperationID && receipt.ExpiresAt.After(time.Now())
			}) {
				return capabilityOutcome{}, capError("not_found", "操作不存在")
			}
			for _, rule := range s.Rules {
				for _, run := range rule.Runs {
					if run.ID == in.OperationID {
						return capabilityOutcome{Status: run.Status, OperationID: run.ID, Step: run.Step, Phase: run.Phase}, nil
					}
				}
			}
			for _, sc := range s.Schedules {
				if sc.ID == in.OperationID && sc.AbilityID == quickTimer.ID {
					return capabilityOutcome{Status: "completed", OperationID: sc.ID}, nil
				}
			}
			return capabilityOutcome{}, capError("not_found", "操作不存在或记录已过期")
		}),
}

func (c capabilityContext) call(ctx context.Context, id string, input json.RawMessage) (any, error) {
	for _, op := range deviceCapabilities {
		if op.ID == id {
			if c.grant != nil && (!op.External || !slices.Contains(c.grant.Operations, id)) {
				return nil, capError("forbidden", "未授权此操作")
			}
			return op.invoke(ctx, c, bytes.TrimSpace(input))
		}
	}
	return nil, capError("not_found", "未知设备能力")
}

func capabilityFailure(err error) *capabilityError {
	var problem *capabilityError
	if errors.As(err, &problem) {
		return problem
	}
	return &capabilityError{"execution_failed", fmt.Sprintf("设备能力执行失败：%s", err)}
}
