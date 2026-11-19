package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testkit/gitconfig"
)

// TestMain configures the cmd/peasant test binary process-wide. It uses
// os.Setenv (not t.Setenv) so it does NOT mark any test as non-parallelizable —
// these settings are constant for the whole binary.
//
//   - HOME + XDG_* are pointed at a throwaway temp directory so no test reads the
//     local ~/.claude (source discovery), ~/.config/peasant (config +
//     credentials), ~/.local/share/peasant (DB), or ~/.local/state/peasant. This
//     is the safety net: hermeticity for every test, and the parallel tests still
//     inject their own per-test dir via --data-dir/--config-dir on top of it.
//   - store.EnvPoolSize=1: the CLI tests drive commands that open the store via
//     the production path, which otherwise opens store.DefaultPoolSize (10)
//     connections per Open — each re-parsing the schema + running PRAGMAs —
//     across hundreds of Opens in this package. One connection is all a
//     single-threaded test needs.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "peasant-cmdtest-home-*")
	if err != nil {
		panic("cmd/peasant TestMain: create temp HOME: " + err.Error())
	}
	// Preserve the developer's Go caches across the HOME redirect below: `go
	// build` resolves GOMODCACHE/GOCACHE from HOME, so without this the matrix
	// build re-downloads the module graph into the throwaway home. Capture the
	// values while HOME still points at the real one. One `go env` call per
	// variable: a single space-separated call cannot round-trip a path that
	// contains a space.
	for _, key := range []string{"GOMODCACHE", "GOCACHE"} {
		if out, envErr := exec.Command("go", "env", key).Output(); envErr == nil {
			if value := strings.TrimSpace(string(out)); value != "" {
				os.Setenv(key, value)
			}
		}
	}
	os.Setenv("HOME", home)
	os.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	os.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	os.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	os.Setenv("OPENCODE_DB", "")
	os.Setenv("OPENCODE_DISABLE_CHANNEL_DB", "")
	os.Setenv("OPENCODE_CHANNEL", "latest")
	os.Setenv(store.EnvPoolSize, "1")
	// Small staging arena: harvest/ingest commands otherwise allocate the 2 GiB
	// production slab per run, which is the dominant race-test memory cost.
	os.Setenv(ingest.EnvArenaSizeBytes, strconv.Itoa(64*1024*1024)) // 64 MiB

	os.Exit(gitconfig.RunWithIsolatedGitConfig(gitconfig.WithCleanup(m, func() {
		if os.Getenv("PEASANT_TEST_KEEP_SCRATCH") != defaults.Key1.String() {
			removePeasantMatrixBuildDir()
			_ = os.RemoveAll(home)
		}
	})))
}
