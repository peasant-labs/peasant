package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
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
//   - GIT_CONFIG_NOSYSTEM=1: the hooks tests ask git which hook path it would
//     really run inside a disposable repository. A core.hooksPath in
//     /etc/gitconfig would make that resolve to a shared path outside the test's
//     repository, so the tests would exercise a different code path on such a
//     machine than in CI - and one that Peasant deliberately refuses to manage.
//     The githooks package closes the same door for the same reason.
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
	// values while HOME still points at the real one.
	if out, envErr := exec.Command("go", "env", "GOMODCACHE", "GOCACHE").Output(); envErr == nil {
		if fields := strings.Fields(string(out)); len(fields) == 2 {
			os.Setenv("GOMODCACHE", fields[0])
			os.Setenv("GOCACHE", fields[1])
		}
	}
	os.Setenv("HOME", home)
	os.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	os.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	os.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	os.Setenv("OPENCODE_DB", "")
	os.Setenv("OPENCODE_DISABLE_CHANNEL_DB", "")
	os.Setenv("OPENCODE_CHANNEL", "latest")
	os.Setenv(store.EnvPoolSize, "1")
	// Small staging arena: harvest/ingest commands otherwise allocate the 2 GiB
	// production slab per run, which is the dominant race-test memory cost.
	os.Setenv(ingest.EnvArenaSizeBytes, strconv.Itoa(64*1024*1024)) // 64 MiB

	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}
