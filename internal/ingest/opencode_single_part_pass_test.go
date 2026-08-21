package ingest_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

type partReadCounts struct {
	mu           sync.Mutex
	legacyParts  int
	orphanParts  int
	sessionParts int
}

type partReadCountingSource struct {
	ingest.OpenCodeSQLiteSource
	counts *partReadCounts
}

func (source partReadCountingSource) LegacyParts(ctx context.Context, request ingest.OpenCodeLegacyPartPageRequest) (ingest.OpenCodeLegacyPartPage, error) {
	source.counts.mu.Lock()
	source.counts.legacyParts++
	source.counts.mu.Unlock()
	return source.OpenCodeSQLiteSource.LegacyParts(ctx, request)
}

func (source partReadCountingSource) LegacyOrphanParts(ctx context.Context, request ingest.OpenCodeLegacyOrphanPartPageRequest) (ingest.OpenCodeLegacyPartPage, error) {
	source.counts.mu.Lock()
	source.counts.orphanParts++
	source.counts.mu.Unlock()
	return source.OpenCodeSQLiteSource.LegacyOrphanParts(ctx, request)
}

func (source partReadCountingSource) LegacySessionParts(ctx context.Context, request ingest.OpenCodeLegacySessionPartPageRequest) (ingest.OpenCodeLegacyPartPage, error) {
	source.counts.mu.Lock()
	source.counts.sessionParts++
	source.counts.mu.Unlock()
	return source.OpenCodeSQLiteSource.LegacySessionParts(ctx, request)
}

// TestOpenCodeLegacyProjectionReadsPartsOnce proves that materializing a legacy
// session reads the part table once through the session-part pass and never
// through the per-message part read or the correlated orphan scan.
func TestOpenCodeLegacyProjectionReadsPartsOnce(t *testing.T) {
	materialized := testfixture.MaterializeByName(t, "legacy-orphan-tolerance")
	root, err := ingest.NewResolvedPath(filepath.Dir(materialized.Path))
	if err != nil {
		t.Fatalf("resolve synthetic OpenCode root: %v", err)
	}
	const session = "ses_3cd91f52effeXd3QAJ54jOyzO1"
	withCanonicalConnection(t, materialized.Path, func(connection *sqlite.Conn) error {
		return sqlitex.Execute(connection, `INSERT INTO part(id, message_id, session_id, time_created, time_updated, data) VALUES(?1, ?2, ?3, ?4, ?4, ?5)`, &sqlitex.ExecOptions{Args: []any{"part_usable_orphan", "msg_absent_orphan", session, 1300, `{"id":"part_usable_orphan","type":"text","text":"USABLE_ORPHAN"}`}})
	})

	counts := &partReadCounts{}
	opener := func(ctx context.Context, path ingest.OpenCodeSQLiteSourcePath, options ingest.OpenCodeSQLiteSourceOptions) (ingest.OpenCodeSQLiteSource, error) {
		source, openErr := ingest.OpenOpenCodeSQLiteSource(ctx, path, options)
		if openErr != nil {
			return nil, openErr
		}
		return partReadCountingSource{OpenCodeSQLiteSource: source, counts: counts}, nil
	}
	filesystem := &ingest.OSFileSystem{}
	adapter, err := ingest.NewOpenCodeAdapterWithCandidateProbe(filesystem, testutil.NoGitResolver(), salt.Salt{}, "latest", fixedCandidateEnvironment{}, filesystem, opener, ingest.DefaultOpenCodeSQLiteSourceOptions())
	if err != nil {
		t.Fatalf("construct candidate-capable adapter: %v", err)
	}
	discovered, err := adapter.Discover(t.Context(), ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{root}})
	if err != nil {
		t.Fatalf("discover legacy session: %v", err)
	}
	var host *ingest.DiscoveredSession
	for index := range discovered {
		if string(discovered[index].SessionID) == session {
			host = &discovered[index]
		}
	}
	if host == nil {
		t.Fatalf("discovery = %+v, want the legacy host session", discovered)
	}
	metadata, data, err := adapter.MaterializeTranscript(t.Context(), *host)
	if err != nil || metadata == nil || len(data) == 0 {
		t.Fatalf("materialize legacy session failed: err=%v data=%d", err, len(data))
	}
	counts.mu.Lock()
	legacyParts, orphanParts, sessionParts := counts.legacyParts, counts.orphanParts, counts.sessionParts
	counts.mu.Unlock()
	if sessionParts == 0 {
		t.Fatalf("session-part pass was never used: legacy=%d orphan=%d session=%d", legacyParts, orphanParts, sessionParts)
	}
	if legacyParts != 0 || orphanParts != 0 {
		t.Fatalf("part table was read twice: per-message=%d orphan-scan=%d, want the single session-part pass only", legacyParts, orphanParts)
	}
}
