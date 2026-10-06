package ingest_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain only checks for leaked goroutines. Tests that need a bounded
// staging arena or a deterministic OpenCode environment inject it through the
// WithArenaSizeBytes and WithOpenCodeEnvironment options; production keeps the
// environment default at the composition root.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
