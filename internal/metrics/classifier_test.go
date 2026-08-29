package metrics_test

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/metrics"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/annotation-run-state/combined-skip.yaml
var classifierCombinedSkipYAML []byte

// --- stubs for ClassifierAnnotator integration tests ---

// stubMetricsStore implements ingest.MetricsStore for testing.
// Only ListEntries and GetMetrics are implemented; all other methods
// delegate to the embedded nil interface and panic if called.
type stubMetricsStore struct {
	ingest.MetricsStore
	entries             []schema.SessionEntry
	m                   *ingest.SessionMetrics
	getMetricsCalls     int
	listEntriesCalls    int
	annotationState     *ingest.AnnotationRunState
	sessionEntriesHash  string
	saveAnnotationState *ingest.AnnotationRunState
	saveAnnotationCalls int
}

func (s *stubMetricsStore) ListEntries(_ context.Context, _ ingest.SessionID) ([]schema.SessionEntry, error) {
	s.listEntriesCalls++
	return s.entries, nil
}

func (s *stubMetricsStore) GetMetrics(_ context.Context, _ ingest.SessionID) (*ingest.SessionMetrics, error) {
	s.getMetricsCalls++
	return s.m, nil
}

func (s *stubMetricsStore) GetAnnotationRunInputs(_ context.Context, sessionID ingest.SessionID) (*ingest.AnnotationRunInputs, error) {
	inputs := &ingest.AnnotationRunInputs{
		SessionID:             sessionID,
		SessionEntriesHash:    s.sessionEntriesHash,
		HasSessionEntriesHash: s.sessionEntriesHash != "",
		State:                 s.annotationState,
	}
	if s.m != nil && s.m.ComputeVersion != nil {
		inputs.ComputeVersion = *s.m.ComputeVersion
		inputs.HasComputeVersion = true
	}
	return inputs, nil
}

func (s *stubMetricsStore) GetCurrentSessionEntriesHash(_ context.Context, _ ingest.SessionID) (string, bool, error) {
	return s.sessionEntriesHash, s.sessionEntriesHash != "", nil
}

func (s *stubMetricsStore) GetAnnotationRunState(_ context.Context, _ ingest.SessionID) (*ingest.AnnotationRunState, error) {
	return s.annotationState, nil
}

func (s *stubMetricsStore) SaveAnnotationRunState(_ context.Context, state ingest.AnnotationRunState) error {
	s.saveAnnotationCalls++
	s.saveAnnotationState = &state
	return nil
}

// stubAnnotationStore records calls for assertion.
type stubAnnotationStore struct {
	annotatorIDs   map[string]string // name → UUID returned
	typeIDs        map[string]string // typeID → UUID returned
	annotatorCalls []string
	typeCalls      []string
	created        []ingest.SessionAnnotationParams
	entryCreated   []ingest.EntryAnnotationParams

	// entryCreateErr, when non-nil, maps entry index → error to simulate partial failures.
	entryCreateErr map[int]error

	// Dedup support (R9): existing annotations keyed by (annotationTypeID + annotatorID + sessionID).
	existing map[string]*ingest.ExistingAnnotation
	// superseded records (oldID, newID) pairs for SupersedeAnnotation calls.
	superseded [][2]string
	// contentHashes records (annotationID, hash) pairs for UpdateContentHash calls.
	contentHashes [][2]string
}

type batchAnnotationStore struct {
	*stubAnnotationStore
	writes []ingest.ClassifierAnnotationWrite
}

func (s *batchAnnotationStore) ApplyClassifierAnnotations(_ context.Context, writes []ingest.ClassifierAnnotationWrite) []ingest.ClassifierAnnotationWriteResult {
	s.writes = append(s.writes, writes...)
	results := make([]ingest.ClassifierAnnotationWriteResult, len(writes))
	for i := range writes {
		results[i] = ingest.ClassifierAnnotationWriteResult{
			Dedup:        ingest.DedupCreate,
			AnnotationID: fmt.Sprintf("batch-ann-uuid-%d", i+1),
		}
	}
	return results
}

func (s *batchAnnotationStore) ApplyClassifierAnnotationsWithProfile(ctx context.Context, writes []ingest.ClassifierAnnotationWrite, stats *ingest.AnnotationProfileStats) []ingest.ClassifierAnnotationWriteResult {
	results := s.ApplyClassifierAnnotations(ctx, writes)
	if stats != nil {
		stats.BatchDedupLookupCount += len(writes)
		stats.BatchInsertParentCount += len(writes)
		stats.BatchInsertTargetCount += len(writes)
		stats.BatchUpdateHashCount += len(writes)
	}
	return results
}

func (s *stubAnnotationStore) GetAnnotatorIDByName(_ context.Context, name string) (string, error) {
	s.annotatorCalls = append(s.annotatorCalls, name)
	return s.annotatorIDs[name], nil
}

func (s *stubAnnotationStore) GetAnnotationTypeID(_ context.Context, typeID string) (string, error) {
	s.typeCalls = append(s.typeCalls, typeID)
	return s.typeIDs[typeID], nil
}

func (s *stubAnnotationStore) CreateSessionAnnotation(_ context.Context, p ingest.SessionAnnotationParams) (string, error) {
	s.created = append(s.created, p)
	return fmt.Sprintf("ann-uuid-%d", len(s.created)), nil
}

func (s *stubAnnotationStore) CreateEntryAnnotation(_ context.Context, p ingest.EntryAnnotationParams) (string, error) {
	if s.entryCreateErr != nil {
		if err, ok := s.entryCreateErr[p.EntryIndex]; ok {
			return "", err
		}
	}
	s.entryCreated = append(s.entryCreated, p)
	return fmt.Sprintf("entry-ann-uuid-%d", len(s.entryCreated)), nil
}

func (s *stubAnnotationStore) FindExistingAnnotation(_ context.Context, p ingest.FindAnnotationParams) (*ingest.ExistingAnnotation, error) {
	if s.existing == nil {
		return nil, nil
	}
	key := p.AnnotationTypeID + "|" + p.AnnotatorID
	if p.SessionID != nil {
		key += "|" + *p.SessionID
	}
	if p.EntryIndex != nil {
		key += fmt.Sprintf("|%d", *p.EntryIndex)
	}
	return s.existing[key], nil
}

func (s *stubAnnotationStore) SupersedeAnnotation(_ context.Context, oldID, newID string) error {
	s.superseded = append(s.superseded, [2]string{oldID, newID})
	return nil
}

func (s *stubAnnotationStore) UpdateContentHash(_ context.Context, annotationID, contentHash string) error {
	s.contentHashes = append(s.contentHashes, [2]string{annotationID, contentHash})
	return nil
}

func (s *stubAnnotationStore) CreateAnnotationAndSupersede(_ context.Context, _ ingest.CreateAnnotationParams, _, _ string) (string, error) {
	// stubAnnotationStore does not exercise CreateAnnotationAndSupersede in classifier tests.
	return "", nil
}

// findResult scans ClassifierEngine results for the given typeID.
// Returns nil if not found.
func findResult(results []*metrics.ClassifierResult, typeID string) *metrics.ClassifierResult {
	for _, r := range results {
		if r.TypeID == typeID {
			return r
		}
	}
	return nil
}

// buildEntry creates a session entry with ContentPreview set.
func buildEntry(sid ingest.SessionID, idx int, role ingest.Role, preview string) schema.SessionEntry {
	return schema.SessionEntry{
		SessionID:      sid,
		EntryIndex:     idx,
		Role:           role,
		EntryType:      ingest.EntryTypeText,
		ContentPreview: &preview,
		Depth:          0,
	}
}

// buildMetrics creates a SessionMetrics with the given tool calls and turn count.
func buildMetrics(sid ingest.SessionID, toolCalls, turnCount int) *ingest.SessionMetrics {
	return &ingest.SessionMetrics{
		SessionID: sid,
		QualityMetrics: schema.QualityMetrics{
			ToolCalls: &toolCalls,
			TurnCount: &turnCount,
		},
	}
}

func buildVersionedMetrics(sid ingest.SessionID, computeVersion int) *ingest.SessionMetrics {
	m := buildMetrics(sid, 3, 4)
	m.ComputeVersion = &computeVersion
	return m
}

// --- ClassifierAnnotator integration tests ---

// TestClassifierAnnotator_Annotate_FrustrationDetected verifies the DB write path
// when a frustration signal is present. Checks annotator name lookup, type ID
// lookup, and CreateSessionAnnotation call with correct params.
func TestClassifierAnnotator_Annotate_FrustrationDetected(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)
	preview := "fuck this bug"
	ms := &stubMetricsStore{
		entries: []schema.SessionEntry{buildEntry(sid, 0, ingest.RoleUser, preview)},
		m:       buildMetrics(sid, 5, 4),
	}
	as := &stubAnnotationStore{
		annotatorIDs: map[string]string{
			"frustration-classifier": "anntr-uuid-2",
			"outcome-classifier":     "anntr-uuid-1",
		},
		typeIDs: map[string]string{
			testutil.TestTypeIDUserFrustration: "type-uuid-20",
			testutil.TestTypeIDSessionOutcome:  "type-uuid-10",
		},
	}

	ca := metrics.NewClassifierAnnotator(ms, as)
	err := ca.Annotate(context.Background(), sid)
	if err != nil {
		t.Fatalf("Annotate returned error: %v", err)
	}

	// Verify GetAnnotatorIDByName was called with the frustration annotator name.
	foundFrustrationAnnotator := false
	for _, name := range as.annotatorCalls {
		if name == "frustration-classifier" {
			foundFrustrationAnnotator = true
			break
		}
	}
	if !foundFrustrationAnnotator {
		t.Errorf("GetAnnotatorIDByName never called with %q; calls: %v", "frustration-classifier", as.annotatorCalls)
	}

	// Verify GetAnnotationTypeID was called with the frustration type ID.
	foundFrustrationType := false
	for _, tid := range as.typeCalls {
		if tid == testutil.TestTypeIDUserFrustration {
			foundFrustrationType = true
			break
		}
	}
	if !foundFrustrationType {
		t.Errorf("GetAnnotationTypeID never called with %q; calls: %v", testutil.TestTypeIDUserFrustration, as.typeCalls)
	}

	// Verify CreateSessionAnnotation was called with correct frustration params.
	foundFrustrationCreate := false
	for _, p := range as.created {
		if p.AnnotatorID == "anntr-uuid-2" && p.AnnotationTypeID == "type-uuid-20" && p.Value == "detected" {
			foundFrustrationCreate = true
			break
		}
	}
	if !foundFrustrationCreate {
		t.Errorf("CreateSessionAnnotation not called with frustration params; created: %+v", as.created)
	}
}

