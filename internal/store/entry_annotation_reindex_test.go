package store_test

import (
	"context"
	"embed"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	peasantExport "github.com/peasant-labs/peasant/internal/export"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/annotation-repair/target-anchors.yaml
var annotationRepairFixturesFS embed.FS

type annotationRepairFixtures struct {
	Cases []annotationRepairCase `yaml:"cases"`
}

type annotationRepairCase struct {
	Name          string                            `yaml:"name"`
	Annotator     string                            `yaml:"annotator"`
	TypeID        string                            `yaml:"type_id"`
	OldEntries    []annotationRepairEntry           `yaml:"old_entries"`
	Target        annotationRepairTarget            `yaml:"target"`
	NewEntries    []annotationRepairEntry           `yaml:"new_entries"`
	WantState     store.AnnotationTargetAnchorState `yaml:"want_state"`
	WantStart     int                               `yaml:"want_start"`
	WantEnd       int                               `yaml:"want_end"`
	WantNoTarget  bool                              `yaml:"want_no_target"`
	WantPushError bool                              `yaml:"want_push_error"`
}

type annotationRepairEntry struct {
	Index      int    `yaml:"index"`
	EntryID    string `yaml:"entry_id"`
	ToolCallID string `yaml:"tool_call_id"`
	EntryType  string `yaml:"entry_type"`
	Role       string `yaml:"role"`
	PartType   string `yaml:"part_type"`
	Content    string `yaml:"content"`
}

type annotationRepairTarget struct {
	Start int `yaml:"start"`
	End   int `yaml:"end"`
}

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

func TestIndexSessionEntries_SupersedesClassifierTargetWhenAnnotatedEntryIsGone(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	seedReindexSession(t, s)
	indexReindexEntries(t, s, 4)
	// The span starts at entry 1, which survives, but extends through entries 2
	// and 3, which do not. Checking only the foreign-keyed start would silently
	// preserve an annotation over content that no longer exists.
	lost := annotateReindexEntry(t, s, 1, 4)

	if err := reindexEntries(s, 2); err != nil {
		t.Fatalf("re-index classifier target loss: %v", err)
	}
	assertAnchorState(t, s, lost, store.AnnotationTargetAnchorSuperseded)
	if got := entryTargetCount(t, s, lost); got != 0 {
		t.Errorf("annotation %s has %d target row(s) after superseded repair, want 0", lost, got)
	}
	entries, listErr := s.ListEntries(context.Background(), schema.SessionID(reindexSessionID))
	if listErr != nil {
		t.Fatalf("list entries after rollback: %v", listErr)
	}
	if len(entries) != 2 {
		t.Errorf("entries after repair = %d, want replacement 2", len(entries))
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

	if err := reindexEntries(s, 2); err != nil {
		t.Fatalf("identical re-index with invalid classifier span: %v", err)
	}
	assertAnchorState(t, s, annotationID, store.AnnotationTargetAnchorSuperseded)
	if got := entryTargetCount(t, s, annotationID); got != 0 {
		t.Fatalf("target rows after invalid classifier span repair = %d, want 0", got)
	}
}

func TestIndexSessionEntries_SupersedesClassifierTargetWhenRemapIsAmbiguous(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	seedReindexSession(t, s)
	indexReindexCustomEntries(t, s, []schema.SessionEntry{
		reindexContentEntry(0, "alpha"),
		reindexContentEntry(1, "same text"),
		reindexContentEntry(2, "charlie"),
	})
	ambiguous := annotateReindexEntry(t, s, 1, 2)

	if err := reindexCustomEntries(s, []schema.SessionEntry{
		reindexContentEntry(0, "alpha"),
		reindexContentEntry(2, "same text"),
		reindexContentEntry(3, "same text"),
	}); err != nil {
		t.Fatalf("re-index ambiguous classifier target: %v", err)
	}
	assertAnchorState(t, s, ambiguous, store.AnnotationTargetAnchorSuperseded)
	if got := entryTargetCount(t, s, ambiguous); got != 0 {
		t.Fatalf("target rows after ambiguous classifier repair = %d, want 0", got)
	}
	entries, listErr := s.ListEntries(context.Background(), schema.SessionID(reindexSessionID))
	if listErr != nil {
		t.Fatalf("list entries after ambiguous rollback: %v", listErr)
	}
	if len(entries) != 3 {
		t.Fatalf("entries after ambiguous classifier repair = %d, want replacement 3", len(entries))
	}
}

func TestIndexSessionEntries_TargetAnchorRepairFixtures(t *testing.T) {
	fixtures := loadAnnotationRepairFixtures(t)
	for _, tc := range fixtures.Cases {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			s := openTestStore(t)
			seedReindexSession(t, s)
			indexReindexCustomEntries(t, s, repairEntries(tc.OldEntries))
			annotationID := annotateReindexEntryWith(t, s, tc.Target.Start, tc.Target.End, tc.Annotator, tc.TypeID)

			if err := reindexCustomEntries(s, repairEntries(tc.NewEntries)); err != nil {
				t.Fatalf("IndexSessionEntries(%s): %v", tc.Name, err)
			}

			assertAnchorState(t, s, annotationID, tc.WantState)
			if tc.WantNoTarget {
				if got := entryTargetCount(t, s, annotationID); got != 0 {
					t.Fatalf("target rows after unresolved repair = %d, want 0", got)
				}
			} else {
				assertReindexTargetSpan(t, s, annotationID, tc.WantStart, tc.WantEnd)
			}
			if tc.WantPushError {
				_, err := push.PushAnnotationsSelected(context.Background(), nil, s, push.AnnotationSelection{}, true, 1)
				if err == nil {
					t.Fatal("push with unresolved target succeeded; want fail-closed error")
				}
				for _, want := range []string{annotationID, "unresolved annotation target", "how to fix"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("push error does not contain %q:\n%v", want, err)
					}
				}
				_, err = peasantExport.ExportAnnotations(context.Background(), s, reindexSessionID)
				if err == nil {
					t.Fatal("export with unresolved target succeeded; want fail-closed error")
				}
				for _, want := range []string{annotationID, "refused to export", "Fix:"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("export error does not contain %q:\n%v", want, err)
					}
				}
			}
		})
	}
}

