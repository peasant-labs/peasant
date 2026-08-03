package defaults

import "github.com/peasant-labs/schema"

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

// HarnessDisplayName returns the human-readable name for a harness.
var HarnessDisplayName = schema.HarnessDisplayName
