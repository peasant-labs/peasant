package api

import (
	_ "embed"
	"testing"
)

//go:embed testdata/kickstart_sessions.yaml
var kickstartSessionsYAML []byte

// TestKickstartSessions runs the session lifecycle fixture against the mounted
// server: one session per scope, the revision clocks, stale writes, the live
// topic, how a session ends, and the origin guard.
func TestKickstartSessions(t *testing.T) {
	runKickstartScript(t, kickstartSessionsYAML)
}
