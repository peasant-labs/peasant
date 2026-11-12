package api

import (
	_ "embed"
	"testing"
)

//go:embed testdata/kickstart_tree.yaml
var kickstartTreeYAML []byte

// TestKickstartTree runs the discovery, forest, preview, and rescan fixture
// against the mounted server.
func TestKickstartTree(t *testing.T) {
	runKickstartScript(t, kickstartTreeYAML)
}
