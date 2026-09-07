package ingest

import (
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
)

// TestSessionColumnSupportIsCachedPerSource proves that the session table's
// column support is read once per source and reused, rather than re-read on
// every session-record page.
func TestSessionColumnSupportIsCachedPerSource(t *testing.T) {
	materialized := testfixture.MaterializeByName(t, "legacy-message-part")
	path, err := NewOpenCodeSQLiteSourcePath(materialized.Path)
	if err != nil {
		t.Fatalf("validate synthetic source path: %v", err)
	}
	opened, err := OpenOpenCodeSQLiteSource(t.Context(), path, DefaultOpenCodeSQLiteSourceOptions())
	if err != nil {
		t.Fatalf("open synthetic source: %v", err)
	}
	source := opened.(*zombiezenOpenCodeSQLiteSource)
	defer func() { _ = source.Close(t.Context()) }()

	pageSize, err := NewOpenCodeCurrentPageSize(MaxOpenCodeCurrentPageSize)
	if err != nil {
		t.Fatalf("build page size: %v", err)
	}
	first, err := source.SessionRecords(t.Context(), OpenCodeSessionRecordPageRequest{PageSize: pageSize})
	if err != nil {
		t.Fatalf("read first session-record page: %v", err)
	}
	if !first.HasParent || !first.HasClock {
		t.Fatalf("first page column support = parent %t clock %t, want both present for the legacy session table", first.HasParent, first.HasClock)
	}
	// Poison the cache with a support value the real schema does not have. A
	// second read must return the cached value, proving the pragma is not re-run.
	source.stateMu.Lock()
	source.sessionColumns = openCodeSessionColumnSupport{table: OpenCodeSessionTableLegacy, hasID: true, present: true, hasParent: false, hasClock: false}
	source.stateMu.Unlock()

	second, err := source.SessionRecords(t.Context(), OpenCodeSessionRecordPageRequest{PageSize: pageSize})
	if err != nil {
		t.Fatalf("read second session-record page: %v", err)
	}
	if second.HasParent || second.HasClock {
		t.Fatalf("second page re-read the session columns instead of using the cache: parent %t clock %t", second.HasParent, second.HasClock)
	}
}
