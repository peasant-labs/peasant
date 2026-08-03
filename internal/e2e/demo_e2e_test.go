//go:build e2e

package e2e

import "testing"

// TestSkipGateDemo is the unasserted "watch it happen live" variant of the
// skip-gate harness, run by `make demo`. It provisions the SAME ephemeral stack
// (podman Postgres + MinIO + real village subprocess + minted demo creds) and
// drives the SAME flow (ingest claude+codex fixtures → annotate → push#1/#2 →
// retraction → unknown-harness schema rejection), but with assertions downgraded to logs — so
// a reviewer can SEE 4 transcripts (2 claude + 2 codex) + K annotations on
// push#1, 0+0 on push#2, and exactly one retraction, without the run failing on a
// transient. Use:
//
//	go test -race -tags=e2e -run TestSkipGateDemo -v ./internal/e2e/...
//
// It uses the throwaway SANDBOX (under the resolved XDG_STATE_HOME) — never the
// real data dir.
func TestSkipGateDemo(t *testing.T) {
	runSkipGateHarness(t, harnessOptions{assert: false})
}
