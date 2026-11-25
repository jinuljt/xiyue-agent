package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
)

type capabilityGrant struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Operations []string          `json:"operations"`
	Workflows  map[string]string `json:"workflows,omitempty"`
	Digest     string            `json:"digest"`
	ExpiresAt  time.Time         `json:"expires_at"`
}

type capabilityReceipt struct {
	Grant     string            `json:"grant"`
	KeyHash   string            `json:"key_hash"`
	InputHash string            `json:"input_hash"`
	Outcome   capabilityOutcome `json:"outcome"`
	ExpiresAt time.Time         `json:"expires_at"`
}

var capabilityIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,80}$`)

func validCapabilityID(id string) bool { return capabilityIDPattern.MatchString(id) }

func hashCapabilityData(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func workflowFingerprint(rule automationRule) string {
	rule.Runs = nil
	// Usage and the clock's next occurrence change without a workflow edit.
	rule.Schedule.NextAt = time.Time{}
	data, _ := json.Marshal(rule)
	return hashCapabilityData(data)
}

func externalWorkflowAllowed(rule automationRule) bool {
	return !slices.ContainsFunc(rule.Actions, func(step automationStep) bool {
		switch step.Action {
		case "speak", "tone", "wait", "confirm", "if":
			return false
		case "tool":
			return step.Value != timerToolID
		default:
			return true
		}
	})
}

func (c capabilityContext) mutate(operation, key string, input any, apply func(*State, time.Time, string) (string, error)) (capabilityOutcome, error) {
	var outcome capabilityOutcome
	if c.grant != nil && !validCapabilityID(key) {
		return outcome, capError("invalid_arguments", "外部动作需要 1 至 80 位字母、数字、横线或下划线组成的幂等键")
	}
	digest, _ := json.Marshal(input)
	inputHash := hashCapabilityData(digest)
	keyHash := hashCapabilityData([]byte(operation + "." + key))
	now := time.Now().UTC()
	err := c.platform.change(func(s *State) error {
		if c.grant != nil {
			if !slices.ContainsFunc(s.CapabilityGrants, func(g capabilityGrant) bool {
				return g.ID == c.grant.ID && g.Digest == c.grant.Digest && g.ExpiresAt.After(now)
			}) {
				return capError("forbidden", "授权已撤销或过期")
			}
			s.CapabilityReceipts = slices.DeleteFunc(s.CapabilityReceipts, func(receipt capabilityReceipt) bool { return !receipt.ExpiresAt.After(now) })
			for _, receipt := range s.CapabilityReceipts {
				if receipt.Grant == c.grant.ID && receipt.KeyHash == keyHash {
					if receipt.InputHash != inputHash {
						return capError("conflict", "幂等键已用于不同参数")
					}
					outcome = receipt.Outcome
					return errNoVoiceChange
				}
			}
			if len(s.CapabilityReceipts) >= 256 {
				return capError("busy", "外部动作记录已满，请稍后再试")
			}
		}
		outcome.OperationID = newID()
		status, err := apply(s, now, outcome.OperationID)
		if err != nil {
			return err
		}
		outcome.Status = status
		if c.grant != nil {
			s.CapabilityReceipts = append(s.CapabilityReceipts, capabilityReceipt{
				Grant: c.grant.ID, KeyHash: keyHash, InputHash: inputHash, Outcome: outcome, ExpiresAt: now.Add(24 * time.Hour),
			})
		}
		return nil
	})
	if errors.Is(err, errNoVoiceChange) {
		err = nil
	}
	if err != nil {
		return capabilityOutcome{}, err
	}
	return outcome, nil
}

func (p *Platform) externalCapabilityGrant(r *http.Request) *capabilityGrant {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || len(token) != 72 {
		return nil
	}
	digest := hashCapabilityData([]byte(token))
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for _, grant := range p.state.CapabilityGrants {
		if grant.ExpiresAt.After(now) && subtle.ConstantTimeCompare([]byte(grant.Digest), []byte(digest)) == 1 {
			return &grant
		}
	}
	return nil
}

func grantView(grant capabilityGrant) map[string]any {
	ids := make([]string, 0, len(grant.Workflows))
	for id := range grant.Workflows {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return map[string]any{"id": grant.ID, "name": grant.Name, "operations": grant.Operations, "workflow_ids": ids, "expires_at": grant.ExpiresAt}
}

type capabilityGrantInput struct {
	Name        string   `json:"name"`
	Operations  []string `json:"operations"`
	WorkflowIDs []string `json:"workflow_ids,omitempty"`
}

func (a *api) grantCapabilityAccess(input capabilityGrantInput) (capabilityGrant, string, error) {
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" || len(input.Name) > 100 || len(input.Operations) == 0 || len(input.Operations) > len(deviceCapabilities) || len(input.WorkflowIDs) > 32 {
		return capabilityGrant{}, "", capError("invalid_arguments", "请填写调用方名称和授权操作")
	}
	allowed := []string{}
	for _, id := range input.Operations {
		if !slices.ContainsFunc(deviceCapabilities, func(op capability) bool { return op.External && op.ID == id }) || slices.Contains(allowed, id) {
			return capabilityGrant{}, "", capError("invalid_arguments", "包含重复或不可授权的操作")
		}
		allowed = append(allowed, id)
	}
	if slices.Contains(allowed, "automation.run") != (len(input.WorkflowIDs) > 0) {
		return capabilityGrant{}, "", capError("invalid_arguments", "运行自动化时须选择具体流程")
	}
	token := newID() + newID() + newID()
	now := time.Now().UTC()
	grant := capabilityGrant{ID: newID(), Name: input.Name, Operations: allowed, Digest: hashCapabilityData([]byte(token)), ExpiresAt: now.AddDate(0, 0, 30)}
	err := a.platform.change(func(s *State) error {
		s.CapabilityGrants = slices.DeleteFunc(s.CapabilityGrants, func(record capabilityGrant) bool { return !record.ExpiresAt.After(now) })
		if len(s.CapabilityGrants) >= 16 {
			return capError("busy", "请先撤销不用的外部授权")
		}
		grant.Workflows = map[string]string{}
		for _, id := range input.WorkflowIDs {
			i := slices.IndexFunc(s.Rules, func(rule automationRule) bool { return rule.ID == id })
			if i < 0 || !externalWorkflowAllowed(s.Rules[i]) {
				return capError("invalid_arguments", "流程包含首版不可远程执行的动作，或已不存在")
			}
			grant.Workflows[id] = workflowFingerprint(s.Rules[i])
		}
		s.CapabilityGrants = append(s.CapabilityGrants, grant)
		return nil
	})
	return grant, token, err
}

func (a *api) appendCapabilityAudit(c capabilityContext, id string, err error) {
	if c.grant == nil {
		return
	}
	code := "ok"
	if err != nil {
		code = capabilityFailure(err).Code
	}
	log.Printf("capability caller=%s operation=%s result=%s", c.grant.ID, id, code)
}
