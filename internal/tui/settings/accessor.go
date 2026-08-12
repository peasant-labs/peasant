// Package settings is the peasant TUI's declarative settings-authoring
// vocabulary, built on the kit component set (internal/tui/kit), the single
// keymap (internal/tui/keymap), and the tokens-only theme
// (internal/tui/theme). A caller (today: the rebuilt kickstart) declares a
// [Registry] of [Section]s of [Field]s, opens a [Draft] over the loaded
// configuration, and mounts a [Flow] that steps the user through the sections
// and commits ONCE, atomically, at a final receipt step.
//
// # Why a Draft, not direct config mutation
//
// Every field edits a working copy the [Draft] owns, never the loaded config
// the caller still holds. Dirty tracking falls out of a baseline-vs-working
// compare, discard is a reset to baseline, and commit routes through the one
// existing atomic save path (config.SaveAtomic) with the same drift-detection
// discipline the kickstart wizard's save already uses - so the settings flow
// cannot invent a second, divergent way to persist configuration.
//
// # Why TreeSelection reuses the real config structs
//
// The selection tree round-trips through [TreeSelection], whose fields ARE
// config.SelectionMode and config.SelectionHarnessConfig - the exact types the
// ingest pipeline, push, discovery lists, and prune already consume. There is
// no parallel selection model to drift out of sync, and pre-population reuses
// config.CompileSelectionMatcher rather than reimplementing which sessions a
// saved selection already covers.
package settings

import "github.com/peasant-labs/peasant/internal/config"

// Accessor is a typed lens into the working configuration a [Draft] owns: Get
// reads the current value of one setting, Set writes it. A [Field] holds an
// Accessor of its value type and never touches the config.Config layout
// directly, so the same field type drives any setting of that shape. Both
// funcs receive the *config.Config the Draft is currently editing (its working
// copy for live edits, its baseline when an edit is being dropped), which is
// why the target is the config pointer rather than the Draft itself.
type Accessor[T any] struct {
	// Get reads the setting's current value from cfg.
	Get func(cfg *config.Config) T
	// Set writes value into cfg, overwriting only the field this accessor owns.
	Set func(cfg *config.Config, value T)
}