func TestIndexSessionEntries_PushRetractsPublishedClassifierAfterTargetLoss(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	seedReindexSession(t, s)
	indexReindexCustomEntries(t, s, []schema.SessionEntry{
		reindexContentEntry(0, "alpha"),
		reindexContentEntry(1, "stale classifier target"),
		reindexContentEntry(2, "charlie"),
	})
	annotationID := annotateReindexEntry(t, s, 1, 2)
	oldItem := schema.AnnotationPushItem{
		TargetKind: schema.TargetEntry,
		EntryTarget: &schema.AnnotationEntryTarget{
			SessionID:  reindexSessionID,
			EntryIndex: 1,
			EndIndex:   2,
		},
		TypeID:        reindexTypeID,
		Value:         "detected",
		AnnotatorName: reindexAnnotator,
	}
	oldHash := oldItem.ComputeContentHash()
	if err := s.UpdateContentHash(context.Background(), annotationID, oldHash); err != nil {
		t.Fatalf("store old classifier annotation content hash: %v", err)
	}

	if err := reindexCustomEntries(s, []schema.SessionEntry{
		reindexContentEntry(0, "alpha"),
		reindexContentEntry(1, "replacement text"),
		reindexContentEntry(2, "charlie"),
	}); err != nil {
		t.Fatalf("re-index classifier target loss: %v", err)
	}
	assertAnchorState(t, s, annotationID, store.AnnotationTargetAnchorSuperseded)
	if got := entryTargetCount(t, s, annotationID); got != 0 {
		t.Fatalf("target rows after classifier target loss = %d, want 0", got)
	}

	var received schema.AnnotationPushRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/annotations/manifest" {
			_ = json.NewEncoder(w).Encode(schema.AnnotationManifestResponse{Hashes: []string{oldHash}})
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode annotation push request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(schema.AnnotationPushResponse{})
	}))
	defer server.Close()

	client := village.NewVillageClient(server.URL, "test-api-key", nil)
	summary, err := push.PushAnnotationsSelected(context.Background(), client, s, push.AnnotationSelection{}, false, 1)
	if err != nil {
		t.Fatalf("push after classifier target loss: %v", err)
	}
	if summary.Retracted != 1 {
		t.Fatalf("retracted count = %d, want 1", summary.Retracted)
	}
	if len(received.Retractions) != 1 || received.Retractions[0] != oldHash {
		t.Fatalf("wire retractions = %v, want [%s]", received.Retractions, oldHash)
	}
	if len(received.Annotations) != 0 {
		t.Fatalf("active annotations sent after classifier target loss = %d, want 0", len(received.Annotations))
	}
}