// TestClassifierAnnotator_Annotate_AnnotatorNotFound verifies that when the annotator
// is not in the DB (ID=0), no CreateSessionAnnotation call is made.
func TestClassifierAnnotator_Annotate_AnnotatorNotFound(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)
	ms := &stubMetricsStore{
		entries: []schema.SessionEntry{buildEntry(sid, 0, ingest.RoleUser, "fuck this")},
		m:       buildMetrics(sid, 1, 2),
	}
	// All annotator IDs return "" (not seeded).
	as := &stubAnnotationStore{
		annotatorIDs: map[string]string{},
		typeIDs:      map[string]string{testutil.TestTypeIDUserFrustration: "type-uuid-20"},
	}

	ca := metrics.NewClassifierAnnotator(ms, as)
	if err := ca.Annotate(context.Background(), sid); err != nil {
		t.Fatalf("Annotate returned error: %v", err)
	}
	if len(as.created) != 0 {
		t.Errorf("expected no CreateSessionAnnotation calls when annotator not found; got %d", len(as.created))
	}
}

// TestClassifierAnnotator_Annotate_TypeNotFound verifies that when the annotation
// type is not in the DB (ID=0), no CreateSessionAnnotation call is made.
func TestClassifierAnnotator_Annotate_TypeNotFound(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)
	ms := &stubMetricsStore{
		entries: []schema.SessionEntry{buildEntry(sid, 0, ingest.RoleUser, "fuck this")},
		m:       buildMetrics(sid, 1, 2),
	}
	// Annotator found but type not seeded.
	as := &stubAnnotationStore{
		annotatorIDs: map[string]string{"frustration-classifier": "anntr-uuid-2"},
		typeIDs:      map[string]string{}, // type not found
	}

	ca := metrics.NewClassifierAnnotator(ms, as)
	if err := ca.Annotate(context.Background(), sid); err != nil {
		t.Fatalf("Annotate returned error: %v", err)
	}
	if len(as.created) != 0 {
		t.Errorf("expected no CreateSessionAnnotation calls when type not found; got %d", len(as.created))
	}
}

// --- classifyFrustration (tested via ClassifierEngine.Run) ---

func TestClassifyFrustration_Detected(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)
	engine := metrics.NewClassifierEngine()

	entries := []schema.SessionEntry{
		buildEntry(sid, 0, ingest.RoleUser, "fuck these errors"),
		buildEntry(sid, 1, ingest.RoleAssistant, "I will fix it"),
	}
	results := engine.Run(context.Background(), sid, entries, nil)

	r := findResult(results, testutil.TestTypeIDUserFrustration)
	if r == nil {
		t.Fatal("expected quality.user_frustration result, got nil")
	}
	if r.Value != "detected" {
		t.Errorf("Value = %q, want %q", r.Value, "detected")
	}
	if r.Confidence <= 0 {
		t.Errorf("Confidence = %v, want > 0", r.Confidence)
	}
}

func TestClassifyFrustration_CaseInsensitive(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)
	engine := metrics.NewClassifierEngine()

	entries := []schema.SessionEntry{
		buildEntry(sid, 0, ingest.RoleUser, "FUCK THE BUILD"),
	}
	results := engine.Run(context.Background(), sid, entries, nil)

	r := findResult(results, testutil.TestTypeIDUserFrustration)
	if r == nil {
		t.Fatal("expected quality.user_frustration result, got nil")
	}
	if r.Value != "detected" {
		t.Errorf("Value = %q, want %q (case-insensitive match)", r.Value, "detected")
	}
}

func TestClassifyFrustration_NotDetected(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)
	engine := metrics.NewClassifierEngine()

	entries := []schema.SessionEntry{
		buildEntry(sid, 0, ingest.RoleUser, "please fix the routing issue"),
		buildEntry(sid, 1, ingest.RoleAssistant, "I found the bug"),
	}
	results := engine.Run(context.Background(), sid, entries, nil)

	r := findResult(results, testutil.TestTypeIDUserFrustration)
	if r == nil {
		t.Fatal("expected quality.user_frustration result, got nil")
	}
	if r.Value != "not_detected" {
		t.Errorf("Value = %q, want %q", r.Value, "not_detected")
	}
}

func TestClassifyFrustration_NoEntries(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)
	engine := metrics.NewClassifierEngine()

	results := engine.Run(context.Background(), sid, nil, nil)

	r := findResult(results, testutil.TestTypeIDUserFrustration)
	if r != nil {
		t.Errorf("expected no quality.user_frustration result for empty entries, got %+v", r)
	}
}

func TestClassifyFrustration_NilContentPreview(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)
	engine := metrics.NewClassifierEngine()

	// Entry with nil ContentPreview — classifier should skip it.
	entries := []schema.SessionEntry{
		{
			SessionID:  sid,
			EntryIndex: 0,
			Role:       ingest.RoleUser,
			EntryType:  ingest.EntryTypeText,
			Depth:      0,
			// ContentPreview intentionally nil
		},
	}
	results := engine.Run(context.Background(), sid, entries, nil)

	r := findResult(results, testutil.TestTypeIDUserFrustration)
	if r == nil {
		t.Fatal("expected quality.user_frustration result (entries exist but no preview), got nil")
	}
	// With no scannable content, should return not_detected.
	if r.Value != "not_detected" {
		t.Errorf("Value = %q, want %q", r.Value, "not_detected")
	}
}

// --- classifyOutcome (tested via ClassifierEngine.Run) ---

func TestClassifyOutcome_NilMetrics(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)
	engine := metrics.NewClassifierEngine()

	entries := []schema.SessionEntry{
		buildEntry(sid, 0, ingest.RoleUser, "hello"),
	}
	// nil metrics → classifyOutcome returns nil
	results := engine.Run(context.Background(), sid, entries, nil)

	r := findResult(results, testutil.TestTypeIDSessionOutcome)
	if r != nil {
		t.Errorf("expected no quality.session_outcome result for nil metrics, got %+v", r)
	}
}

func TestClassifyOutcome_Abandoned_ZeroToolsOneTurn(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)
	engine := metrics.NewClassifierEngine()

	entries := []schema.SessionEntry{
		buildEntry(sid, 0, ingest.RoleUser, "quick question"),
	}
	m := buildMetrics(sid, 0, 1) // toolCalls=0, turnCount=1
	results := engine.Run(context.Background(), sid, entries, m)

	r := findResult(results, testutil.TestTypeIDSessionOutcome)
	if r == nil {
		t.Fatal("expected quality.session_outcome result, got nil")
	}
	if r.Value != "abandoned" {
		t.Errorf("Value = %q, want %q", r.Value, "abandoned")
	}
}

func TestClassifyOutcome_Abandoned_ZeroToolsZeroTurns(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)
	engine := metrics.NewClassifierEngine()

	m := buildMetrics(sid, 0, 0) // toolCalls=0, turnCount=0 (≤ 1)
	results := engine.Run(context.Background(), sid, nil, m)

	r := findResult(results, testutil.TestTypeIDSessionOutcome)
	if r == nil {
		t.Fatal("expected quality.session_outcome result, got nil")
	}
	if r.Value != "abandoned" {
		t.Errorf("Value = %q, want %q", r.Value, "abandoned")
	}
}

func TestClassifyOutcome_NotAbandoned_WithToolCalls(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)
	engine := metrics.NewClassifierEngine()

	// Session with tool calls and multiple turns — not abandoned.
	// computeOutcome will classify based on error patterns.
	entries := []schema.SessionEntry{
		buildEntry(sid, 0, ingest.RoleUser, "fix the bug"),
		buildEntry(sid, 1, ingest.RoleAssistant, "done"),
	}
	m := buildMetrics(sid, 3, 4) // 3 tool calls, 4 turns
	results := engine.Run(context.Background(), sid, entries, m)

	r := findResult(results, testutil.TestTypeIDSessionOutcome)
	// With 2 entries and no errors, computeOutcome should return resolved.
	if r == nil {
		t.Fatal("expected quality.session_outcome result, got nil")
	}
	validValues := map[string]bool{"resolved": true, "partial": true, "failed": true}
	if !validValues[r.Value] {
		t.Errorf("Value = %q, want one of {resolved, partial, failed}", r.Value)
	}
	if r.Value == "abandoned" {
		t.Errorf("Value = %q, should not be abandoned (toolCalls=3 > 0)", r.Value)
	}
}

func TestClassifyOutcome_Resolved(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)
	engine := metrics.NewClassifierEngine()

	// Session with tool calls + no errors → should resolve to "resolved".
	entries := []schema.SessionEntry{
		{SessionID: sid, EntryIndex: 0, Role: ingest.RoleUser, EntryType: ingest.EntryTypeText, Depth: 0, IsError: false},
		{SessionID: sid, EntryIndex: 1, Role: ingest.RoleAssistant, EntryType: ingest.EntryTypeText, Depth: 0, IsError: false},
		{SessionID: sid, EntryIndex: 2, Role: ingest.RoleUser, EntryType: ingest.EntryTypeText, Depth: 0, IsError: false},
		{SessionID: sid, EntryIndex: 3, Role: ingest.RoleAssistant, EntryType: ingest.EntryTypeText, Depth: 0, IsError: false},
	}
	m := buildMetrics(sid, 2, 4)
	results := engine.Run(context.Background(), sid, entries, m)

	r := findResult(results, testutil.TestTypeIDSessionOutcome)
	if r == nil {
		t.Fatal("expected quality.session_outcome result, got nil")
	}
	if r.Value != "resolved" {
		t.Errorf("Value = %q, want %q (no errors → resolved)", r.Value, "resolved")
	}
}

