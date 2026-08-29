package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

const (
	reindexSessionID   = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	reindexProjectHash = "3333333333333333333333333333333333333333333333333333333333333333"
	reindexAnnotator   = "frustration-classifier"
	reindexTypeID      = "quality.frustration_signal"
)

// TestIndexSessionEntries_CarriesEntryAnnotationsAcrossAReindex proves that
// re-indexing a session leaves its entry-targeted annotations attached.
//
// annotation_target_entries has no ON DELETE CASCADE, so a re-index has to clear
// it by hand before the entries it references go. Clearing it and stopping there
// left the annotations row alive with nothing to point at — an orphan that
// 'peasant ingest verify' counts as healthy and that fails the wire contract's
// target-arm validation before a single request is made, so every subsequent
// push used to stop on it. Under a git hook that meant a failure on every commit,
// and the remedy Peasant printed for a
// different problem ('peasant ingest --force --session <id>') is exactly what
// created it.
func TestIndexSessionEntries_CarriesEntryAnnotationsAcrossAReindex(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	seedReindexSession(t, s)
	indexReindexEntries(t, s, 4)
	annotationID := annotateReindexEntry(t, s, 1, 2)

	if got := entryTargetCount(t, s, annotationID); got != 1 {
		t.Fatalf("seeded annotation has %d target row(s), want 1", got)
	}

	// The exact operation 'peasant ingest --force --session <id>' performs.
	indexReindexEntries(t, s, 4)

	if got := entryTargetCount(t, s, annotationID); got != 1 {
		t.Fatalf("after re-indexing, annotation %s has %d target row(s), want 1: an entry annotation with no target cannot be published",
			annotationID, got)
	}
	assertReindexTargetSpan(t, s, annotationID, 1, 2)

	// Drive the next step of the printed recovery sequence through the real
	// annotation conversion path. A preserved target must remain publishable;
	// merely retaining a child row is not enough if it no longer forms a valid
	// wire target.
	summary, err := push.PushAnnotationsSelected(context.Background(), nil, s, push.AnnotationSelection{}, true, 1)
	if err != nil {
		t.Fatalf("dry-run annotation push after re-index: %v", err)
	}
	if len(summary.Unpublishable) != 0 {
		t.Fatalf("re-index left %d unpublishable annotation(s), want none: %+v", len(summary.Unpublishable), summary.Unpublishable)
	}
	if summary.Total == 0 {
		t.Fatal("the entry annotation disappeared from the publishable set after re-index")
	}
}

// TestIndexSessionEntries_RollsBackWhenAnAnnotatedEntryIsGone covers the one case
// an attachment cannot safely move: the entry it described is no longer in the
// transcript.
//
// Detaching the target creates the poison row this repair exists to prevent, and
// deleting the annotation would lose user data. The whole replacement therefore
// rolls back, retaining the old entries and target until the user removes or
// recreates the annotation deliberately.
func TestIndexSessionEntries_RollsBackWhenAnAnnotatedEntryIsGone(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	seedReindexSession(t, s)
	indexReindexEntries(t, s, 4)
	// The span starts at entry 1, which survives, but extends through entries 2
	// and 3, which do not. Checking only the foreign-keyed start would silently
	// preserve an annotation over content that no longer exists.
	lost := annotateReindexEntry(t, s, 1, 4)

	// The transcript shrank: entry 3 no longer exists.
	err := reindexEntries(s, 2)
	if err == nil {
		t.Fatal("re-index succeeded after deleting an annotated entry; want a rollback that preserves the target")
	}
	for _, want := range []string{lost, "[1,4)", "rolled back", "annotate prune", "--dry-run"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("re-index error does not contain %q:\n%v", want, err)
		}
	}
	if got := entryTargetCount(t, s, lost); got != 1 {
		t.Errorf("annotation %s has %d target row(s) after rollback, want 1", lost, got)
	}
	entries, listErr := s.ListEntries(context.Background(), schema.SessionID(reindexSessionID))
	if listErr != nil {
		t.Fatalf("list entries after rollback: %v", listErr)
	}
	if len(entries) != 4 {
		t.Errorf("entries after rollback = %d, want the original 4", len(entries))
	}
}

