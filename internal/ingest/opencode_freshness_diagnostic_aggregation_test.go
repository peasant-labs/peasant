package ingest_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
)

// legacyFreshnessFailingSource fails the batch legacy freshness read so every
// session on the database falls back to the mtime floor.
type legacyFreshnessFailingSource struct {
	ingest.OpenCodeSQLiteSource
}

func (source legacyFreshnessFailingSource) LegacyFreshnessBySession(context.Context) (map[string]time.Time, error) {
	return nil, errors.New("synthetic legacy freshness read failure")
}

// TestOpenCodeFreshnessDiagnosticAggregatedPerPath proves that a database whose
// row freshness read fails for every clockless session records one freshness
// diagnostic naming the affected sessions, not one diagnostic per session. Both
// sessions lose their session clock so both take the row-aggregate path that the
// failing read exercises.
func TestOpenCodeFreshnessDiagnosticAggregatedPerPath(t *testing.T) {
	materialized := testfixture.MaterializeByName(t, "session-clock-present-and-absent")
	updateSyntheticSessionClock(t, materialized.Path, "ses_3cd91f52effeXd3QAJ54jOyzvE", 0)
	updateSyntheticSessionClock(t, materialized.Path, "ses_3cd91f52effeXd3QAJ54jOyzvF", 0)
	root, err := ingest.NewResolvedPath(filepath.Dir(materialized.Path))
	if err != nil {
		t.Fatalf("resolve synthetic OpenCode root: %v", err)
	}
	opener := func(ctx context.Context, path ingest.OpenCodeSQLiteSourcePath, options ingest.OpenCodeSQLiteSourceOptions) (ingest.OpenCodeSQLiteSource, error) {
		source, openErr := ingest.OpenOpenCodeSQLiteSource(ctx, path, options)
		if openErr != nil {
			return nil, openErr
		}
		return legacyFreshnessFailingSource{OpenCodeSQLiteSource: source}, nil
	}
	filesystem := &ingest.OSFileSystem{}
	adapter, err := ingest.NewOpenCodeAdapterWithCandidateProbe(filesystem, testutil.NoGitResolver(), salt.Salt{}, "latest", fixedCandidateEnvironment{}, filesystem, opener, ingest.DefaultOpenCodeSQLiteSourceOptions())
	if err != nil {
		t.Fatalf("construct candidate-capable adapter: %v", err)
	}
	discovered, err := adapter.Discover(t.Context(), ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{root}})
	if err != nil {
		t.Fatalf("discover with a failing freshness read: %v", err)
	}
	if len(discovered) != 2 {
		t.Fatalf("discovery kept %d sessions, want both sessions on the mtime floor", len(discovered))
	}
	freshnessDiagnostics := 0
	var message string
	for _, evidence := range adapter.CandidateEvidence() {
		if filepath.Clean(evidence.Candidate.Path) != filepath.Clean(materialized.Path) {
			continue
		}
		for _, diagnostic := range evidence.Diagnostics {
			if diagnostic.Stage == ingest.OpenCodeProbeFreshness {
				freshnessDiagnostics++
				message = diagnostic.What
			}
		}
	}
	if freshnessDiagnostics != 1 {
		t.Fatalf("freshness diagnostics on the path = %d, want exactly one aggregated diagnostic for both sessions", freshnessDiagnostics)
	}
	if !strings.Contains(message, "ses_3cd91f52effeXd3QAJ54jOyzvE") || !strings.Contains(message, "ses_3cd91f52effeXd3QAJ54jOyzvF") {
		t.Fatalf("aggregated freshness diagnostic %q does not name both affected sessions", message)
	}
}
