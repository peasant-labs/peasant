package testgate

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"
)

// BudgetEnforcement is the closed set of budget miss dispositions.
type BudgetEnforcement string

const (
	// EnforcementBlocking fails the gate on a budget miss (the default).
	EnforcementBlocking BudgetEnforcement = "blocking"
	// EnforcementWarn prints the miss as an unmissable WARN line and leaves
	// the exit code green. Test failures, screen findings, and invocation
	// errors still fail the gate.
	EnforcementWarn BudgetEnforcement = "warn"
)

// Budget is the committed budget fixture (budget.yaml). It is not committed by
// this slice; the release gate pins the reference value. The gate reads it when
// present, otherwise TEST_BUDGET from the environment, otherwise prints raw
// walls only.
//
// Enforcement is a deliberate contract change to this frozen shape at
// version 1, not a version bump: an absent field defaults to blocking, so
// every existing fixture keeps its meaning.
type Budget struct {
	Version     int               `yaml:"version"`
	Seconds     int               `yaml:"seconds"`
	Basis       string            `yaml:"basis"`
	Enforcement BudgetEnforcement `yaml:"enforcement"`
}

// LoadBudget reads budget.yaml. found is false when the file is absent; any
// other read or decode error is returned.
func LoadBudget(path string) (Budget, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Budget{}, false, nil
		}
		return Budget{}, false, fmt.Errorf("read budget %s: %w", path, err)
	}
	var b Budget
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&b); err != nil {
		return Budget{}, false, fmt.Errorf("decode budget %s: %w", path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Budget{}, false, fmt.Errorf("budget %s must contain exactly one document", path)
	}
	if b.Version != 1 {
		return Budget{}, false, fmt.Errorf("budget %s version = %d, want 1", path, b.Version)
	}
	if b.Seconds <= 0 {
		return Budget{}, false, fmt.Errorf("budget %s seconds = %d, want a positive value", path, b.Seconds)
	}
	if b.Enforcement == "" {
		b.Enforcement = EnforcementBlocking
	}
	switch b.Enforcement {
	case EnforcementBlocking, EnforcementWarn:
	default:
		return Budget{}, false, fmt.Errorf("budget %s enforcement = %q, want blocking or warn: an unknown enforcement fails closed so a typo cannot silently demote the gate; fix the enforcement field in %s", path, b.Enforcement, path)
	}
	return b, true, nil
}