// --- helpers ---------------------------------------------------------------

func loadAnnotationRepairFixtures(t *testing.T) annotationRepairFixtures {
	t.Helper()
	data, err := annotationRepairFixturesFS.ReadFile("testdata/annotation-repair/target-anchors.yaml")
	if err != nil {
		t.Fatalf("read annotation repair fixtures: %v", err)
	}
	var fixtures annotationRepairFixtures
	if err := yaml.Unmarshal(data, &fixtures); err != nil {
		t.Fatalf("parse annotation repair fixtures: %v", err)
	}
	requiredNames := map[string]bool{
		"resolved-entry-id":               false,
		"resolved-tool-call-id":           false,
		"resolved-content-fingerprint":    false,
		"unresolved-human-ambiguous":      false,
		"unresolved-human-no-match":       false,
		"unresolved-human-missing-target": false,
		"superseded-classifier-ambiguous": false,
	}
	for _, tc := range fixtures.Cases {
		if _, ok := requiredNames[tc.Name]; ok {
			requiredNames[tc.Name] = true
		}
	}
	for name, seen := range requiredNames {
		if !seen {
			t.Fatalf("annotation repair fixture %q is required", name)
		}
	}
	return fixtures
}

func repairEntries(fixtures []annotationRepairEntry) []schema.SessionEntry {
	entries := make([]schema.SessionEntry, 0, len(fixtures))
	for _, fixture := range fixtures {
		entryType := schema.EntryTypeText
		if fixture.EntryType == "tool_use" {
			entryType = schema.EntryTypeToolUse
		}
		role := schema.RoleAssistant
		if fixture.Role == "user" {
			role = schema.RoleUser
		}
		entry := schema.SessionEntry{
			SessionID:      schema.SessionID(reindexSessionID),
			EntryIndex:     fixture.Index,
			Harness:        ingest.HarnessClaudeCode,
			EntryType:      entryType,
			Role:           role,
			ContentPreview: strPtr(fixture.Content),
		}
		if fixture.EntryID != "" {
			entry.EntryID = strPtr(fixture.EntryID)
		}
		if fixture.ToolCallID != "" {
			entry.ToolCallID = strPtr(fixture.ToolCallID)
		}
		if fixture.PartType != "" {
			entry.PartType = strPtr(fixture.PartType)
		}
		entries = append(entries, entry)
	}
	return entries
}

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
	return annotateReindexEntryWith(t, s, entryIndex, endIndex, reindexAnnotator, reindexTypeID)
}

func annotateReindexEntryWith(t *testing.T, s *store.Store, entryIndex, endIndex int, annotatorName, typeID string) string {
	t.Helper()
	ctx := context.Background()
	annotatorID, err := s.GetAnnotatorIDByName(ctx, annotatorName)
	if err != nil || annotatorID == "" {
		t.Fatalf("resolve annotator %q: id=%q err=%v", annotatorName, annotatorID, err)
	}
	annotationTypeID, err := s.GetAnnotationTypeID(ctx, typeID)
	if err != nil || annotationTypeID == "" {
		t.Fatalf("resolve annotation type %q: id=%q err=%v", typeID, annotationTypeID, err)
	}
	id, err := s.CreateEntryAnnotation(ctx, ingest.EntryAnnotationParams{
		SessionID:        reindexSessionID,
		EntryIndex:       entryIndex,
		EndIndex:         endIndex,
		AnnotatorID:      annotatorID,
		AnnotationTypeID: annotationTypeID,
		Value:            "detected",
	})
	if err != nil {
		t.Fatalf("CreateEntryAnnotation(%d,%d): %v", entryIndex, endIndex, err)
	}
	return id
}

func assertAnchorState(t *testing.T, s *store.Store, annotationID string, want store.AnnotationTargetAnchorState) {
	t.Helper()
	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)
	got := ""
	if err := sqlitex.ExecuteTransient(conn, `SELECT state FROM annotation_target_anchors WHERE annotation_id = ?`, &sqlitex.ExecOptions{
		Args: []any{annotationID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			got = stmt.ColumnText(0)
			return nil
		},
	}); err != nil {
		t.Fatalf("read annotation target anchor %s: %v", annotationID, err)
	}
	if got != string(want) {
		t.Fatalf("anchor state = %q, want %q", got, want)
	}
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