// --- classifyScope (tested via ClassifierEngine.Run) ---

// buildToolUseEntry creates a depth=1 tool_use entry with ToolInput JSON.
func buildToolUseEntry(sid ingest.SessionID, idx int, toolInput string) schema.SessionEntry {
	toolName := "Write"
	return schema.SessionEntry{
		SessionID:    sid,
		EntryIndex:   idx,
		Role:         ingest.RoleAssistant,
		EntryType:    ingest.EntryTypeToolUse,
		HasToolUse:   true,
		ToolNamesCSV: &toolName,
		ToolInput:    &toolInput,
		Depth:        1,
		ParentIndex:  intPtr(0),
	}
}

// TestClassifyScope_NoToolUseEntries verifies that entries without tool_use produce
// no scope result (insufficient file path data).
func TestClassifyScope_NoToolUseEntries(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)
	engine := metrics.NewClassifierEngine()

	entries := []schema.SessionEntry{
		buildEntry(sid, 0, ingest.RoleUser, "fix the routing issue"),
		buildEntry(sid, 1, ingest.RoleAssistant, "I found the bug"),
	}
	m := buildMetrics(sid, 2, 4)
	results := engine.Run(context.Background(), sid, entries, m)

	r := findResult(results, testutil.TestTypeIDSessionScope)
	if r != nil {
		t.Errorf("expected nil metadata.session_scope result (no tool_use entries), got %+v", r)
	}
}

// TestClassifyScope_NoEntries verifies nil input returns no result.
func TestClassifyScope_NoEntries(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)
	engine := metrics.NewClassifierEngine()

	results := engine.Run(context.Background(), sid, nil, nil)

	r := findResult(results, testutil.TestTypeIDSessionScope)
	if r != nil {
		t.Errorf("expected nil metadata.session_scope result for empty entries, got %+v", r)
	}
}

// TestClassifyScope_SingleFilePath verifies that a single file path yields its directory as scope.
func TestClassifyScope_SingleFilePath(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)
	engine := metrics.NewClassifierEngine()

	entries := []schema.SessionEntry{
		buildEntry(sid, 0, ingest.RoleAssistant, "I'll edit the file"),
		buildToolUseEntry(sid, 1, `{"file_path": "/home/user/project/src/main.go", "content": "package main"}`),
	}
	results := engine.Run(context.Background(), sid, entries, nil)

	r := findResult(results, testutil.TestTypeIDSessionScope)
	if r == nil {
		t.Fatal("expected metadata.session_scope result, got nil")
	}
	if r.Value != "/home/user/project/src" {
		t.Errorf("Value = %q, want %q", r.Value, "/home/user/project/src")
	}
	if r.Confidence <= 0 {
		t.Errorf("Confidence = %v, want > 0", r.Confidence)
	}
	if r.Provenance == nil {
		t.Fatal("expected Provenance, got nil")
	}
	if r.Provenance.Method != "heuristic" {
		t.Errorf("Provenance.Method = %q, want %q", r.Provenance.Method, "heuristic")
	}
}

// TestClassifyScope_CommonPrefix verifies that multiple files in the same project
// produce the common directory prefix as scope.
func TestClassifyScope_CommonPrefix(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)
	engine := metrics.NewClassifierEngine()

	entries := []schema.SessionEntry{
		buildEntry(sid, 0, ingest.RoleAssistant, "editing files"),
		buildToolUseEntry(sid, 1, `{"file_path": "/home/user/project/src/main.go", "content": "a"}`),
		buildToolUseEntry(sid, 2, `{"file_path": "/home/user/project/src/util.go", "content": "b"}`),
		buildToolUseEntry(sid, 3, `{"file_path": "/home/user/project/test/main_test.go", "content": "c"}`),
	}
	results := engine.Run(context.Background(), sid, entries, nil)

	r := findResult(results, testutil.TestTypeIDSessionScope)
	if r == nil {
		t.Fatal("expected metadata.session_scope result, got nil")
	}
	// Common prefix: /home/user/project
	if r.Value != "/home/user/project" {
		t.Errorf("Value = %q, want %q", r.Value, "/home/user/project")
	}
}

// TestClassifyScope_NotebookPath verifies that notebook_path is also extracted.
func TestClassifyScope_NotebookPath(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)
	engine := metrics.NewClassifierEngine()

	entries := []schema.SessionEntry{
		buildEntry(sid, 0, ingest.RoleAssistant, "editing notebook"),
		buildToolUseEntry(sid, 1, `{"notebook_path": "/home/user/analysis/data.ipynb", "new_source": "import pandas"}`),
	}
	results := engine.Run(context.Background(), sid, entries, nil)

	r := findResult(results, testutil.TestTypeIDSessionScope)
	if r == nil {
		t.Fatal("expected metadata.session_scope result, got nil")
	}
	if r.Value != "/home/user/analysis" {
		t.Errorf("Value = %q, want %q", r.Value, "/home/user/analysis")
	}
}

// TestClassifyScope_RelativePaths verifies relative paths produce a scope.
func TestClassifyScope_RelativePaths(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)
	engine := metrics.NewClassifierEngine()

	entries := []schema.SessionEntry{
		buildEntry(sid, 0, ingest.RoleAssistant, "editing"),
		buildToolUseEntry(sid, 1, `{"file_path": "src/main.go", "content": "a"}`),
		buildToolUseEntry(sid, 2, `{"file_path": "src/util.go", "content": "b"}`),
	}
	results := engine.Run(context.Background(), sid, entries, nil)

	r := findResult(results, testutil.TestTypeIDSessionScope)
	if r == nil {
		t.Fatal("expected metadata.session_scope result, got nil")
	}
	if r.Value != "src" {
		t.Errorf("Value = %q, want %q", r.Value, "src")
	}
}

// TestClassifyScope_DisjointPaths_FallsBackToMostFrequent verifies that disjoint
// paths with no common prefix fall back to the most frequent directory.
func TestClassifyScope_DisjointPaths_FallsBackToMostFrequent(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)
	engine := metrics.NewClassifierEngine()

	entries := []schema.SessionEntry{
		buildEntry(sid, 0, ingest.RoleAssistant, "editing"),
		buildToolUseEntry(sid, 1, `{"file_path": "/home/user/project-a/main.go", "content": "a"}`),
		buildToolUseEntry(sid, 2, `{"file_path": "/opt/other/util.go", "content": "b"}`),
		buildToolUseEntry(sid, 3, `{"file_path": "/opt/other/test.go", "content": "c"}`),
	}
	results := engine.Run(context.Background(), sid, entries, nil)

	r := findResult(results, testutil.TestTypeIDSessionScope)
	if r == nil {
		t.Fatal("expected metadata.session_scope result, got nil")
	}
	// /opt/other has 2 files, /home/user/project-a has 1 → most frequent wins.
	if r.Value != "/opt/other" {
		t.Errorf("Value = %q, want %q", r.Value, "/opt/other")
	}
	if !strings.Contains(r.Reason, "most frequent directory") {
		t.Errorf("Reason = %q, want it to mention 'most frequent directory'", r.Reason)
	}
}

// TestClassifyScope_NoFilePaths verifies that tool_use entries without file_path or
// notebook_path yield no scope result.
func TestClassifyScope_NoFilePaths(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)
	engine := metrics.NewClassifierEngine()

	entries := []schema.SessionEntry{
		buildEntry(sid, 0, ingest.RoleAssistant, "running command"),
		buildToolUseEntry(sid, 1, `{"command": "go test ./..."}`),
	}
	results := engine.Run(context.Background(), sid, entries, nil)

	r := findResult(results, testutil.TestTypeIDSessionScope)
	if r != nil {
		t.Errorf("expected nil metadata.session_scope (no file paths), got %+v", r)
	}
}

// TestClassifyScope_MalformedJSON verifies that malformed ToolInput is skipped gracefully.
func TestClassifyScope_MalformedJSON(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)
	engine := metrics.NewClassifierEngine()

	entries := []schema.SessionEntry{
		buildEntry(sid, 0, ingest.RoleAssistant, "editing"),
		buildToolUseEntry(sid, 1, `{invalid json`),
		buildToolUseEntry(sid, 2, `{"file_path": "/home/user/project/main.go", "content": "ok"}`),
	}
	results := engine.Run(context.Background(), sid, entries, nil)

	r := findResult(results, testutil.TestTypeIDSessionScope)
	if r == nil {
		t.Fatal("expected metadata.session_scope result (one valid path), got nil")
	}
	if r.Value != "/home/user/project" {
		t.Errorf("Value = %q, want %q", r.Value, "/home/user/project")
	}
}

// --- findResults helper for entry classifiers that produce multiple results ---

// findResults returns all ClassifierResults matching the given typeID.
func findResults(results []*metrics.ClassifierResult, typeID string) []*metrics.ClassifierResult {
	var found []*metrics.ClassifierResult
	for _, r := range results {
		if r.TypeID == typeID {
			found = append(found, r)
		}
	}
	return found
}

// newFullAnnotationStore returns a stubAnnotationStore pre-populated with all
// session-level and entry-level annotator/type IDs needed by the classifier engine.
func newFullAnnotationStore() *stubAnnotationStore {
	return &stubAnnotationStore{
		annotatorIDs: map[string]string{
			"outcome-classifier":             "anntr-uuid-1",
			"frustration-classifier":         "anntr-uuid-2",
			"scope-classifier":               "anntr-uuid-3",
			"frustration-signal-classifier":  "anntr-uuid-4",
			"resolution-evidence-classifier": "anntr-uuid-5",
		},
		typeIDs: map[string]string{
			testutil.TestTypeIDSessionOutcome:     "type-uuid-10",
			testutil.TestTypeIDUserFrustration:    "type-uuid-20",
			testutil.TestTypeIDSessionScope:       "type-uuid-30",
			testutil.TestTypeIDFrustrationSignal:  "type-uuid-40",
			testutil.TestTypeIDResolutionEvidence: "type-uuid-50",
		},
	}
}

