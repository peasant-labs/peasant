package testutil

import (
	"fmt"
	"strings"
)

// RequireFixtureNames asserts every name in required is present in present,
// and that required itself declares no blank or duplicate entry. It is a
// deletion-protection manifest, not a row count: it never bounds how many
// rows a fixture holds, only that the named ones remain. Adding a new row
// never requires touching the required list; removing or renaming a
// required row without updating the list fails the check.
//
// source names what is being validated (e.g. "screenshot fixture",
// "Claude evidence fixture testdata/migrations/v44_claude_evidence.yaml")
// and kind names the axis within it (e.g. "push session", "record") so a
// single error format serves every caller.
func RequireFixtureNames(source, kind string, required []string, present map[string]bool) error {
	if len(required) == 0 {
		return fmt.Errorf("%s declares no required %s names; list every %s name the fixture must retain", source, kind, kind)
	}
	seen := make(map[string]bool, len(required))
	for _, name := range required {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("%s required %s names has a blank entry; name every required %s", source, kind, kind)
		}
		if seen[name] {
			return fmt.Errorf("%s required %s names repeats %q; list each required %s once", source, kind, name, kind)
		}
		seen[name] = true
		if !present[name] {
			return fmt.Errorf("%s is missing required %s %q; restore the row or remove it from the required %s names list", source, kind, name, kind)
		}
	}
	return nil
}
