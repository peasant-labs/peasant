package e2e

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
)

func loadCursorFixtureIndex(t *testing.T) *fixtureIndex {
	t.Helper()
	m, err := LoadFixtureIndex(filepath.Join(CursorFixtureSourcePath(), fixtureIndexFile))
	if err != nil {
		t.Fatalf("load cursor fixture index: %v", err)
	}
	return m
}

func discoverCursorFixture(t *testing.T) []ingest.DiscoveredSession {
	t.Helper()
	sourcePath, err := ingest.NewResolvedPath(CursorFixtureSourcePath())
	if err != nil {
		t.Fatalf("NewResolvedPath(cursor fixture): %v", err)
	}
	adapter := ingest.NewCursorAdapter(&ingest.OSFileSystem{}, testutil.NoGitResolver(), salt.Salt{})
	sessions, err := adapter.Discover(context.Background(), ingest.SourceConfig{
		Enabled: true,
		Paths:   []ingest.ResolvedPath{sourcePath},
	})
	if err != nil {
		t.Fatalf("CursorAdapter.Discover: %v", err)
	}
	return sessions
}

func TestFixture_CursorDiscover(t *testing.T) {
	idx := loadCursorFixtureIndex(t)
	sessions := discoverCursorFixture(t)
	if len(sessions) != ExpectedCursorTranscripts {
		t.Fatalf("Discover returned %d sessions, want %d", len(sessions), ExpectedCursorTranscripts)
	}

	rootSID, err := ingest.NewSessionID(CursorFixtureRootSessionID)
	if err != nil {
		t.Fatalf("NewSessionID(root): %v", err)
	}
	abortedSID, err := ingest.NewSessionID(CursorFixtureAbortedSessionID)
	if err != nil {
		t.Fatalf("NewSessionID(aborted): %v", err)
	}

	var root, aborted *ingest.DiscoveredSession
	for i := range sessions {
		switch sessions[i].SessionID {
		case rootSID:
			root = &sessions[i]
		case abortedSID:
			aborted = &sessions[i]
		}
	}
	if root == nil {
		t.Fatalf("root session %q not discovered", CursorFixtureRootSessionID)
	}
	if aborted == nil {
		t.Fatalf("aborted session %q not discovered", CursorFixtureAbortedSessionID)
	}

	wantRootPath := filepath.Join(CursorFixtureSourcePath(), idx.Sessions[0].Path)
	if root.SourcePath.String() != wantRootPath {
		t.Errorf("root.SourcePath = %q, want %q", root.SourcePath, wantRootPath)
	}
	if root.Harness != ingest.HarnessCursor {
		t.Errorf("root.Harness = %q, want %q", root.Harness, ingest.HarnessCursor)
	}
	if root.Title == "" {
		t.Error("root.Title is empty")
	}
	if root.CWD == "" {
		t.Error("root.CWD is empty")
	}

	wantAbortedPath := filepath.Join(CursorFixtureSourcePath(), idx.Sessions[1].Path)
	if aborted.SourcePath.String() != wantAbortedPath {
		t.Errorf("aborted.SourcePath = %q, want %q", aborted.SourcePath, wantAbortedPath)
	}
}

func TestFixture_CursorIndexer(t *testing.T) {
	sessions := discoverCursorFixture(t)
	rootSID, err := ingest.NewSessionID(CursorFixtureRootSessionID)
	if err != nil {
		t.Fatalf("NewSessionID(root): %v", err)
	}

	var root ingest.DiscoveredSession
	for _, s := range sessions {
		if s.SessionID == rootSID {
			root = s
			break
		}
	}
	if root.SessionID == "" {
		t.Fatalf("root session %q not in Discover output", CursorFixtureRootSessionID)
	}

	idx := ingest.NewCursorIndexer(&ingest.OSFileSystem{}, ingest.WithCursorFullDepth(true))
	entries, err := idx.IndexTranscript(context.Background(), root)
	if err != nil {
		t.Fatalf("IndexTranscript(root): %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("IndexTranscript(root) returned no entries")
	}

	toolUse := 0
	for _, e := range entries {
		if e.HasToolUse {
			toolUse++
		}
	}
	if toolUse == 0 {
		t.Fatal("indexed root session has no tool_use entries — fixture must preserve tooling")
	}
}
