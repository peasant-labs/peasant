// Package kickstart rebuilds peasant's first-run onboarding on the declarative
// settings vocabulary (internal/tui/settings) rendered by the kit component set.
// It owns the VIEW composition only: the business logic it drives - the login
// runner, the ingest pipeline, discovery/inventory, and the atomic config save -
// is reused untouched through the same seams the legacy wizard used.
//
// The selection a user makes in the tree round-trips through
// settings.TreeSelection, whose fields ARE the real config.SelectionConfig
// types, so there is no parallel selection model to drift. This file owns the
// one policy the rebuild adds on top of that round-trip: a project-first
// select-all saves the exact projects currently on screen (see
// [DeriveSelection]).
package kickstart

import (
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/settings"
)

// DeriveSelection converts a checked project->branch->session scan forest into
// the config.SelectionConfig kickstart persists. The checked node ids are
// written through the neutral selection field; this wrapper only preserves the
// caller's auto-ingest flag on the returned value.
func DeriveSelection(roots []*kit.TreeNode, autoIngestNewBranches bool) config.SelectionConfig {
	ts := settings.FromTreeNodes(roots)
	return ts.ToSelectionConfig(autoIngestNewBranches)
}