func TestClassifierAnnotator_Annotate_CachesSeededIDs(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)
	ms := &stubMetricsStore{
		entries: []schema.SessionEntry{
			buildEntry(sid, 0, ingest.RoleUser, "please fix this fuck bug"),
			buildEntry(sid, 1, ingest.RoleAssistant, "done"),
		},
		m: buildMetrics(sid, 3, 4),
	}
	as := newFullAnnotationStore()
	ca := metrics.NewClassifierAnnotator(ms, as)

	if err := ca.Annotate(context.Background(), sid); err != nil {
		t.Fatalf("first Annotate: %v", err)
	}
	if err := ca.Annotate(context.Background(), sid); err != nil {
		t.Fatalf("second Annotate: %v", err)
	}

	assertLookupOnce := func(kind, needle string, values []string) {
		t.Helper()
		var got int
		for _, value := range values {
			if value == needle {
				got++
			}
		}
		if got != 1 {
			t.Errorf("expected %s lookup for %q once, got %d calls in %v", kind, needle, got, values)
		}
	}

	if got := len(as.annotatorCalls); got != 4 {
		t.Errorf("expected exactly 4 annotator lookups across two runs, got %d: %v", got, as.annotatorCalls)
	}
	if got := len(as.typeCalls); got != 4 {
		t.Errorf("expected exactly 4 annotation type lookups across two runs, got %d: %v", got, as.typeCalls)
	}
	assertLookupOnce("annotator", "outcome-classifier", as.annotatorCalls)
	assertLookupOnce("annotator", "frustration-classifier", as.annotatorCalls)
	assertLookupOnce("annotator", "frustration-signal-classifier", as.annotatorCalls)
	assertLookupOnce("annotator", "resolution-evidence-classifier", as.annotatorCalls)
	assertLookupOnce("annotation type", testutil.TestTypeIDSessionOutcome, as.typeCalls)
	assertLookupOnce("annotation type", testutil.TestTypeIDUserFrustration, as.typeCalls)
	assertLookupOnce("annotation type", testutil.TestTypeIDFrustrationSignal, as.typeCalls)
	assertLookupOnce("annotation type", testutil.TestTypeIDResolutionEvidence, as.typeCalls)
}

func TestClassifierAnnotator_Annotate_NoStateRunsAndSavesState(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)
	ms := &stubMetricsStore{
		entries:            []schema.SessionEntry{buildEntry(sid, 0, ingest.RoleUser, "please fix this bug")},
		m:                  buildVersionedMetrics(sid, 7),
		sessionEntriesHash: strings.Repeat("a", 64),
	}
	as := newFullAnnotationStore()
	ca := metrics.NewClassifierAnnotator(ms, as)

	if err := ca.Annotate(context.Background(), sid); err != nil {
		t.Fatalf("Annotate: %v", err)
	}

	if ms.listEntriesCalls != 1 {
		t.Fatalf("ListEntries calls = %d, want 1", ms.listEntriesCalls)
	}
	if ms.saveAnnotationCalls != 1 {
		t.Fatalf("SaveAnnotationRunState calls = %d, want 1", ms.saveAnnotationCalls)
	}
	if ms.saveAnnotationState == nil {
		t.Fatal("SaveAnnotationRunState did not receive state")
	}
	if ms.saveAnnotationState.SessionEntriesHash != strings.Repeat("a", 64) {
		t.Errorf("saved hash = %q, want current hash", ms.saveAnnotationState.SessionEntriesHash)
	}
	if ms.saveAnnotationState.ComputeVersion != 7 {
		t.Errorf("saved compute version = %d, want 7", ms.saveAnnotationState.ComputeVersion)
	}
	if ms.saveAnnotationState.ClassifierVersion != metrics.CurrentClassifierAnnotationVersion {
		t.Errorf("saved classifier version = %d, want %d", ms.saveAnnotationState.ClassifierVersion, metrics.CurrentClassifierAnnotationVersion)
	}
}

func TestClassifierAnnotator_Annotate_MatchingStateSkipsWork(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)
	hash := strings.Repeat("b", 64)
	ms := &stubMetricsStore{
		entries:            []schema.SessionEntry{buildEntry(sid, 0, ingest.RoleUser, "fuck this bug")},
		m:                  buildVersionedMetrics(sid, 8),
		sessionEntriesHash: hash,
		annotationState: &ingest.AnnotationRunState{
			SessionID:          sid,
			SessionEntriesHash: hash,
			ComputeVersion:     8,
			ClassifierVersion:  metrics.CurrentClassifierAnnotationVersion,
			AnnotatedAt:        time.UnixMilli(1700000000000),
		},
	}
	as := newFullAnnotationStore()
	ca := metrics.NewClassifierAnnotator(ms, as)

	if err := ca.Annotate(context.Background(), sid); err != nil {
		t.Fatalf("Annotate: %v", err)
	}

	if ms.listEntriesCalls != 0 {
		t.Fatalf("ListEntries calls = %d, want 0", ms.listEntriesCalls)
	}
	if ms.getMetricsCalls != 0 {
		t.Fatalf("GetMetrics calls = %d, want 0", ms.getMetricsCalls)
	}
	if len(as.created) != 0 || len(as.entryCreated) != 0 || len(as.annotatorCalls) != 0 || len(as.typeCalls) != 0 {
		t.Fatalf("annotation work ran despite matching state: created=%d entryCreated=%d annotatorCalls=%d typeCalls=%d",
			len(as.created), len(as.entryCreated), len(as.annotatorCalls), len(as.typeCalls))
	}
	if ms.saveAnnotationCalls != 0 {
		t.Fatalf("SaveAnnotationRunState calls = %d, want 0", ms.saveAnnotationCalls)
	}
}

type classifierCombinedSkipFixture struct {
	Cases []classifierCombinedSkipCase `yaml:"cases"`
}

type classifierCombinedSkipCase struct {
	Name                   string `yaml:"name"`
	CurrentHash            string `yaml:"current_hash"`
	HasMetrics             bool   `yaml:"has_metrics"`
	HasComputeVersion      bool   `yaml:"has_compute_version"`
	MetricComputeVersion   int    `yaml:"metric_compute_version"`
	StateHash              string `yaml:"state_hash"`
	StateComputeVersion    int    `yaml:"state_compute_version"`
	StateClassifierVersion int    `yaml:"state_classifier_version"`
	HasState               bool   `yaml:"has_state"`
	WantListEntriesCalls   int    `yaml:"want_list_entries_calls"`
	WantGetMetricsCalls    int    `yaml:"want_get_metrics_calls"`
	WantSaveStateCalls     int    `yaml:"want_save_state_calls"`
}

func TestClassifierAnnotator_Annotate_CombinedLookupDecisions(t *testing.T) {
	t.Parallel()
	var fixture classifierCombinedSkipFixture
	if err := yaml.Unmarshal(classifierCombinedSkipYAML, &fixture); err != nil {
		t.Fatalf("unmarshal combined skip fixture: %v", err)
	}
	assertRequiredClassifierCombinedSkipCases(t, fixture.Cases)
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			sid := mustSessionID(t, testutil.TestSessionUUID)
			var sessionMetrics *ingest.SessionMetrics
			if tc.HasMetrics {
				if tc.HasComputeVersion {
					sessionMetrics = buildVersionedMetrics(sid, tc.MetricComputeVersion)
				} else {
					sessionMetrics = buildMetrics(sid, 3, 4)
				}
			}
			ms := &stubMetricsStore{
				entries:            []schema.SessionEntry{buildEntry(sid, 0, ingest.RoleUser, "please fix this bug")},
				m:                  sessionMetrics,
				sessionEntriesHash: tc.CurrentHash,
			}
			if tc.HasState {
				ms.annotationState = &ingest.AnnotationRunState{
					SessionID:          sid,
					SessionEntriesHash: tc.StateHash,
					ComputeVersion:     tc.StateComputeVersion,
					ClassifierVersion:  tc.StateClassifierVersion,
					AnnotatedAt:        time.UnixMilli(1700000000000),
				}
			}
			as := newFullAnnotationStore()
			ca := metrics.NewClassifierAnnotator(ms, as)

			if err := ca.Annotate(context.Background(), sid); err != nil {
				t.Fatalf("Annotate: %v", err)
			}
			if ms.listEntriesCalls != tc.WantListEntriesCalls {
				t.Fatalf("ListEntries calls = %d, want %d", ms.listEntriesCalls, tc.WantListEntriesCalls)
			}
			if ms.getMetricsCalls != tc.WantGetMetricsCalls {
				t.Fatalf("GetMetrics calls = %d, want %d", ms.getMetricsCalls, tc.WantGetMetricsCalls)
			}
			if ms.saveAnnotationCalls != tc.WantSaveStateCalls {
				t.Fatalf("SaveAnnotationRunState calls = %d, want %d", ms.saveAnnotationCalls, tc.WantSaveStateCalls)
			}
		})
	}
}

func assertRequiredClassifierCombinedSkipCases(t *testing.T, cases []classifierCombinedSkipCase) {
	t.Helper()
	requiredNames := map[string]bool{
		"current state skips without metrics read":            false,
		"stale hash reads metrics and recomputes":             false,
		"stale metric version reads metrics and recomputes":   false,
		"missing state reads metrics and recomputes":          false,
		"missing metrics recomputes without saving state":     false,
		"nil compute version recomputes without saving state": false,
	}
	for _, tc := range cases {
		if _, ok := requiredNames[tc.Name]; ok {
			requiredNames[tc.Name] = true
		}
	}
	for name, seen := range requiredNames {
		if !seen {
			t.Fatalf("required combined skip fixture case %q is missing", name)
		}
	}
}

