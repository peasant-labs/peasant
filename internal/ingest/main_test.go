package ingest_test

import (
	"os"
	"strconv"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	// Use a small staging arena so pipeline tests don't allocate the 2 GiB
	// production slab per run — the dominant race-test memory cost.
	os.Setenv(ingest.EnvArenaSizeBytes, strconv.Itoa(64*1024*1024)) // 64 MiB
	// Candidate discovery must never inherit a developer's explicit OpenCode
	// database override while tests exercise the production adapter wiring.
	os.Setenv("OPENCODE_DB", "")
	os.Setenv("OPENCODE_DISABLE_CHANNEL_DB", "")
	os.Setenv("OPENCODE_CHANNEL", "latest")
	goleak.VerifyTestMain(m)
}