func TestIndexSessionEntries_RemapsEntryAnnotationByEntryID(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	seedReindexSession(t, s)
	indexReindexCustomEntries(t, s, []schema.SessionEntry{
		reindexEntry(0, "entry-0", "alpha"),
		reindexEntry(1, "entry-1", "bravo"),
		reindexEntry(2, "entry-2", "charlie"),
	})
	annotationID := annotateReindexEntry(t, s, 1, 2)

	indexReindexCustomEntries(t, s, []schema.SessionEntry{
		reindexEntry(0, "entry-0", "alpha"),
		reindexEntry(1, "entry-new", "new text"),
		reindexEntry(2, "entry-1", "bravo"),
		reindexEntry(3, "entry-2", "charlie"),
	})

	assertReindexTargetSpan(t, s, annotationID, 2, 3)
}

func TestIndexSessionEntries_DoesNotSkipInvalidEntryAnnotationSpan(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	seedReindexSession(t, s)
	indexReindexEntries(t, s, 2)
	annotationID := annotateReindexEntry(t, s, 1, 2)
	setReindexTargetEnd(t, s, annotationID, 3)

	err := reindexEntries(s, 2)
	if err == nil {
		t.Fatal("identical re-index succeeded with an invalid existing annotation span; want fail-safe rollback")
	}
	for _, want := range []string{annotationID, "rolled back", "no unique contiguous anchor match"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("re-index error does not contain %q:\n%v", want, err)
		}
	}
	assertReindexTargetSpan(t, s, annotationID, 1, 3)
}

func TestIndexSessionEntries_RollsBackWhenEntryAnnotationRemapIsAmbiguous(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	seedReindexSession(t, s)
	indexReindexCustomEntries(t, s, []schema.SessionEntry{
		reindexContentEntry(0, "alpha"),
		reindexContentEntry(1, "same text"),
		reindexContentEntry(2, "charlie"),
	})
	ambiguous := annotateReindexEntry(t, s, 1, 2)

	err := reindexCustomEntries(s, []schema.SessionEntry{
		reindexContentEntry(0, "alpha"),
		reindexContentEntry(2, "same text"),
		reindexContentEntry(3, "same text"),
	})
	if err == nil {
		t.Fatal("re-index succeeded with two possible annotation targets; want rollback")
	}
	if !strings.Contains(err.Error(), "no unique contiguous anchor match") {
		t.Fatalf("re-index error does not explain ambiguous remap:\n%v", err)
	}
	assertReindexTargetSpan(t, s, ambiguous, 1, 2)
	entries, listErr := s.ListEntries(context.Background(), schema.SessionID(reindexSessionID))
	if listErr != nil {
		t.Fatalf("list entries after ambiguous rollback: %v", listErr)
	}
	if len(entries) != 3 {
		t.Fatalf("entries after ambiguous rollback = %d, want original 3", len(entries))
	}
}

// --- helpers ---------------------------------------------------------------

func seedReindexSession(t *testing.T, s *store.Store) {
	t.Helper()
	const startMs = 1700000000000
	ingested := int64(startMs + 120000)
	entry := ingest.StoreEntry{
		Metadata: &schema.UnifiedMetadata{
			SchemaVersion: ingest.CurrentSchemaVersion,
			SessionID:     schema.SessionID(reindexSessionID),
			ModelHarness:  defaults.HarnessClaudeCode,
			Model:         schema.ModelID("claude-opus-4-6"),
			HostSlug:      schema.HostSlug("github.com-test-reindex"),
			Project: schema.ProjectContext{
				Hash:     schema.ProjectHash(reindexProjectHash),
				Name:     "test-project",
				FilePath: "/test/project",
			},
			Timestamp: schema.TimestampInfo{Start: startMs, End: startMs + 60000, Ingested: &ingested},
			Source:    schema.SourceInfo{FilePath: "/test/session.jsonl", Format: schema.SourceFormatJSONL},
		},
	}
	if err := s.InsertSessions(context.Background(), []ingest.StoreEntry{entry}); err != nil {
		t.Fatalf("seed session: %v", err)
	}
}

func indexReindexEntries(t *testing.T, s *store.Store, count int) {
	t.Helper()
	if err := reindexEntries(s, count); err != nil {
		t.Fatalf("IndexSessionEntries(%d entries): %v", count, err)
	}
}

func reindexEntries(s *store.Store, count int) error {
	entries := make([]schema.SessionEntry, count)
	for i := range entries {
		entries[i] = schema.SessionEntry{
			SessionID:  schema.SessionID(reindexSessionID),
			EntryIndex: i,
			Harness:    ingest.HarnessClaudeCode,
			EntryType:  schema.EntryTypeText,
			Role:       schema.RoleAssistant,
		}
	}
	return s.IndexSessionEntries(context.Background(), schema.SessionID(reindexSessionID), entries)
}

