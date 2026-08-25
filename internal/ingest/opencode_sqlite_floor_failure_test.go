package ingest_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
)

// statFailFileSystem fails Stat for one target path, so a test can force the
// content-mtime floor read to fail while the source still opens.
type statFailFileSystem struct {
	*ingest.OSFileSystem
	failPath string
}

var _ ingest.FileSystem = (*statFailFileSystem)(nil)
var _ ingest.OpenCodeCandidateFileSystem = (*statFailFileSystem)(nil)

func (fsys *statFailFileSystem) Stat(path string) (os.FileInfo, error) {
	if filepath.Clean(path) == filepath.Clean(fsys.failPath) {
		return nil, os.ErrPermission
	}
	return fsys.OSFileSystem.Stat(path)
}

// TestOpenCodeFloorReadFailureSkipsOnlyTheAffectedSession proves a failed
// content-mtime floor read skips only the clockless session it belongs to and
// names it, while a clock-bearing session on the same database still discovers.
func TestOpenCodeFloorReadFailureSkipsOnlyTheAffectedSession(t *testing.T) {
	const (
		clockless    = "ses_3cd91f52effeXd3QAJ54jOyzvE"
		clockBearing = "ses_3cd91f52effeXd3QAJ54jOyzvF"
	)
	materialized := testfixture.MaterializeByName(t, "session-clock-present-and-absent")
	databasePath := materialized.Path
	root, err := ingest.NewResolvedPath(filepath.Dir(databasePath))
	if err != nil {
		t.Fatal(err)
	}
	// The earlier-sorting session loses its clock, so it takes the floor path;
	// the later-sorting session keeps its clock and never needs the floor.
	updateSyntheticSessionClock(t, databasePath, clockless, 0)

	// Fail the WAL sidecar stat, so the content-mtime floor read fails while
	// candidate resolution and the source open still succeed.
	filesystem := &statFailFileSystem{OSFileSystem: &ingest.OSFileSystem{}, failPath: databasePath + "-wal"}
	environment := mountedCurrentEnvironment{"OPENCODE_DB": databasePath}
	adapter, err := ingest.NewOpenCodeAdapterWithCandidateProbe(filesystem, testutil.NoGitResolver(), salt.Salt{}, "latest", environment, filesystem, ingest.OpenOpenCodeSQLiteSource, ingest.DefaultOpenCodeSQLiteSourceOptions())
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := adapter.Discover(t.Context(), ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{root}})
	if err != nil {
		t.Fatal(err)
	}

	discovered := make(map[string]bool, len(sessions))
	for _, session := range sessions {
		discovered[string(session.SessionID)] = true
	}
	if discovered[clockless] {
		t.Fatalf("the session whose floor read failed was still discovered: %v", discovered)
	}
	if !discovered[clockBearing] {
		t.Fatalf("a clock-bearing session on the same database was dropped by the floor failure: %v", discovered)
	}

	named := false
	for _, evidence := range adapter.CandidateEvidence() {
		for _, diagnostic := range evidence.Diagnostics {
			if strings.Contains(diagnostic.What, clockless) {
				named = true
			}
			if strings.Contains(diagnostic.What, clockBearing) {
				t.Fatalf("the clock-bearing session was named in a skip diagnostic: %q", diagnostic.What)
			}
		}
	}
	if !named {
		t.Fatalf("the floor failure did not record a per-session diagnostic naming %q", clockless)
	}
}
