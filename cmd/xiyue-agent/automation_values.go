package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"
)

const automationValueBudget = 32 << 10

var automationReference = regexp.MustCompile(`\{\{\s*([A-Za-z_][A-Za-z0-9_]{0,31}(?:\.[A-Za-z0-9_-]+)*)\s*\}\}`)

func automationValue(values map[string]json.RawMessage, path string) (json.RawMessage, error) {
	name, field, _ := strings.Cut(path, ".")
	value, ok := values[name]
	if !ok {
		return nil, fmt.Errorf("未取得变量 %s", name)
	}
	return selectNetworkValue(value, field)
}

func expandAutomationText(text string, values map[string]json.RawMessage) (string, error) {
	var failure error
	out := automationReference.ReplaceAllStringFunc(text, func(match string) string {
		path := automationReference.FindStringSubmatch(match)[1]
		value, err := automationValue(values, path)
		if err != nil {
			failure = err
			return ""
		}
		if len(value) > 0 && value[0] == '"' {
			var text string
			if err := json.Unmarshal(value, &text); err == nil {
				return text
			}
		}
		var compact bytes.Buffer
		_ = json.Compact(&compact, value)
		return compact.String()
	})
	if failure == nil && len(out) > 4000 {
		failure = errors.New("替换变量后的文字过长，请提取更具体的字段")
	}
	return out, failure
}

func automationScalar(value json.RawMessage) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.UseNumber()
	var result any
	if !json.Valid(value) || decoder.Decode(&result) != nil {
		return nil, errors.New("判断值须为有效 JSON")
	}
	switch result.(type) {
	case string, bool, json.Number, nil:
		return result, nil
	default:
		return nil, errors.New("请选择单个文本、数字、布尔值或空值作为判断值")
	}
}

func equalAutomationValues(value, expected json.RawMessage) (bool, error) {
	actual, err := automationScalar(value)
	if err != nil {
		return false, err
	}
	want, err := automationScalar(expected)
	if err != nil {
		return false, err
	}
	if a, ok := actual.(json.Number); ok {
		if b, ok := want.(json.Number); ok {
			parse := func(n json.Number) (*big.Rat, bool) {
				raw := strings.ToLower(string(n))
				_, exponent, scaled := strings.Cut(raw, "e")
				if scaled {
					e, err := strconv.Atoi(exponent)
					if err != nil || e < -1000 || e > 1000 {
						return nil, false
					}
				}
				if len(raw) > 1000 {
					return nil, false
				}
				return new(big.Rat).SetString(raw)
			}
			x, okX := parse(a)
			y, okY := parse(b)
			if !okX || !okY {
				return false, errors.New("用于比较的数字超出支持范围")
			}
			return x.Cmp(y) == 0, nil
		}
	}
	return actual == want, nil
}

func validateAutomationReferences(text string, variables map[string]bool) error {
	for _, match := range automationReference.FindAllStringSubmatch(text, -1) {
		name, _, _ := strings.Cut(match[1], ".")
		if !variables[name] {
			return fmt.Errorf("请先在前面的步骤保存变量 %s", name)
		}
	}
	return nil
}
