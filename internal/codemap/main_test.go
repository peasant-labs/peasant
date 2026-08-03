package codemap_test

import (
	"os"
	"testing"

	"github.com/peasant-labs/peasant/internal/store"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	// Small pool (2, not 1: some store paths take a second connection while
	// holding the first) — mirrors internal/store's TestMain rationale.
	os.Setenv(store.EnvPoolSize, "2")
	goleak.VerifyTestMain(m)
}
