package api

import (
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/store"
)

// testXDGHomes isolates the XDG config, data, and state roots for one test.
// Tests inject them into a syncHandler or a ServerConfig instead of calling the
// process environment, so a test that only needs its own roots can run in
// parallel with the rest of the package.
type testXDGHomes struct{ Config, Data, State string }

// newTestXDGHomes creates three sibling roots under one t.TempDir.
func newTestXDGHomes(t *testing.T) testXDGHomes {
	t.Helper()
	home := t.TempDir()
	return testXDGHomes{
		Config: filepath.Join(home, "config"),
		Data:   filepath.Join(home, "data"),
		State:  filepath.Join(home, "state"),
	}
}

// dbPath is the analytics database path these roots resolve to.
func (hs testXDGHomes) dbPath() string {
	return string(defaults.ResolveDBFilePathWith(hs.Data))
}

// handler builds a syncHandler wired to these roots.
func (hs testXDGHomes) handler(db *store.Store, cfg *config.Config) *syncHandler {
	return &syncHandler{
		store:      db,
		config:     cfg,
		configHome: hs.Config,
		dataHome:   hs.Data,
		stateHome:  hs.State,
	}
}

// config wires a ServerConfig to these roots, preserving the caller's fields.
func (hs testXDGHomes) config(cfg ServerConfig) ServerConfig {
	cfg.ConfigHome, cfg.DataHome, cfg.StateHome = hs.Config, hs.Data, hs.State
	return cfg
}

// TestSyncHandlerXDGRootsKeepTheEnvironmentDefault pins that omitting the
// injected roots leaves the process environment as the default: the handler
// resolves XDG_* from the environment, and an injected override wins only where
// it is set. This is the no-option half of the env-to-option injection, so it
// deliberately reads and mutates process environment and stays serial.
func TestSyncHandlerXDGRootsKeepTheEnvironmentDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv(defaults.EnvXDGConfigHome.String(), filepath.Join(home, "config"))
	t.Setenv(defaults.EnvXDGDataHome.String(), filepath.Join(home, "data"))
	t.Setenv(defaults.EnvXDGStateHome.String(), filepath.Join(home, "state"))

	env := &syncHandler{}
	if got, want := env.configDir().String(), filepath.Join(home, "config", string(defaults.AppName)); got != want {
		t.Fatalf("environment configDir = %q, want %q", got, want)
	}
	if got, want := env.dbPath().String(), filepath.Join(home, "data", string(defaults.AppName), "peasant.db"); got != want {
		t.Fatalf("environment dbPath = %q, want %q", got, want)
	}
	if got, want := env.stateDir().String(), filepath.Join(home, "state", string(defaults.AppName)); got != want {
		t.Fatalf("environment stateDir = %q, want %q", got, want)
	}

	injected := &syncHandler{
		configHome: filepath.Join(home, "injected-config"),
		dataHome:   filepath.Join(home, "injected-data"),
		stateHome:  filepath.Join(home, "injected-state"),
	}
	if got, want := injected.configDir().String(), filepath.Join(home, "injected-config", string(defaults.AppName)); got != want {
		t.Fatalf("injected configDir = %q, want %q", got, want)
	}
	if got, want := injected.dbPath().String(), filepath.Join(home, "injected-data", string(defaults.AppName), "peasant.db"); got != want {
		t.Fatalf("injected dbPath = %q, want %q", got, want)
	}
	if got, want := injected.stateDir().String(), filepath.Join(home, "injected-state", string(defaults.AppName)); got != want {
		t.Fatalf("injected stateDir = %q, want %q", got, want)
	}
}
