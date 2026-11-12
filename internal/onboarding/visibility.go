package onboarding

// HiddenBecause reports why a field is not shown under w. The conditions of the
// stop that holds the field are read first, then the field's own; the first one
// that does not hold is the reason. The second result is false when the field is
// shown. A field no stop holds is judged by its own conditions alone.
//
// It is the one place that decides whether a field is shown, so the answers a
// presentation may send, the answers DropHidden removes, and the rows a page
// draws cannot disagree.
func (m Model) HiddenBecause(field Field, w WriteContext) (Condition, bool) {
	conditions := field.Conditions()
	for _, stop := range m.Stops {
		for _, candidate := range stop.Fields {
			if candidate != nil && candidate.Key() == field.Key() {
				conditions = append(append([]Condition(nil), stop.Visible...), conditions...)
				return firstConditionNotHolding(conditions, w)
			}
		}
	}
	return firstConditionNotHolding(conditions, w)
}

func firstConditionNotHolding(conditions []Condition, w WriteContext) (Condition, bool) {
	for _, condition := range conditions {
		if !condition.Evaluate(w) {
			return condition, true
		}
	}
	return Condition{}, false
}
