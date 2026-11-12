package defaults

import (
	"cmp"
	"slices"
	"strings"

	"github.com/peasant-labs/schema"
)

// Harness identifies the coding tool or AI-assisted development environment.
// Re-exported from schema (which re-exports from bestiary).
type Harness = schema.Harness

const (
	HarnessClaudeCode  = schema.HarnessClaudeCode
	HarnessGeminiCLI   = schema.HarnessGeminiCLI
	HarnessCodex       = schema.HarnessCodex
	HarnessOpenCode    = schema.HarnessOpenCode
	HarnessCursor      = schema.HarnessCursor
	HarnessAntigravity = schema.HarnessAntigravity
	HarnessStrike      = schema.HarnessStrike
	HarnessPi          = schema.HarnessPi
)

// LegacyHarnessClaude and LegacyHarnessGemini are the PRE-RENAME harness
// identifiers ("claude", "gemini") that the bestiary migration replaced with
// "claude-code" and "gemini-cli" respectively.
//
// PURPOSE: these are recognized ONLY to deprecate/migrate inputs written by
// pre-rename installs — resolveHarnessFlag (CLI) emits a rename hint, and config
// migration remaps them to the canonical bestiary IDs. They are intentionally NOT
// part of AllHarnesses and are not IsKnown().
//
// TEMPORARY: remove these (and their two switch call sites) when the project goes
// PUBLIC. At that point no pre-rename installs remain to migrate, so accepting the
// old names only invites confusion. Typed as Harness so the case sites stay typed
// and ast-grep's no-bare-provider-literals rule has a single sanctioned home here.
const (
	LegacyHarnessClaude Harness = "claude"
	LegacyHarnessGemini Harness = "gemini"
)

// AllHarnesses is the canonical list of harnesses peasant supports for ingestion.
var AllHarnesses = schema.AllHarnesses

// Harnesses returns every harness identifier known to bestiary (the full known
// set, a superset of AllHarnesses). Re-exported from schema.
var Harnesses = schema.Harnesses

// InKnownOrder returns the distinct values of harnesses in the contract's known
// order. A value the contract does not recognise sorts after the known values,
// by name. No value is dropped.
func InKnownOrder(harnesses []Harness) []Harness {
	known := schema.Harnesses()
	rank := make(map[Harness]int, len(known))
	for index, harness := range known {
		if _, ok := rank[harness]; !ok {
			rank[harness] = index
		}
	}
	seen := make(map[Harness]struct{}, len(harnesses))
	ordered := make([]Harness, 0, len(harnesses))
	for _, harness := range harnesses {
		if _, ok := seen[harness]; ok {
			continue
		}
		seen[harness] = struct{}{}
		ordered = append(ordered, harness)
	}
	slices.SortFunc(ordered, func(a, b Harness) int {
		rankA, knownA := rank[a]
		rankB, knownB := rank[b]
		switch {
		case knownA && knownB:
			return cmp.Compare(rankA, rankB)
		case knownA:
			return -1
		case knownB:
			return 1
		default:
			return strings.Compare(string(a), string(b))
		}
	})
	return ordered
}

// HarnessDisplayName returns the human-readable name for a harness.
var HarnessDisplayName = schema.HarnessDisplayName
