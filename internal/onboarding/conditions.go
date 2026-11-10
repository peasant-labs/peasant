package onboarding

import (
	"fmt"
	"slices"
)

// ConditionKind is the closed set of visibility facts. A presentation evaluates
// them through Condition.Evaluate or Visible. It does not restate the rule.
type ConditionKind string

const (
	CondSelectionNarrowed   ConditionKind = "selection-narrowed"
	CondVillageConnected    ConditionKind = "village-connected"
	CondVillageDisconnected ConditionKind = "village-disconnected"
	CondLoginAvailable      ConditionKind = "login-available"
	CondClaudeSessions      ConditionKind = "claude-sessions-present"
	CondAnswer              ConditionKind = "answer-equals"
)

// AllConditionKinds is the closed set.
var AllConditionKinds = []ConditionKind{
	CondSelectionNarrowed,
	CondVillageConnected,
	CondVillageDisconnected,
	CondLoginAvailable,
	CondClaudeSessions,
	CondAnswer,
}

// NewConditionKind refuses an unknown token.
func NewConditionKind(s string) (ConditionKind, error) {
	kind := ConditionKind(s)
	if !kind.IsValid() {
		return "", fmt.Errorf(
			"read visibility condition %q.\n"+
				"what: %q is not a visibility condition.\n"+
				"why: the closed set is %s.\n"+
				"where: onboarding.NewConditionKind.\n"+
				"when: a caller named a condition.\n"+
				"means: the condition was not accepted.\n"+
				"fix: use one of the closed kind tokens.",
			s, s, joinKeys(AllConditionKinds))
	}
	return kind, nil
}

// IsValid reports whether k is a closed kind.
func (k ConditionKind) IsValid() bool { return slices.Contains(AllConditionKinds, k) }

// String returns the stable token, or "unknown".
func (k ConditionKind) String() string {
	if k.IsValid() {
		return string(k)
	}
	return "unknown"
}

// Condition is one visibility fact. Field and Value are read only for
// answer-equals. Negate inverts the result.
type Condition struct {
	Kind   ConditionKind
	Field  FieldKey
	Value  string
	Negate bool
}

// Evaluate reports whether c holds for w.
func (c Condition) Evaluate(w WriteContext) bool {
	holds := c.holds(w)
	if c.Negate {
		return !holds
	}
	return holds
}

func (c Condition) holds(w WriteContext) bool {
	switch c.Kind {
	case CondSelectionNarrowed:
		return selectionMode(w) == "selected"
	case CondVillageConnected:
		return w.VillageConnected
	case CondVillageDisconnected:
		return !w.VillageConnected
	case CondLoginAvailable:
		return w.LoginAvailable
	case CondClaudeSessions:
		return w.ClaudeSessions
	case CondAnswer:
		return answerToken(w, c.Field) == c.Value
	default:
		return false
	}
}

// String returns a stable description of the condition.
func (c Condition) String() string {
	text := c.Kind.String()
	if c.Kind == CondAnswer {
		text += " " + c.Field.String() + "=" + c.Value
	}
	if c.Negate {
		return "not " + text
	}
	return text
}

// ConditionsHold reports whether every condition holds. An empty list holds.
func ConditionsHold(conditions []Condition, w WriteContext) bool {
	for _, condition := range conditions {
		if !condition.Evaluate(w) {
			return false
		}
	}
	return true
}

// Visible is the one field-visibility evaluator. A hidden stop hides its fields.
// Field conditions are then applied. Presentations call this instead of restating a rule.
func Visible(field Field, w WriteContext) bool {
	if field == nil {
		return false
	}
	return ConditionsHold(field.Conditions(), w)
}

func selectionMode(w WriteContext) string {
	if token := answerToken(w, FieldSelection); token != "" {
		mode, _, _, ok := decodeSelectionToken(token)
		if ok && mode != "" {
			return mode
		}
	}
	if w.Config == nil {
		return ""
	}
	return string(w.Config.Selection.Mode)
}

func answerToken(w WriteContext, key FieldKey) string {
	if w.Answers == nil {
		return ""
	}
	value, ok := w.Answers[key]
	if !ok || value.IsUnset() {
		return ""
	}
	token, err := StringToken(value)
	if err == nil {
		return token
	}
	return string(value)
}
