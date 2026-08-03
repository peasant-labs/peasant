package tui

import "github.com/peasant-labs/schema"

// derefInt safely dereferences a *int, returning 0 if nil.
func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// derefFloat safely dereferences a *float64, returning 0 if nil.
func derefFloat(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

// derefOutcome safely dereferences a *schema.SessionOutcome, returning "" if nil.
func derefOutcome(p *schema.SessionOutcome) schema.SessionOutcome {
	if p == nil {
		return ""
	}
	return *p
}
