package ingest_test

import (
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
)

// TestOpenCodeHybridDiscoversCurrentOnly proves that a hybrid database whose
// current projection discovers successfully contributes current sessions only.
// A session that lives in the legacy message and part tables but was deleted
// from the current session_message projection is not resurrected from the stale
// legacy rows.
func TestOpenCodeHybridDiscoversCurrentOnly(t *testing.T) {
	t.Parallel()
	materialized := testfixture.MaterializeByName(t, "hybrid-current-only-suppresses-legacy")
	root, err := ingest.NewResolvedPath(filepath.Dir(materialized.Path))
	if err != nil {
		t.Fatalf("resolve synthetic OpenCode root: %v", err)
	}
	filesystem := &ingest.OSFileSystem{}
	adapter, err := ingest.NewOpenCodeAdapterWithCandidateProbe(filesystem, testutil.NoGitResolver(), salt.Salt{}, "latest", fixedCandidateEnvironment{}, filesystem, ingest.OpenOpenCodeSQLiteSource, ingest.DefaultOpenCodeSQLiteSourceOptions())
	if err != nil {
		t.Fatalf("construct candidate-capable adapter: %v", err)
	}
	discovered, err := adapter.Discover(t.Context(), ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{root}})
	if err != nil {
		t.Fatalf("run hybrid OpenCode discovery: %v", err)
	}
	origins := make(map[string]ingest.TranscriptOrigin, len(discovered))
	for _, session := range discovered {
		origins[string(session.SessionID)] = session.TranscriptOrigin
	}
	const survivor = "ses_3cd91f52effeXd3QAJ54jOyzH1"
	const deletedFromCurrent = "ses_3cd91f52effeXd3QAJ54jOyzH2"
	if origin, ok := origins[survivor]; !ok || origin != ingest.TranscriptOriginOpenCodeCurrentSQLite {
		t.Fatalf("hybrid discovery = %+v, want the surviving session %q as a current SQLite winner", discovered, survivor)
	}
	if _, resurrected := origins[deletedFromCurrent]; resurrected {
		t.Fatalf("hybrid discovery resurrected legacy-only session %q from stale legacy rows: %+v", deletedFromCurrent, discovered)
	}
	if len(discovered) != 1 {
		t.Fatalf("hybrid discovery returned %d sessions, want only the one current session", len(discovered))
	}
}