func TestClassifierAnnotator_Annotate_StaleClassifierVersionRuns(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)
	hash := strings.Repeat("c", 64)
	ms := &stubMetricsStore{
		entries:            []schema.SessionEntry{buildEntry(sid, 0, ingest.RoleUser, "please fix this bug")},
		m:                  buildVersionedMetrics(sid, 8),
		sessionEntriesHash: hash,
		annotationState: &ingest.AnnotationRunState{
			SessionID:          sid,
			SessionEntriesHash: hash,
			ComputeVersion:     8,
			ClassifierVersion:  metrics.CurrentClassifierAnnotationVersion - 1,
			AnnotatedAt:        time.UnixMilli(1700000000000),
		},
	}
	as := newFullAnnotationStore()
	ca := metrics.NewClassifierAnnotator(ms, as)

	if err := ca.Annotate(context.Background(), sid); err != nil {
		t.Fatalf("Annotate: %v", err)
	}
	if ms.listEntriesCalls != 1 {
		t.Fatalf("ListEntries calls = %d, want 1", ms.listEntriesCalls)
	}
	if ms.saveAnnotationCalls != 1 {
		t.Fatalf("SaveAnnotationRunState calls = %d, want 1", ms.saveAnnotationCalls)
	}
}

func TestClassifierAnnotator_Annotate_StaleComputeVersionRuns(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)
	hash := strings.Repeat("d", 64)
	ms := &stubMetricsStore{
		entries:            []schema.SessionEntry{buildEntry(sid, 0, ingest.RoleUser, "please fix this bug")},
		m:                  buildVersionedMetrics(sid, 9),
		sessionEntriesHash: hash,
		annotationState: &ingest.AnnotationRunState{
			SessionID:          sid,
			SessionEntriesHash: hash,
			ComputeVersion:     8,
			ClassifierVersion:  metrics.CurrentClassifierAnnotationVersion,
			AnnotatedAt:        time.UnixMilli(1700000000000),
		},
	}
	as := newFullAnnotationStore()
	ca := metrics.NewClassifierAnnotator(ms, as)

	if err := ca.Annotate(context.Background(), sid); err != nil {
		t.Fatalf("Annotate: %v", err)
	}
	if ms.listEntriesCalls != 1 {
		t.Fatalf("ListEntries calls = %d, want 1", ms.listEntriesCalls)
	}
	if ms.saveAnnotationCalls != 1 {
		t.Fatalf("SaveAnnotationRunState calls = %d, want 1", ms.saveAnnotationCalls)
	}
}

func TestClassifierAnnotator_Annotate_PersistFailureDoesNotSaveState(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)
	ms := &stubMetricsStore{
		entries:            []schema.SessionEntry{buildEntry(sid, 0, ingest.RoleUser, "fuck this bug")},
		m:                  buildVersionedMetrics(sid, 10),
		sessionEntriesHash: strings.Repeat("e", 64),
	}
	as := newFullAnnotationStore()
	as.entryCreateErr = map[int]error{0: errors.New("simulated DB error")}
	ca := metrics.NewClassifierAnnotator(ms, as)

	if err := ca.Annotate(context.Background(), sid); err != nil {
		t.Fatalf("Annotate: %v", err)
	}
	if ms.listEntriesCalls != 1 {
		t.Fatalf("ListEntries calls = %d, want 1", ms.listEntriesCalls)
	}
	if ms.saveAnnotationCalls != 0 {
		t.Fatalf("SaveAnnotationRunState calls = %d, want 0", ms.saveAnnotationCalls)
	}
}

// --- Entry-level classifier: persistResult dispatch tests ---

// TestPersistResult_TargetNil_CreatesSessionAnnotation verifies that when
// Target is nil, persistResult calls CreateSessionAnnotation (not CreateEntryAnnotation).
func TestPersistResult_TargetNil_CreatesSessionAnnotation(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)

	// Session with frustration pattern → session-level classifiers fire.
	ms := &stubMetricsStore{
		entries: []schema.SessionEntry{buildEntry(sid, 0, ingest.RoleUser, "please fix the bug")},
		m:       buildMetrics(sid, 3, 4),
	}
	as := newFullAnnotationStore()

	ca := metrics.NewClassifierAnnotator(ms, as)
	if err := ca.Annotate(context.Background(), sid); err != nil {
		t.Fatalf("Annotate: %v", err)
	}

	// Session-level classifiers should create session annotations.
	if len(as.created) == 0 {
		t.Fatal("expected session-level annotations to be created via CreateSessionAnnotation")
	}

	// Verify at least one session annotation has the frustration annotator.
	foundSession := false
	for _, p := range as.created {
		if p.AnnotatorID == "anntr-uuid-2" && p.Value == "not_detected" {
			foundSession = true
			break
		}
	}
	if !foundSession {
		t.Errorf("expected session-level frustration annotation with not_detected; created: %+v", as.created)
	}
}

// TestPersistResult_TargetNonNil_CreatesEntryAnnotation verifies that when
// Target is non-nil, persistResult calls CreateEntryAnnotation (not CreateSessionAnnotation).
func TestPersistResult_TargetNonNil_CreatesEntryAnnotation(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)

	// Session with frustration pattern in entry → entry-level classifier fires.
	ms := &stubMetricsStore{
		entries: []schema.SessionEntry{buildEntry(sid, 0, ingest.RoleUser, "fuck this bug")},
		m:       buildMetrics(sid, 3, 4),
	}
	as := newFullAnnotationStore()

	ca := metrics.NewClassifierAnnotator(ms, as)
	if err := ca.Annotate(context.Background(), sid); err != nil {
		t.Fatalf("Annotate: %v", err)
	}

	// Entry-level classifiers should create entry annotations.
	if len(as.entryCreated) == 0 {
		t.Fatal("expected entry-level annotations to be created via CreateEntryAnnotation")
	}

	// Verify the frustration signal entry annotation targets entry index 0.
	foundEntry := false
	for _, p := range as.entryCreated {
		if p.AnnotatorID == "anntr-uuid-4" && p.Value == "detected" && p.EntryIndex == 0 {
			foundEntry = true
			break
		}
	}
	if !foundEntry {
		t.Errorf("expected entry-level frustration_signal annotation at entry 0; entryCreated: %+v", as.entryCreated)
	}
}

// --- Multi-result persistence ---

// TestMultiResult_AllPersisted verifies that 3 entry-level spans all get persisted.
func TestMultiResult_AllPersisted(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)

	// 3 entries with frustration patterns → 3 entry-level results.
	ms := &stubMetricsStore{
		entries: []schema.SessionEntry{
			buildEntry(sid, 0, ingest.RoleUser, "fuck this"),
			buildEntry(sid, 1, ingest.RoleUser, "fucking broken"),
			buildEntry(sid, 2, ingest.RoleUser, "what the fuck"),
		},
		m: buildMetrics(sid, 3, 4),
	}
	as := newFullAnnotationStore()

	ca := metrics.NewClassifierAnnotator(ms, as)
	if err := ca.Annotate(context.Background(), sid); err != nil {
		t.Fatalf("Annotate: %v", err)
	}

	// Count frustration_signal entry annotations.
	frustrationEntryCount := 0
	for _, p := range as.entryCreated {
		if p.AnnotationTypeID == "type-uuid-40" && p.Value == "detected" {
			frustrationEntryCount++
		}
	}
	if frustrationEntryCount != 3 {
		t.Errorf("expected 3 frustration_signal entry annotations, got %d; entryCreated: %+v",
			frustrationEntryCount, as.entryCreated)
	}
}

// --- Partial failure ---

// TestPartialFailure_SomePersisted verifies that when span 2 (entry index 1) fails,
// spans 1 and 3 (entry indices 0 and 2) still succeed.
func TestPartialFailure_SomePersisted(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)

	ms := &stubMetricsStore{
		entries: []schema.SessionEntry{
			buildEntry(sid, 0, ingest.RoleUser, "fuck this"),
			buildEntry(sid, 1, ingest.RoleUser, "fucking broken"),
			buildEntry(sid, 2, ingest.RoleUser, "what the fuck"),
		},
		m: buildMetrics(sid, 3, 4),
	}
	as := newFullAnnotationStore()
	// Inject error for entry index 1.
	as.entryCreateErr = map[int]error{
		1: errors.New("simulated DB error"),
	}

	ca := metrics.NewClassifierAnnotator(ms, as)
	// Annotate should not return error (best-effort).
	if err := ca.Annotate(context.Background(), sid); err != nil {
		t.Fatalf("Annotate: %v", err)
	}

	// Entries 0 and 2 should succeed; entry 1 should fail.
	frustrationEntryCount := 0
	for _, p := range as.entryCreated {
		if p.AnnotationTypeID == "type-uuid-40" && p.Value == "detected" {
			frustrationEntryCount++
		}
	}
	if frustrationEntryCount != 2 {
		t.Errorf("expected 2 successful frustration_signal entry annotations (entry 1 failed), got %d; entryCreated: %+v",
			frustrationEntryCount, as.entryCreated)
	}

	// Verify the successful entries are indices 0 and 2.
	indices := map[int]bool{}
	for _, p := range as.entryCreated {
		if p.AnnotationTypeID == "type-uuid-40" {
			indices[p.EntryIndex] = true
		}
	}
	if !indices[0] || !indices[2] {
		t.Errorf("expected entries 0 and 2 persisted; got indices: %v", indices)
	}
	if indices[1] {
		t.Errorf("entry 1 should have failed; got indices: %v", indices)
	}
}

// --- EndIndex=0 defaults ---

