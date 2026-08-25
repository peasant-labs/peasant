package ingest_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
)

// TestOpenCodeBatchReadFailureDemotesOnlyClocklessSessions proves a failed row
// freshness aggregate read never demotes a clock-bearing session. One database
// holds a clock-bearing session and a clockless one, its legacy aggregate read
// is fault-injected, and only the clockless session falls to the database and
// WAL mtime floor and is named in the floor-fallback diagnostic. The
// clock-bearing session keeps its session clock as its changed time and is not
// named.
func TestOpenCodeBatchReadFailureDemotesOnlyClocklessSessions(t *testing.T) {
	const (
		clockBearing   = "ses_3cd91f52effeXd3QAJ54jOyzvE"
		clockless      = "ses_3cd91f52effeXd3QAJ54jOyzvF"
		clockBearingMS = 1010
	)
	materialized := testfixture.MaterializeByName(t, "session-clock-present-and-absent")
	databasePath := materialized.Path
	root, err := ingest.NewResolvedPath(filepath.Dir(databasePath))
	if err != nil {
		t.Fatal(err)
	}
	// One session loses its clock, so only it needs the row aggregate the fault
	// fails. The other keeps the clock the fixture materialized from its rows.
	updateSyntheticSessionClock(t, databasePath, clockless, 0)

	floor := time.UnixMilli(1_600_000_000_000)
	setDatabaseModTime(t, databasePath, floor)

	opener := func(ctx context.Context, path ingest.OpenCodeSQLiteSourcePath, options ingest.OpenCodeSQLiteSourceOptions) (ingest.OpenCodeSQLiteSource, error) {
		source, openErr := ingest.OpenOpenCodeSQLiteSource(ctx, path, options)
		if openErr != nil {
			return nil, openErr
		}
		return legacyFreshnessFailingSource{OpenCodeSQLiteSource: source}, nil
	}
	filesystem := &ingest.OSFileSystem{}
	environment := mountedCurrentEnvironment{"OPENCODE_DB": databasePath}
	adapter, err := ingest.NewOpenCodeAdapterWithCandidateProbe(filesystem, testutil.NoGitResolver(), salt.Salt{}, "latest", environment, filesystem, opener, ingest.DefaultOpenCodeSQLiteSourceOptions())
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := adapter.Discover(t.Context(), ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{root}})
	if err != nil {
		t.Fatal(err)
	}

	byID := make(map[string]ingest.DiscoveredSession, len(sessions))
	for _, session := range sessions {
		byID[string(session.SessionID)] = session
	}
	clocklessSession, keptClockless := byID[clockless]
	if !keptClockless {
		t.Fatalf("the clockless session was dropped by the failed aggregate read: %v", byID)
	}
	if !clocklessSession.ModTime.Equal(floor) {
		t.Fatalf("clockless session did not fall to the mtime floor: ModTime=%s want %s", clocklessSession.ModTime, floor)
	}
	clockBearingSession, keptClockBearing := byID[clockBearing]
	if !keptClockBearing {
		t.Fatalf("the clock-bearing session was dropped by the failed aggregate read: %v", byID)
	}
	if !clockBearingSession.ModTime.Equal(time.UnixMilli(clockBearingMS)) {
		t.Fatalf("clock-bearing session did not keep its session clock: ModTime=%s want %s", clockBearingSession.ModTime, time.UnixMilli(clockBearingMS))
	}

	var floorFallback string
	for _, evidence := range adapter.CandidateEvidence() {
		if filepath.Clean(evidence.Candidate.Path) != filepath.Clean(databasePath) {
			continue
		}
		for _, diagnostic := range evidence.Diagnostics {
			if diagnostic.Stage == ingest.OpenCodeProbeFreshness {
				floorFallback = diagnostic.What
			}
		}
	}
	if !strings.Contains(floorFallback, clockless) {
		t.Fatalf("the floor-fallback diagnostic %q does not name the clockless session %q", floorFallback, clockless)
	}
	if strings.Contains(floorFallback, clockBearing) {
		t.Fatalf("the floor-fallback diagnostic %q named the clock-bearing session %q, which kept its clock", floorFallback, clockBearing)
	}
}
