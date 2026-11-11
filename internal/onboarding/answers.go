package onboarding

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

// Answers is the ordered-apply input. Keys absent from the map are untouched.
type Answers map[FieldKey]schema.LocalSettingValue

// SameSettingValue compares two setting values as JSON, ignoring whitespace.
// Values that are not valid JSON are compared byte for byte.
func SameSettingValue(a, b schema.LocalSettingValue) bool {
	var left, right bytes.Buffer
	if json.Compact(&left, a) != nil || json.Compact(&right, b) != nil {
		return bytes.Equal(a, b)
	}
	return bytes.Equal(left.Bytes(), right.Bytes())
}

// StringValue encodes a wire token as a JSON string.
func StringValue(token string) schema.LocalSettingValue {
	data, _ := json.Marshal(token)
	return schema.LocalSettingValue(data)
}

// StringToken decodes a JSON string token.
func StringToken(value schema.LocalSettingValue) (string, error) {
	var token string
	if err := json.Unmarshal(value, &token); err != nil {
		return "", fmt.Errorf("value is not a JSON string: %w", err)
	}
	return token, nil
}

// BoolValue encodes a boolean answer.
func BoolValue(on bool) schema.LocalSettingValue {
	data, _ := json.Marshal(on)
	return schema.LocalSettingValue(data)
}

// BoolToken decodes a JSON boolean.
func BoolToken(value schema.LocalSettingValue) (bool, error) {
	var on bool
	if err := json.Unmarshal(value, &on); err != nil {
		return false, fmt.Errorf("value is not a JSON boolean: %w", err)
	}
	return on, nil
}

// IntValue encodes an integer answer.
func IntValue(number int) schema.LocalSettingValue {
	data, _ := json.Marshal(number)
	return schema.LocalSettingValue(data)
}

// IntToken decodes a JSON integer.
func IntToken(value schema.LocalSettingValue) (int, error) {
	var number int
	if err := json.Unmarshal(value, &number); err != nil {
		return 0, fmt.Errorf("value is not a JSON integer: %w", err)
	}
	return number, nil
}

// SelectionMappingValue encodes a selection mapping answer. It is one object,
// not a list of node ids.
func SelectionMappingValue(mode config.SelectionMode, harnesses map[string]config.SelectionHarnessConfig) (schema.LocalSettingValue, error) {
	data, err := json.Marshal(SelectionAnswer{Mode: string(mode), Harnesses: harnesses})
	if err != nil {
		return nil, fmt.Errorf("encode selection mapping: %w", err)
	}
	return schema.LocalSettingValue(data), nil
}

func decodeSelectionToken(token string) (string, map[string]config.SelectionHarnessConfig, []string, bool) {
	var answer SelectionAnswer
	if err := json.Unmarshal([]byte(token), &answer); err != nil {
		return "", nil, nil, false
	}
	return answer.Mode, answer.Harnesses, answer.IDs, true
}

// ApplyAnswers writes answered fields in declaration order and mutates cfg.
// It is the only caller of Field.Write. Later answers win.
func (m Model) ApplyAnswers(cfg *config.Config, answers Answers, w WriteContext) error {
	if cfg == nil {
		return actionable(
			"apply setup answers",
			"the configuration pointer is nil",
			"onboarding.Model.ApplyAnswers",
			"before any field was written",
			"no answer was applied",
			"pass the configuration the answers should mutate")
	}
	w.Config = cfg
	if w.Answers == nil {
		w.Answers = answers
	}
	for _, field := range m.Fields() {
		value, ok := answers[field.Key()]
		if !ok {
			continue
		}
		if err := field.Write(cfg, w, value); err != nil {
			return err
		}
	}
	return nil
}