// TestEndIndex_ZeroDefaultsToEntryIndexPlusOne verifies that a Target with EndIndex=0
// results in a single-entry span (semantically EntryIndex+1).
func TestEndIndex_ZeroDefaultsToEntryIndexPlusOne(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)

	// One frustration entry → entry classifier produces one result.
	ms := &stubMetricsStore{
		entries: []schema.SessionEntry{buildEntry(sid, 0, ingest.RoleUser, "fuck this")},
		m:       buildMetrics(sid, 3, 4),
	}
	as := newFullAnnotationStore()

	ca := metrics.NewClassifierAnnotator(ms, as)
	if err := ca.Annotate(context.Background(), sid); err != nil {
		t.Fatalf("Annotate: %v", err)
	}

	// Entry classifier result should have EndIndex=0 (default single-entry span).
	if len(as.entryCreated) == 0 {
		t.Fatal("expected entry annotations to be created")
	}
	for _, p := range as.entryCreated {
		if p.AnnotationTypeID == "type-uuid-40" {
			// The EntryClassifierFunc sets EndIndex=0 (default) → persistResult passes 0.
			// CreateAnnotation/CreateEntryAnnotation handles the 0→EntryIndex+1 default.
			if p.EndIndex != 0 {
				t.Errorf("EndIndex = %d, want 0 (default single-entry span)", p.EndIndex)
			}
			break
		}
	}
}

// --- classifyFrustrationEntries (entry-level) tests ---

// TestClassifyFrustrationEntries_DetectsExpletiveAtCorrectIndex verifies that the
// frustration entry classifier identifies the correct entry index.
func TestClassifyFrustrationEntries_DetectsExpletiveAtCorrectIndex(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)
	engine := metrics.NewClassifierEngine()

	entries := []schema.SessionEntry{
		buildEntry(sid, 0, ingest.RoleUser, "please fix the routing issue"),
		buildEntry(sid, 1, ingest.RoleAssistant, "I found the bug"),
		buildEntry(sid, 2, ingest.RoleUser, "fuck this, it is still broken"),
		buildEntry(sid, 3, ingest.RoleAssistant, "let me try again"),
	}
	results := engine.Run(context.Background(), sid, entries, nil)

	frustrationSignals := findResults(results, testutil.TestTypeIDFrustrationSignal)
	if len(frustrationSignals) != 1 {
		t.Fatalf("expected 1 frustration_signal result, got %d", len(frustrationSignals))
	}

	r := frustrationSignals[0]
	if r.Value != "detected" {
		t.Errorf("Value = %q, want %q", r.Value, "detected")
	}
	if r.Target == nil {
		t.Fatal("expected non-nil Target for entry-level result")
	}
	if r.Target.EntryIndex != 2 {
		t.Errorf("Target.EntryIndex = %d, want 2 (the entry with the expletive)", r.Target.EntryIndex)
	}
	if r.Target.EndIndex != 0 {
		t.Errorf("Target.EndIndex = %d, want 0 (single-entry default)", r.Target.EndIndex)
	}
}

func TestClassifyFrustrationEntries_UsesStoredEntryIndex(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)
	engine := metrics.NewClassifierEngine()

	entries := []schema.SessionEntry{
		buildEntry(sid, 8, ingest.RoleUser, "please fix the routing issue"),
		buildEntry(sid, 21, ingest.RoleAssistant, "I found the bug"),
		buildEntry(sid, 34, ingest.RoleUser, "fuck this, it is still broken"),
	}
	results := engine.Run(context.Background(), sid, entries, nil)

	frustrationSignals := findResults(results, testutil.TestTypeIDFrustrationSignal)
	if len(frustrationSignals) != 1 {
		t.Fatalf("expected 1 frustration_signal result, got %d", len(frustrationSignals))
	}

	r := frustrationSignals[0]
	if r.Target == nil {
		t.Fatal("expected non-nil Target for entry-level result")
	}
	if r.Target.EntryIndex != 34 {
		t.Errorf("Target.EntryIndex = %d, want stored entry index 34", r.Target.EntryIndex)
	}
}

// TestClassifyFrustrationEntries_MultipleExpletives verifies multiple entries with
// frustration patterns each produce their own result.
func TestClassifyFrustrationEntries_MultipleExpletives(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)
	engine := metrics.NewClassifierEngine()

	entries := []schema.SessionEntry{
		buildEntry(sid, 0, ingest.RoleUser, "fuck this"),
		buildEntry(sid, 1, ingest.RoleAssistant, "let me try"),
		buildEntry(sid, 2, ingest.RoleUser, "fucking hell"),
	}
	results := engine.Run(context.Background(), sid, entries, nil)

	frustrationSignals := findResults(results, testutil.TestTypeIDFrustrationSignal)
	if len(frustrationSignals) != 2 {
		t.Fatalf("expected 2 frustration_signal results, got %d", len(frustrationSignals))
	}

	// Verify the indices are 0 and 2.
	indices := make(map[int]bool)
	for _, r := range frustrationSignals {
		if r.Target == nil {
			t.Fatal("expected non-nil Target")
		}
		indices[r.Target.EntryIndex] = true
	}
	if !indices[0] || !indices[2] {
		t.Errorf("expected indices {0, 2}, got %v", indices)
	}
}

// TestClassifyFrustrationEntries_NoExpletives returns no results.
func TestClassifyFrustrationEntries_NoExpletives(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)
	engine := metrics.NewClassifierEngine()

	entries := []schema.SessionEntry{
		buildEntry(sid, 0, ingest.RoleUser, "please fix the bug"),
		buildEntry(sid, 1, ingest.RoleAssistant, "done"),
	}
	results := engine.Run(context.Background(), sid, entries, nil)

	frustrationSignals := findResults(results, testutil.TestTypeIDFrustrationSignal)
	if len(frustrationSignals) != 0 {
		t.Errorf("expected 0 frustration_signal results, got %d", len(frustrationSignals))
	}
}

// TestClassifyFrustrationEntries_EmptyEntries returns no results.
func TestClassifyFrustrationEntries_EmptyEntries(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)
	engine := metrics.NewClassifierEngine()

	results := engine.Run(context.Background(), sid, nil, nil)

	frustrationSignals := findResults(results, testutil.TestTypeIDFrustrationSignal)
	if len(frustrationSignals) != 0 {
		t.Errorf("expected 0 frustration_signal results for empty entries, got %d", len(frustrationSignals))
	}
}

// --- classifyResolutionEntries (entry-level) tests ---

// TestClassifyResolutionEntries_MarksResolutionEvidence verifies that the outcome
// entry classifier identifies resolution evidence at the correct turn index.
func TestClassifyResolutionEntries_MarksResolutionEvidence(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)
	engine := metrics.NewClassifierEngine()

	entries := []schema.SessionEntry{
		buildEntry(sid, 0, ingest.RoleUser, "fix the routing issue"),
		buildEntry(sid, 1, ingest.RoleAssistant, "I found the bug"),
		buildEntry(sid, 2, ingest.RoleUser, "great, test it"),
		buildEntry(sid, 3, ingest.RoleAssistant, "All tests pass and the fix is complete"),
	}
	results := engine.Run(context.Background(), sid, entries, nil)

	resolutionEvidence := findResults(results, testutil.TestTypeIDResolutionEvidence)
	if len(resolutionEvidence) == 0 {
		t.Fatal("expected at least 1 resolution_evidence result")
	}

	// Entry at index 3 says "All tests pass" which contains "all tests pass".
	foundIdx3 := false
	for _, r := range resolutionEvidence {
		if r.Target != nil && r.Target.EntryIndex == 3 {
			foundIdx3 = true
			if r.Value != "present" {
				t.Errorf("Value = %q, want %q", r.Value, "present")
			}
		}
	}
	if !foundIdx3 {
		t.Error("expected resolution_evidence result at entry index 3")
	}
}

func TestClassifyResolutionEntries_UsesStoredEntryIndex(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)
	engine := metrics.NewClassifierEngine()

	entries := []schema.SessionEntry{
		buildEntry(sid, 11, ingest.RoleUser, "fix the routing issue"),
		buildEntry(sid, 17, ingest.RoleAssistant, "I found the bug"),
		buildEntry(sid, 42, ingest.RoleAssistant, "All tests pass and the fix is complete"),
	}
	results := engine.Run(context.Background(), sid, entries, nil)

	resolutionEvidence := findResults(results, testutil.TestTypeIDResolutionEvidence)
	if len(resolutionEvidence) == 0 {
		t.Fatal("expected at least 1 resolution_evidence result")
	}

	foundStoredIndex := false
	for _, r := range resolutionEvidence {
		if r.Target == nil {
			t.Fatal("expected non-nil Target for entry-level result")
		}
		if r.Target.EntryIndex == 42 {
			foundStoredIndex = true
		}
		if r.Target.EntryIndex == 2 {
			t.Errorf("Target.EntryIndex = %d, want stored entry index 42, not slice offset", r.Target.EntryIndex)
		}
	}
	if !foundStoredIndex {
		t.Errorf("expected resolution_evidence result at stored entry index 42; got %+v", resolutionEvidence)
	}
}

// TestClassifyResolutionEntries_OnlyAssistantEntries verifies that user entries
// are not scanned for resolution evidence.
func TestClassifyResolutionEntries_OnlyAssistantEntries(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)
	engine := metrics.NewClassifierEngine()

	// User says "done" — should NOT be picked up as resolution evidence.
	entries := []schema.SessionEntry{
		buildEntry(sid, 0, ingest.RoleUser, "I successfully fixed it myself, done"),
	}
	results := engine.Run(context.Background(), sid, entries, nil)

	resolutionEvidence := findResults(results, testutil.TestTypeIDResolutionEvidence)
	if len(resolutionEvidence) != 0 {
		t.Errorf("expected 0 resolution_evidence results (user entries only), got %d", len(resolutionEvidence))
	}
}

// TestClassifyResolutionEntries_EmptyEntries returns no results.
func TestClassifyResolutionEntries_EmptyEntries(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)
	engine := metrics.NewClassifierEngine()

	results := engine.Run(context.Background(), sid, nil, nil)

	resolutionEvidence := findResults(results, testutil.TestTypeIDResolutionEvidence)
	if len(resolutionEvidence) != 0 {
		t.Errorf("expected 0 resolution_evidence results for empty entries, got %d", len(resolutionEvidence))
	}
}

