// Package kickstart rebuilds peasant's first-run onboarding on the declarative
// settings vocabulary (internal/tui/settings) rendered by the kit component set.
// It owns the VIEW composition only: the business logic it drives - the login
// runner, the ingest pipeline, discovery/inventory, and the atomic config save -
// is reused untouched through the same seams the legacy wizard used.
//
// The selection a user makes in the tree round-trips through
// settings.TreeSelection, whose fields ARE the real config.SelectionConfig
// types, so there is no parallel selection model to drift. This file owns the
// one policy the rebuild adds on top of that round-trip: the ratified root-check
// behaviour (see [DeriveSelection]).
package kickstart

import (
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/settings"
)

// DeriveSelection converts a checked project->branch->session scan
// forest into the config.SelectionConfig the rebuilt kickstart persists.
//
// It applies the one ratified behaviour change the rebuild introduces on top of
// the settings round-trip: selecting every provider root ("select everything")
// persists Mode=all as a STANDING policy - AutoIngestNewBranches forced on - so
// projects discovered AFTER onboarding are auto-included without re-running
// kickstart. The legacy project-first wizard instead enumerated today's projects
// into a mode:selected allowlist, so a later-discovered project would be silently
// excluded; that divergence is banked in the equivalence oracle
// (testdata/equivalence/ratified_divergence.yaml).
//
// Any narrower selection keeps the user's explicit auto-ingest-new-branches
// answer, and the derivation is exactly settings.FromTreeNodes - the same logic
// the general settings flow uses - so the two never drift.
func DeriveSelection(roots []*kit.TreeNode, autoIngestNewBranches bool) config.SelectionConfig {
	ts := settings.FromTreeNodes(roots)
	if ts.Mode == config.SelectionModeAll {
		autoIngestNewBranches = true
	}
	return ts.ToSelectionConfig(autoIngestNewBranches)
}