func indexReindexCustomEntries(t *testing.T, s *store.Store, entries []schema.SessionEntry) {
	t.Helper()
	if err := reindexCustomEntries(s, entries); err != nil {
		t.Fatalf("IndexSessionEntries(custom): %v", err)
	}
}

func reindexCustomEntries(s *store.Store, entries []schema.SessionEntry) error {
	return s.IndexSessionEntries(context.Background(), schema.SessionID(reindexSessionID), entries)
}

func reindexEntry(index int, entryID string, content string) schema.SessionEntry {
	return schema.SessionEntry{
		SessionID:      schema.SessionID(reindexSessionID),
		EntryIndex:     index,
		Harness:        ingest.HarnessClaudeCode,
		EntryType:      schema.EntryTypeText,
		Role:           schema.RoleAssistant,
		EntryID:        strPtr(entryID),
		ContentPreview: strPtr(content),
	}
}

func reindexContentEntry(index int, content string) schema.SessionEntry {
	return schema.SessionEntry{
		SessionID:      schema.SessionID(reindexSessionID),
		EntryIndex:     index,
		Harness:        ingest.HarnessClaudeCode,
		EntryType:      schema.EntryTypeText,
		Role:           schema.RoleAssistant,
		ContentPreview: strPtr(content),
	}
}

// annotateReindexEntry attaches one entry annotation to [entryIndex, endIndex)
// and returns its store ID.
func annotateReindexEntry(t *testing.T, s *store.Store, entryIndex, endIndex int) string {
	t.Helper()
	ctx := context.Background()
	annotatorID, err := s.GetAnnotatorIDByName(ctx, reindexAnnotator)
	if err != nil || annotatorID == "" {
		t.Fatalf("resolve annotator %q: id=%q err=%v", reindexAnnotator, annotatorID, err)
	}
	typeID, err := s.GetAnnotationTypeID(ctx, reindexTypeID)
	if err != nil || typeID == "" {
		t.Fatalf("resolve annotation type %q: id=%q err=%v", reindexTypeID, typeID, err)
	}
	id, err := s.CreateEntryAnnotation(ctx, ingest.EntryAnnotationParams{
		SessionID:        reindexSessionID,
		EntryIndex:       entryIndex,
		EndIndex:         endIndex,
		AnnotatorID:      annotatorID,
		AnnotationTypeID: typeID,
		Value:            "detected",
	})
	if err != nil {
		t.Fatalf("CreateEntryAnnotation(%d,%d): %v", entryIndex, endIndex, err)
	}
	return id
}

// entryTargetCount reports how many attachments an annotation currently has.
func entryTargetCount(t *testing.T, s *store.Store, annotationID string) int {
	t.Helper()
	count := 0
	forEachTargetEntry(t, s, annotationID, func(_, _ int) { count++ })
	return count
}

// assertReindexTargetSpan checks the restored attachment names the same span.
func assertReindexTargetSpan(t *testing.T, s *store.Store, annotationID string, wantStart, wantEnd int) {
	t.Helper()
	forEachTargetEntry(t, s, annotationID, func(start, end int) {
		if start != wantStart || end != wantEnd {
			t.Errorf("restored span = [%d,%d), want [%d,%d): an attachment that moves points the annotation at different content",
				start, end, wantStart, wantEnd)
		}
	})
}

func setReindexTargetEnd(t *testing.T, s *store.Store, annotationID string, endIndex int) {
	t.Helper()
	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)
	if err := sqlitex.ExecuteTransient(conn, `UPDATE annotation_target_entries SET end_index = ? WHERE annotation_id = ?`, &sqlitex.ExecOptions{Args: []any{endIndex, annotationID}}); err != nil {
		t.Fatalf("update annotation target end_index for %s: %v", annotationID, err)
	}
}

// forEachTargetEntry reads the attachments straight out of the table, so the
// assertion is about what is actually stored rather than about what a reader
// chooses to report.
func forEachTargetEntry(t *testing.T, s *store.Store, annotationID string, visit func(start, end int)) {
	t.Helper()
	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)
	err := sqlitex.ExecuteTransient(conn,
		`SELECT entry_index, end_index FROM annotation_target_entries WHERE annotation_id = ? ORDER BY entry_index`,
		&sqlitex.ExecOptions{
			Args: []any{annotationID},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				visit(stmt.ColumnInt(0), stmt.ColumnInt(1))
				return nil
			},
		})
	if err != nil {
		t.Fatalf("read annotation targets: %v", err)
	}
}