// TestClassifyResolutionEntries_NoResolutionPhrases returns no results when
// no resolution evidence phrases are found.
func TestClassifyResolutionEntries_NoResolutionPhrases(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)
	engine := metrics.NewClassifierEngine()

	entries := []schema.SessionEntry{
		buildEntry(sid, 0, ingest.RoleUser, "what is wrong"),
		buildEntry(sid, 1, ingest.RoleAssistant, "looking into it"),
		buildEntry(sid, 2, ingest.RoleUser, "any ideas"),
		buildEntry(sid, 3, ingest.RoleAssistant, "still investigating"),
	}
	results := engine.Run(context.Background(), sid, entries, nil)

	resolutionEvidence := findResults(results, testutil.TestTypeIDResolutionEvidence)
	if len(resolutionEvidence) != 0 {
		t.Errorf("expected 0 resolution_evidence results, got %d", len(resolutionEvidence))
	}
}

// TestClassifyResolutionEntries_MultipleEvidenceTurns verifies multiple assistant
// entries with resolution evidence each produce their own result.
func TestClassifyResolutionEntries_MultipleEvidenceTurns(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)
	engine := metrics.NewClassifierEngine()

	entries := []schema.SessionEntry{
		buildEntry(sid, 0, ingest.RoleUser, "fix both issues"),
		buildEntry(sid, 1, ingest.RoleAssistant, "Fixed the first bug"),
		buildEntry(sid, 2, ingest.RoleUser, "now fix the second"),
		buildEntry(sid, 3, ingest.RoleAssistant, "Successfully resolved the second issue"),
	}
	results := engine.Run(context.Background(), sid, entries, nil)

	resolutionEvidence := findResults(results, testutil.TestTypeIDResolutionEvidence)
	if len(resolutionEvidence) != 2 {
		t.Fatalf("expected 2 resolution_evidence results, got %d", len(resolutionEvidence))
	}

	indices := make(map[int]bool)
	for _, r := range resolutionEvidence {
		if r.Target == nil {
			t.Fatal("expected non-nil Target")
		}
		indices[r.Target.EntryIndex] = true
	}
	if !indices[1] || !indices[3] {
		t.Errorf("expected indices {1, 3}, got %v", indices)
	}
}

// ---------------------------------------------------------------------------
// Dedup / Supersession tests (R9)
// ---------------------------------------------------------------------------

// TestDedup_SameHash_Skip verifies that when an existing annotation has the same
// content hash as the new one, no new annotation is created (dedup skip).
func TestDedup_SameHash_Skip(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)

	// Prepare entries that produce a known outcome annotation.
	ms := &stubMetricsStore{
		entries: []schema.SessionEntry{
			buildEntry(sid, 0, ingest.RoleUser, "please fix the bug"),
			buildEntry(sid, 1, ingest.RoleAssistant, "done"),
		},
		m: buildMetrics(sid, 3, 4),
	}

	as := newFullAnnotationStore()

	// Pre-compute what the content hash WILL be for the outcome annotation.
	// Run the classifier engine once to get the result, then compute the hash.
	engine := metrics.NewClassifierEngine()
	results := engine.Run(context.Background(), sid, ms.entries, ms.m)
	outcomeResult := findResult(results, testutil.TestTypeIDSessionOutcome)
	if outcomeResult == nil {
		t.Fatal("classifier should produce an outcome result")
	}

	// Compute the content hash the same way persistResult does.
	var confidence *float64
	if outcomeResult.Confidence > 0 {
		confidence = &outcomeResult.Confidence
	}
	var reason *string
	if outcomeResult.Reason != "" {
		reason = &outcomeResult.Reason
	}
	sidStr := string(sid)
	expectedHash := schema.ComputeAnnotationHash(
		"type-uuid-10", "anntr-uuid-1", outcomeResult.Value,
		&sidStr, nil, nil,
		confidence, reason, outcomeResult.Provenance,
	)

	// Seed the existing annotation with the SAME hash → should skip.
	as.existing = map[string]*ingest.ExistingAnnotation{
		"type-uuid-10|anntr-uuid-1|" + sidStr: {
			ID:          "existing-ann-id",
			ContentHash: expectedHash,
		},
	}

	ca := metrics.NewClassifierAnnotator(ms, as)
	if err := ca.Annotate(context.Background(), sid); err != nil {
		t.Fatalf("Annotate: %v", err)
	}

	// Outcome classifier should have been skipped (same hash).
	// Other classifiers (frustration, scope) may still create annotations.
	for _, p := range as.created {
		if p.AnnotationTypeID == "type-uuid-10" {
			t.Errorf("expected outcome annotation to be SKIPPED (same hash), but CreateSessionAnnotation was called: %+v", p)
		}
	}

	// SupersedeAnnotation should NOT have been called for the outcome.
	for _, pair := range as.superseded {
		if pair[0] == "existing-ann-id" {
			t.Errorf("expected no supersession for same-hash annotation, but SupersedeAnnotation was called: %v", pair)
		}
	}
}

// TestDedup_DifferentHash_Supersede verifies that when an existing annotation has
// a different content hash, a new annotation is created and the old one is superseded.
func TestDedup_DifferentHash_Supersede(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)

	ms := &stubMetricsStore{
		entries: []schema.SessionEntry{
			buildEntry(sid, 0, ingest.RoleUser, "please fix the bug"),
			buildEntry(sid, 1, ingest.RoleAssistant, "done"),
		},
		m: buildMetrics(sid, 3, 4),
	}

	as := newFullAnnotationStore()

	// Seed with a DIFFERENT hash → should supersede.
	sidStr := string(sid)
	as.existing = map[string]*ingest.ExistingAnnotation{
		"type-uuid-10|anntr-uuid-1|" + sidStr: {
			ID:          "old-outcome-ann",
			ContentHash: "different-hash-abc",
		},
	}

	ca := metrics.NewClassifierAnnotator(ms, as)
	if err := ca.Annotate(context.Background(), sid); err != nil {
		t.Fatalf("Annotate: %v", err)
	}

	// A new outcome annotation should have been created.
	foundOutcomeCreate := false
	for _, p := range as.created {
		if p.AnnotationTypeID == "type-uuid-10" {
			foundOutcomeCreate = true
			break
		}
	}
	if !foundOutcomeCreate {
		t.Error("expected new outcome annotation to be created (different hash → supersede)")
	}

	// The old annotation should have been superseded.
	foundSupersede := false
	for _, pair := range as.superseded {
		if pair[0] == "old-outcome-ann" {
			foundSupersede = true
			break
		}
	}
	if !foundSupersede {
		t.Errorf("expected SupersedeAnnotation to be called for old-outcome-ann; superseded: %v", as.superseded)
	}

	// Content hash should have been stored on the new annotation.
	foundHashUpdate := false
	for _, pair := range as.contentHashes {
		if pair[0] != "" && pair[1] != "" {
			foundHashUpdate = true
			break
		}
	}
	if !foundHashUpdate {
		t.Error("expected UpdateContentHash to be called for the new annotation")
	}
}

// TestDedup_NoExisting_Create verifies that when no existing annotation is found,
// a new one is created and its content hash is stored.
func TestDedup_NoExisting_Create(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)

	ms := &stubMetricsStore{
		entries: []schema.SessionEntry{
			buildEntry(sid, 0, ingest.RoleUser, "please fix the bug"),
			buildEntry(sid, 1, ingest.RoleAssistant, "done"),
		},
		m: buildMetrics(sid, 3, 4),
	}

	as := newFullAnnotationStore()
	// No existing annotations seeded.

	ca := metrics.NewClassifierAnnotator(ms, as)
	if err := ca.Annotate(context.Background(), sid); err != nil {
		t.Fatalf("Annotate: %v", err)
	}

	// Session annotations should have been created (outcome and frustration at least).
	if len(as.created) == 0 {
		t.Fatal("expected session annotations to be created")
	}

	// No supersession should occur.
	if len(as.superseded) != 0 {
		t.Errorf("expected no supersession calls, got %d", len(as.superseded))
	}

	// Content hashes should have been stored.
	if len(as.contentHashes) == 0 {
		t.Error("expected content hashes to be stored on new annotations")
	}
}

// TestDedup_EntryLevel_SameHash_Skip verifies that entry-level annotations are
// also deduplicated when the content hash matches.
func TestDedup_EntryLevel_SameHash_Skip(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)

	ms := &stubMetricsStore{
		entries: []schema.SessionEntry{
			buildEntry(sid, 0, ingest.RoleUser, "fuck this bug"),
		},
		m: buildMetrics(sid, 3, 4),
	}

	as := newFullAnnotationStore()

	// Run engine to get the exact entry-level result.
	engine := metrics.NewClassifierEngine()
	results := engine.Run(context.Background(), sid, ms.entries, ms.m)
	frustSignals := findResults(results, testutil.TestTypeIDFrustrationSignal)
	if len(frustSignals) == 0 {
		t.Fatal("expected frustration signal results")
	}

	// Compute the hash that persistResult will compute for entry index 0.
	r := frustSignals[0]
	var confidence *float64
	if r.Confidence > 0 {
		confidence = &r.Confidence
	}
	var reason *string
	if r.Reason != "" {
		reason = &r.Reason
	}
	sidStr := string(sid)
	entryIdx := r.Target.EntryIndex
	expectedHash := schema.ComputeAnnotationHash(
		"type-uuid-40", "anntr-uuid-4", r.Value,
		&sidStr, &entryIdx, nil,
		confidence, reason, r.Provenance,
	)

	// Seed the existing entry annotation with the same hash.
	as.existing = map[string]*ingest.ExistingAnnotation{
		fmt.Sprintf("type-uuid-40|anntr-uuid-4|%s|%d", sidStr, entryIdx): {
			ID:          "existing-entry-ann",
			ContentHash: expectedHash,
		},
	}

	ca := metrics.NewClassifierAnnotator(ms, as)
	if err := ca.Annotate(context.Background(), sid); err != nil {
		t.Fatalf("Annotate: %v", err)
	}

	// The frustration signal entry annotation should have been skipped.
	for _, p := range as.entryCreated {
		if p.AnnotationTypeID == "type-uuid-40" && p.EntryIndex == entryIdx {
			t.Errorf("expected entry annotation at index %d to be SKIPPED (same hash), but CreateEntryAnnotation was called", entryIdx)
		}
	}
}

