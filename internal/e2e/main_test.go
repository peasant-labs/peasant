package e2e

import (
	"os"
	"os/exec"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
)

func TestMain(m *testing.M) {
	// Cover direct go test callers as well as Make. Child CLI commands inherit
	// this environment through os.Environ, including isolated XDG sandboxes.
	if os.Getenv(ingest.EnvArenaSizeBytes) == "" {
		os.Setenv(ingest.EnvArenaSizeBytes, "67108864")
	}
	if os.Getenv("GOMEMLIMIT") == "" {
		os.Setenv("GOMEMLIMIT", "2GiB")
		// Runtime startup precedes TestMain; setting the env alone only affects children.
		debug.SetMemoryLimit(2 * 1024 * 1024 * 1024)
	}
	os.Exit(m.Run())
}

func TestMemoryBudgetInheritedByChild(t *testing.T) {
	// Use the same inheritance as the real CLI's isolated XDG environments.
	command := exec.Command("env")
	command.Env = append(os.Environ(), xdgEnvAssignments(t.TempDir(), t.TempDir(), t.TempDir())...)
	out, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), ingest.EnvArenaSizeBytes+"="+os.Getenv(ingest.EnvArenaSizeBytes)+"\n") ||
		!strings.Contains(string(out), "GOMEMLIMIT="+os.Getenv("GOMEMLIMIT")+"\n") {
		t.Fatal("isolated child environment lost the arena or Go memory budget")
	}
	if debug.SetMemoryLimit(-1) <= 0 || os.Getenv(ingest.EnvArenaSizeBytes) == "" {
		t.Fatal("test runner memory defaults were not installed")
	}
}
