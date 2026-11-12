package api

import (
	_ "embed"
	"testing"
)

//go:embed testdata/kickstart_patch.yaml
var kickstartPatchYAML []byte

// TestKickstartPatch runs the PATCH fixture against the mounted server: what an
// answer request may carry and what a refused one leaves behind.
func TestKickstartPatch(t *testing.T) {
	runKickstartScript(t, kickstartPatchYAML)
}