// TestDedup_EntryLevel_DifferentHash_Supersede verifies that entry-level
// annotations with different hashes trigger supersession.
func TestDedup_EntryLevel_DifferentHash_Supersede(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)

	ms := &stubMetricsStore{
		entries: []schema.SessionEntry{
			buildEntry(sid, 0, ingest.RoleUser, "fuck this bug"),
		},
		m: buildMetrics(sid, 3, 4),
	}

	as := newFullAnnotationStore()

	// Seed with a different hash for the entry-level frustration signal.
	sidStr := string(sid)
	as.existing = map[string]*ingest.ExistingAnnotation{
		fmt.Sprintf("type-uuid-40|anntr-uuid-4|%s|0", sidStr): {
			ID:          "old-entry-ann",
			ContentHash: "old-hash-xyz",
		},
	}

	ca := metrics.NewClassifierAnnotator(ms, as)
	if err := ca.Annotate(context.Background(), sid); err != nil {
		t.Fatalf("Annotate: %v", err)
	}

	// New entry annotation should have been created.
	foundEntryCreate := false
	for _, p := range as.entryCreated {
		if p.AnnotationTypeID == "type-uuid-40" && p.EntryIndex == 0 {
			foundEntryCreate = true
			break
		}
	}
	if !foundEntryCreate {
		t.Error("expected new entry annotation to be created (different hash → supersede)")
	}

	// Old entry annotation should have been superseded.
	foundSupersede := false
	for _, pair := range as.superseded {
		if pair[0] == "old-entry-ann" {
			foundSupersede = true
			break
		}
	}
	if !foundSupersede {
		t.Errorf("expected SupersedeAnnotation for old-entry-ann; superseded: %v", as.superseded)
	}
}

// TestDedup_ProvenanceVersionChange_TriggersSupersession verifies that a change in
// Provenance.Version (e.g., classifier version bump) produces a different content
// hash and triggers supersession of the old annotation.
func TestDedup_ProvenanceVersionChange_TriggersSupersession(t *testing.T) {
	t.Parallel()

	sidStr := "99d59925-36bc-424c-a789-8be54d9702ba"
	typeID := "type-uuid-10"
	annotatorID := "anntr-uuid-1"

	// Hash with provenance v1.
	prov1 := &schema.Provenance{Method: "heuristic", Version: "v1"}
	val := "resolved"
	hash1 := schema.ComputeAnnotationHash(
		typeID, annotatorID, val,
		&sidStr, nil, nil,
		nil, nil, prov1,
	)

	// Hash with provenance v2.
	prov2 := &schema.Provenance{Method: "heuristic", Version: "v2"}
	hash2 := schema.ComputeAnnotationHash(
		typeID, annotatorID, val,
		&sidStr, nil, nil,
		nil, nil, prov2,
	)

	// The hashes must differ — a version bump must trigger supersession.
	if hash1 == hash2 {
		t.Error("expected different content hashes for different Provenance.Version, got same hash")
	}
	if hash1 == "" || hash2 == "" {
		t.Error("content hashes should be non-empty")
	}
}

// TestAnnotationDedupResult_String verifies the String() method on AnnotationDedupResult.
func TestAnnotationDedupResult_String(t *testing.T) {
	t.Parallel()
	cases := []struct {
		d    ingest.AnnotationDedupResult
		want string
	}{
		{ingest.DedupCreate, "create"},
		{ingest.DedupSkip, "skip"},
		{ingest.DedupSupersede, "supersede"},
		{ingest.AnnotationDedupResult(99), "unknown"},
	}
	for _, tc := range cases {
		if got := tc.d.String(); got != tc.want {
			t.Errorf("AnnotationDedupResult(%d).String() = %q, want %q", int(tc.d), got, tc.want)
		}
	}
}

func TestClassifierAnnotator_AnnotateWithProfile_RecordsAnnotationWork(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)
	ms := &stubMetricsStore{entries: []schema.SessionEntry{buildEntry(sid, 0, ingest.RoleUser, "fuck this bug")}, m: buildMetrics(sid, 5, 4)}
	as := newFullAnnotationStore()
	profiler := &ingest.IndexProfiler{}

	ca := metrics.NewClassifierAnnotator(ms, as)
	if err := ca.AnnotateWithProfile(context.Background(), sid, profiler); err != nil {
		t.Fatalf("AnnotateWithProfile returned error: %v", err)
	}

	stats := profiler.Snapshot().Annotation
	if stats.ListEntriesCount != 1 || stats.GetMetricsCount != 1 || stats.ClassifierRunCount != 1 {
		t.Fatalf("profile load/run counts = list:%d metrics:%d run:%d, want 1 each", stats.ListEntriesCount, stats.GetMetricsCount, stats.ClassifierRunCount)
	}
	if stats.ResultCount == 0 {
		t.Fatal("ResultCount = 0, want classifier results")
	}
	if stats.ResultCount != stats.SessionResultCount+stats.EntryResultCount {
		t.Fatalf("result target counts do not add up: total=%d session=%d entry=%d", stats.ResultCount, stats.SessionResultCount, stats.EntryResultCount)
	}
	if stats.IDCacheMisses != stats.ResultCount || stats.DedupLookupCount != stats.ResultCount || stats.DedupCreateCount != stats.ResultCount || stats.UpdateContentHashCount != stats.ResultCount {
		t.Fatalf("profile result counters do not match result count %d: %+v", stats.ResultCount, stats)
	}
	if stats.CreateSessionCount != len(as.created) || stats.CreateEntryCount != len(as.entryCreated) {
		t.Fatalf("profile create counts = session:%d entry:%d, want session:%d entry:%d", stats.CreateSessionCount, stats.CreateEntryCount, len(as.created), len(as.entryCreated))
	}
}

func TestClassifierAnnotator_Annotate_UsesBatchPersistenceWhenAvailable(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)
	ms := &stubMetricsStore{entries: []schema.SessionEntry{buildEntry(sid, 0, ingest.RoleUser, "fuck this bug"), buildEntry(sid, 1, ingest.RoleAssistant, "All tests pass")}, m: buildMetrics(sid, 3, 4)}
	as := &batchAnnotationStore{stubAnnotationStore: newFullAnnotationStore()}
	ca := metrics.NewClassifierAnnotator(ms, as)

	if err := ca.Annotate(context.Background(), sid); err != nil {
		t.Fatalf("Annotate: %v", err)
	}
	if len(as.writes) == 0 {
		t.Fatal("expected classifier results to use batch persistence")
	}
	if len(as.created) != 0 || len(as.entryCreated) != 0 {
		t.Fatalf("expected no fallback create calls when batch persistence is available; session=%d entry=%d", len(as.created), len(as.entryCreated))
	}

	foundEntryWrite := false
	foundSessionWrite := false
	for _, write := range as.writes {
		if write.ContentHash == "" {
			t.Fatal("batch write had empty content hash")
		}
		if write.Create.SessionID != nil {
			foundSessionWrite = true
		}
		if write.Create.EntryTarget != nil && write.Find.EntryIndex != nil {
			foundEntryWrite = true
		}
	}
	if !foundSessionWrite || !foundEntryWrite {
		t.Fatalf("batch target coverage = session:%t entry:%t, want both", foundSessionWrite, foundEntryWrite)
	}
}

func TestClassifierAnnotator_AnnotateWithProfile_RecordsBatchPersistence(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)
	ms := &stubMetricsStore{entries: []schema.SessionEntry{buildEntry(sid, 0, ingest.RoleUser, "fuck this bug")}, m: buildMetrics(sid, 3, 4)}
	as := &batchAnnotationStore{stubAnnotationStore: newFullAnnotationStore()}
	profiler := &ingest.IndexProfiler{}
	ca := metrics.NewClassifierAnnotator(ms, as)

	if err := ca.AnnotateWithProfile(context.Background(), sid, profiler); err != nil {
		t.Fatalf("AnnotateWithProfile: %v", err)
	}
	stats := profiler.Snapshot().Annotation
	if stats.BatchWriteCount != 1 || stats.BatchResultCount != len(as.writes) || stats.DedupCreateCount != len(as.writes) {
		t.Fatalf("batch profile counters do not match writes %d: %+v", len(as.writes), stats)
	}
	if stats.BatchDedupLookupCount != len(as.writes) || stats.BatchInsertParentCount != len(as.writes) || stats.BatchInsertTargetCount != len(as.writes) || stats.BatchUpdateHashCount != len(as.writes) {
		t.Fatalf("profiled batch store counters do not match writes %d: %+v", len(as.writes), stats)
	}
	if stats.DedupLookupCount != 0 || stats.CreateSessionCount != 0 || stats.CreateEntryCount != 0 || stats.UpdateContentHashCount != 0 {
		t.Fatalf("batch profile double-counted per-result operations: %+v", stats)
	}
	breakdownTotal := 0
	foundSession := false
	foundEntry := false
	for _, row := range stats.SortedAnnotationResults() {
		breakdownTotal += row.SkipCount + row.CreateCount + row.SupersedeCount
		if row.TargetKind == ingest.AnnotationProfileTargetSession {
			foundSession = true
		}
		if row.TargetKind == ingest.AnnotationProfileTargetEntry {
			foundEntry = true
		}
	}
	if breakdownTotal != len(as.writes) || !foundSession || !foundEntry {
		t.Fatalf("annotation result breakdown total=%d session=%t entry=%t, want total=%d and both targets: %+v", breakdownTotal, foundSession, foundEntry, len(as.writes), stats.SortedAnnotationResults())
	}
}