// AppliedCopy runs ApplyAnswers on a copy of cfg and returns the copy: the
// configuration the answers would produce. The caller's configuration is not
// mutated. Reading the copy shows every field the answers moved, the answered
// ones and the ones an answer moves as a side effect.
func (m Model) AppliedCopy(cfg *config.Config, answers Answers, w WriteContext) (*config.Config, error) {
	if cfg == nil {
		return nil, actionable(
			"apply setup answers to a copy",
			"the configuration pointer is nil",
			"onboarding.Model.AppliedCopy",
			"before the copy was written",
			"no copy was returned",
			"pass the configuration the answers would change")
	}
	copied, err := cloneConfig(cfg)
	if err != nil {
		return nil, err
	}
	w.Config = &copied
	if err := m.ApplyAnswers(&copied, answers, w); err != nil {
		return nil, err
	}
	return &copied, nil
}

// DropHidden removes answers whose fields are hidden after the remaining
// answers are applied. It repeats until a pass drops nothing, because dropping
// one answer can hide another.
func (m Model) DropHidden(cfg *config.Config, answers Answers, w WriteContext) (Answers, error) {
	if cfg == nil {
		return nil, actionable(
			"drop hidden setup answers",
			"the configuration pointer is nil",
			"onboarding.Model.DropHidden",
			"before hidden answers were removed",
			"the answer set was not changed",
			"pass the configuration the answers would be applied to")
	}
	current := copyAnswers(answers)
	limit := len(m.Fields()) + 1
	for pass := 0; pass < limit; pass++ {
		copied, err := cloneConfig(cfg)
		if err != nil {
			return nil, err
		}
		trial := w
		trial.Config = &copied
		trial.Answers = current
		if err := m.ApplyAnswers(&copied, current, trial); err != nil {
			return nil, err
		}
		trial.Config = &copied
		next := Answers{}
		dropped := false
		for _, field := range m.Fields() {
			value, ok := current[field.Key()]
			if !ok {
				continue
			}
			if m.fieldShown(field, trial) {
				next[field.Key()] = append(schema.LocalSettingValue(nil), value...)
				continue
			}
			dropped = true
		}
		current = next
		if !dropped {
			return current, nil
		}
	}
	return nil, actionable(
		"drop hidden setup answers",
		"hiding one answer kept revealing another past the field count",
		"onboarding.Model.DropHidden",
		"while converging hidden answers",
		"the answer set was not returned",
		"remove the visibility cycle before applying answers")
}

// Validate runs the visible fields' validators in declaration order and returns
// the first actionable error.
func (m Model) Validate(w WriteContext) error {
	if w.Config == nil {
		return actionable(
			"validate setup answers",
			"the configuration pointer is nil",
			"onboarding.Model.Validate",
			"before visible fields were checked",
			"no field was validated",
			"pass the configuration the answers produced")
	}
	for _, field := range m.Fields() {
		if !m.fieldShown(field, w) {
			continue
		}
		if err := field.Validate(w); err != nil {
			return err
		}
	}
	return nil
}

func (m Model) fieldShown(field Field, w WriteContext) bool {
	_, hidden := m.HiddenBecause(field, w)
	return !hidden
}

func copyAnswers(answers Answers) Answers {
	out := Answers{}
	for key, value := range answers {
		out[key] = append(schema.LocalSettingValue(nil), value...)
	}
	return out
}

func cloneConfig(src *config.Config) (config.Config, error) {
	if src == nil {
		return config.Config{}, actionable(
			"copy a configuration",
			"the configuration pointer is nil",
			"onboarding.cloneConfig",
			"before a trial apply",
			"no copy was made",
			"pass a configuration")
	}
	data, err := yaml.Marshal(src)
	if err != nil {
		return config.Config{}, fmt.Errorf("marshal configuration for a trial apply: %w", err)
	}
	var dst config.Config
	if err := yaml.Unmarshal(data, &dst); err != nil {
		return config.Config{}, fmt.Errorf("unmarshal configuration for a trial apply: %w", err)
	}
	dst.ClaudeRetentionDays = src.ClaudeRetentionDays
	return dst, nil
}
