package api

import (
	"os"
	"strconv"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
)

func TestMain(m *testing.M) {
	// Mounted API tests invoke the real ingest pipeline. Match the CLI and
	// ingest test budgets rather than allocate the production 2 GiB arena.
	os.Setenv(ingest.EnvArenaSizeBytes, strconv.Itoa(64*1024*1024))
	// A re-exec child of a timestamp test asks for a non-UTC local zone so a
	// dropped UTC normalization becomes observable; install it before any test
	// runs. This is a no-op in the normal (parent) process.
	installForcedNonUTCLocalZoneIfRequested()
	os.Exit(m.Run())
}
