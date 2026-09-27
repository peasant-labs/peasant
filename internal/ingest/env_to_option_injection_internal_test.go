package ingest

import (
	"testing"

	"github.com/peasant-labs/peasant/internal/salt"
)

// TestStagingArenaSizeKeepsTheEnvironmentDefault proves WithArenaSizeBytes is an
// override on top of, not a replacement for, the environment default production
// reads at the composition root.
func TestStagingArenaSizeKeepsTheEnvironmentDefault(t *testing.T) {
	// Omitting the option and the environment yields the production slab.
	t.Setenv(EnvArenaSizeBytes, "")
	if got := (&Pipeline{}).stagingArenaSize(); got != DefaultArenaSizeBytes {
		t.Fatalf("no option, no env: arena = %d, want %d", got, int64(DefaultArenaSizeBytes))
	}
	// Omitting the option still honours the environment override.
	t.Setenv(EnvArenaSizeBytes, "1572864")
	if got := (&Pipeline{}).stagingArenaSize(); got != 1572864 {
		t.Fatalf("no option, env override: arena = %d, want 1572864", got)
	}
	// An explicit option wins over the environment.
	pipeline := &Pipeline{}
	WithArenaSizeBytes(42)(pipeline)
	if got := pipeline.stagingArenaSize(); got != 42 {
		t.Fatalf("option over env: arena = %d, want 42", got)
	}
}

// TestOpenCodeAdapterEnvironmentInjectionAndDefault proves
// WithOpenCodeEnvironment is an override on top of the system environment the
// production constructor reads by default.
func TestOpenCodeAdapterEnvironmentInjectionAndDefault(t *testing.T) {
	t.Setenv(openCodeInstallationChannelEnv, "prod")

	injected := NewOpenCodeAdapter(&OSFileSystem{}, noGitResolver{}, salt.Salt{}, WithOpenCodeEnvironment(fixedOpenCodeAdapterEnvironment{}))
	if injected.candidateChannel != "latest" {
		t.Fatalf("injected empty environment: channel = %q, want the default %q", injected.candidateChannel, "latest")
	}

	defaulted := NewOpenCodeAdapter(&OSFileSystem{}, noGitResolver{}, salt.Salt{})
	if defaulted.candidateChannel != "prod" {
		t.Fatalf("no option: channel = %q, want the system environment value %q", defaulted.candidateChannel, "prod")
	}
}
