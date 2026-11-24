package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"slices"
)

var bundledFlowHashes = map[string]string{
	"short-timer":       "b89a6e382665aff12981c11d22cacfb8ada04ec0a74b107406c9c55d77aca3ac",
	"stretch-guide":     "1faad4af4a9a315305acb2b06fac980e833ebdff20ccb4c8e4287c5ac43d2941",
	"check-reminder":    "f2f164d4f22ea78ecaef7c7f84b38e798f24f2b5d90c9cfe3b423d21905a75de",
	"cooking-countdown": "5129ee62edd9ab2ea68bbec8a0e7726fbe1e2e486919b951eac4d7eb689b5bc7",
	"tea-guide":         "57d963914c9faee9eda6d9c0cab0ded4bca2997085897bfb87d0480fb4bf10c9",
}

func unusedBundledFlow(s *State, ability Ability) bool {
	id := ability.Manifest.ID
	if ability.Enabled || len(ability.Config) > 0 || bundledFlowHashes[id] == "" {
		return false
	}
	data, _ := json.Marshal(ability.Manifest)
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:]) == bundledFlowHashes[id] &&
		!slices.ContainsFunc(s.Tasks, func(t Task) bool { return t.AbilityID == id }) &&
		!slices.ContainsFunc(s.Schedules, func(sc Schedule) bool { return sc.AbilityID == id }) &&
		!slices.ContainsFunc(s.Rules, func(rule automationRule) bool {
			return slices.ContainsFunc(rule.Actions, func(step automationStep) bool { return step.Action == "flow" && step.Value == id })
		})
}

func convertLegacyFlow(m Manifest, values map[string]string) ([]automationStep, error) {
	var steps []automationStep
	for _, source := range m.Steps {
		text := placeholder.ReplaceAllStringFunc(source.Text, func(match string) string {
			return values[match[2:len(match)-2]]
		})
		switch source.Kind {
		case "say":
			steps = append(steps, automationStep{Action: "speak", Value: text})
		case "tone":
			steps = append(steps, automationStep{Action: "tone", Value: text})
		case "ask", "confirm":
			steps = append(steps, automationStep{Action: "confirm", Value: text, OnTimeout: "stop"})
		case "timer":
			steps = append(steps, automationStep{Action: "speak", Value: text}, automationStep{Action: "wait", Seconds: source.Seconds})
		default:
			return nil, errors.New("旧版流程包含暂无法转换的步骤")
		}
	}
	return steps, nil
}

func (p *Platform) migrateLegacyFlows() error {
	if p.state.FlowMigration {
		return nil
	}
	return p.change(func(s *State) error {
		s.FlowMigration = true
		for _, id := range slices.Sorted(maps.Keys(s.Abilities)) {
			switch id {
			case "todo", "reminder", "quick-timer", "guided-flow":
				continue
			}
			ability := s.Abilities[id]
			if ability.Manifest.Menu || unusedBundledFlow(s, ability) {
				continue
			}
			values, err := configValues(ability, nil)
			if err != nil || len(s.Rules) >= maxAutomationRules {
				s.LegacyFlows = append(s.LegacyFlows, id)
				continue
			}
			steps, err := convertLegacyFlow(ability.Manifest, values)
			if err != nil {
				s.LegacyFlows = append(s.LegacyFlows, id)
				continue
			}
			rule := automationRule{
				ID: newID(), Name: ability.Manifest.Name, Trigger: "manual",
				Schedule: Schedule{Enabled: ability.Enabled, Timezone: s.Settings.Timezone}, Actions: steps,
			}
			if validateAutomation(s, rule) != nil {
				s.LegacyFlows = append(s.LegacyFlows, id)
				continue
			}
			s.Rules = append(s.Rules, rule)
		}
		return nil
	})
}
