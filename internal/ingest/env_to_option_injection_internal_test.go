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

// TestWithIndexersCopiesIntoTheWeakSeam proves the generic injection bridge
// accepts the composed registry output and an explicit compatibility-typed map
// alike, copies their values into the pipeline's own compatibility-typed seam,
// and never aliases the caller's map. A later mutation of the caller's map must
// not change what a built pipeline dispatches to, every injected value must keep
// its dynamic type, and a nil map must leave the seam unset.
func TestWithIndexersCopiesIntoTheWeakSeam(t *testing.T) {
	t.Parallel()

	registry := NewIndexerRegistry(nil, IndexerRegistryOptions{})
	fromRegistry := &Pipeline{}
	WithIndexers(registry)(fromRegistry)
	if len(fromRegistry.indexers) != len(registry) {
		t.Fatalf("registry injection copied %d indexers, want %d", len(fromRegistry.indexers), len(registry))
	}
	for harness, indexer := range registry {
		got, ok := fromRegistry.indexers[harness]
		if !ok || got != TranscriptIndexer(indexer) {
			t.Fatalf("registry injection lost harness %q", harness)
		}
	}
	// Every injected value keeps its dynamic type and both surfaces: the seam
	// is weak, but it does not erase what was put into it.
	if _, ok := fromRegistry.indexers[HarnessStrike].(*StrikeIndexer); !ok {
		t.Fatalf("the seam changed a registry indexer's dynamic type: got %T", fromRegistry.indexers[HarnessStrike])
	}
	if _, ok := fromRegistry.indexers[HarnessClaudeCode].(AuthoritativeTranscriptIndexer); !ok {
		t.Fatal("the seam dropped the strict capture surface of a registry indexer")
	}

	// An explicit compatibility-typed map compiles unchanged and is copied too.
	weak := map[Harness]TranscriptIndexer{HarnessClaudeCode: registry[HarnessClaudeCode]}
	fromWeak := &Pipeline{}
	WithIndexers(weak)(fromWeak)
	delete(weak, HarnessClaudeCode)
	weak[HarnessCodex] = registry[HarnessCodex]
	if fromWeak.indexers[HarnessClaudeCode] == nil {
		t.Fatal("the pipeline aliases the caller's map: deleting an entry reached the pipeline")
	}
	if _, ok := fromWeak.indexers[HarnessCodex]; ok {
		t.Fatal("the pipeline aliases the caller's map: a later insertion reached the pipeline")
	}

	// A nil entry is not copied, so the seam never carries an indexer dispatch
	// would panic on; a non-nil entry beside it still arrives.
	withNil := map[Harness]TranscriptIndexer{HarnessClaudeCode: nil, HarnessCodex: registry[HarnessCodex]}
	fromMixed := &Pipeline{}
	WithIndexers(withNil)(fromMixed)
	if _, ok := fromMixed.indexers[HarnessClaudeCode]; ok {
		t.Fatal("a nil indexer entry was copied into the pipeline seam")
	}
	if _, ok := fromMixed.indexers[HarnessCodex]; !ok {
		t.Fatal("a non-nil indexer beside a nil entry was dropped")
	}

	// A nil map leaves the seam unset.
	var absent map[Harness]TranscriptIndexer
	fromNil := &Pipeline{indexers: map[Harness]TranscriptIndexer{HarnessClaudeCode: registry[HarnessClaudeCode]}}
	WithIndexers(absent)(fromNil)
	if fromNil.indexers != nil {
		t.Fatal("a nil map must leave the seam unset")
	}
}
