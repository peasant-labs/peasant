package export_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/export"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/transcript"
	"github.com/peasant-labs/schema"
)

// attemptBarrierLocker delegates to the real per-session file locker and
// observably signals when an exclusive attempt starts, so the test proves a
// genuine flock contention rather than a scheduler pause.
type attemptBarrierLocker struct {
	store.SessionLocker
	attempts chan struct{}
}

func (b *attemptBarrierLocker) LockExclusive(ctx context.Context, id schema.SessionID) (func() error, error) {
	select {
	case b.attempts <- struct{}{}:
	default:
	}
	return b.SessionLocker.LockExclusive(ctx, id)
}

// pauseAfterMaterialize wraps the real snapshot reader and holds the shared
// session lock AFTER the production export boundary has hydrated, folded,
// validated and serialized one generation. The real reader keeps the lock until
// the callback returns, so an activation attempted while this wrapper blocks
// must wait even though the export artefact is already materialized.
type pauseAfterMaterialize struct {
	inner   indexformat.SnapshotReader
	entered chan string
	release chan struct{}
}

func (r pauseAfterMaterialize) WithSessionSnapshot(ctx context.Context, id schema.SessionID, fn func(indexformat.ReadSnapshot) error) error {
	return r.inner.WithSessionSnapshot(ctx, id, func(snapshot indexformat.ReadSnapshot) error {
		if err := fn(snapshot); err != nil {
			return err
		}
		r.entered <- snapshot.GenerationID
		<-r.release
		return nil
	})
}

// openBarrierGenerationStore opens a real generation-capable store with the
// production artifact and lock implementations behind the attempt barrier.
func openBarrierGenerationStore(t *testing.T, attempts chan struct{}) *store.Store {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "artifacts")
	artifacts, err := store.NewOSGenerationArtifactStore(root)
	if err != nil {
		t.Fatal(err)
	}
	locker, err := store.NewFileSessionLocker(root)
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(
		filepath.Join(dir, "generations.db"),
		store.WithPoolSize(2),
		store.WithIndexFormats(store.V2IndexFormat()),
		store.WithGenerationArtifacts(artifacts, &attemptBarrierLocker{SessionLocker: locker, attempts: attempts}),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// buildBarrierGeneration builds a self-contained V2 candidate: a user text
// record, an assistant reply carrying a folded tool call and its result, with
// full bodies longer than the display preview limit. Content records carry only
// their refs; activation fills the managed path, byte length and integrity
// digest. Tool rows are depth-one children of the assistant turn, so folding
// attaches them instead of emitting a second owner for the same ref.
func buildBarrierGeneration(t *testing.T, sid schema.SessionID, genID, text, toolInput, toolOutput string) (indexformat.V2, map[schema.SourceEntryRef][]byte) {
	t.Helper()
	refs := []schema.SourceEntryRef{"e_u1", "e_a1", "e_call1", "e_result1"}
	callID := "call-barrier"
	parent := 1
	entries := []schema.SessionEntry{
		{SessionID: sid, EntryIndex: 0, Harness: defaults.HarnessClaudeCode, EntryType: schema.EntryTypeText, Role: schema.RoleUser, ContentPreview: &text, SourceEntryRef: refs[0]},
		{SessionID: sid, EntryIndex: 1, Harness: defaults.HarnessClaudeCode, EntryType: schema.EntryTypeText, Role: schema.RoleAssistant, ContentPreview: &text, SourceEntryRef: refs[1]},
		{SessionID: sid, EntryIndex: 2, Harness: defaults.HarnessClaudeCode, EntryType: schema.EntryTypeToolUse, Role: schema.RoleAssistant, Depth: 1, ParentIndex: &parent, ToolInput: &toolInput, ToolCallID: &callID, SourceEntryRef: refs[2]},
		{SessionID: sid, EntryIndex: 3, Harness: defaults.HarnessClaudeCode, EntryType: schema.EntryTypeToolResult, Role: schema.RoleTool, Depth: 1, ParentIndex: &parent, ToolOutput: &toolOutput, ToolCallID: &callID, SourceEntryRef: refs[3]},
	}
	inputCount := int64(1)
	generation := indexformat.Generation{
		ID:           genID,
		Completeness: indexformat.GenerationCompletenessComplete,
		Metadata: schema.UnifiedMetadata{
			SchemaVersion: ingest.CurrentSchemaVersion,
			SessionID:     sid,
			ModelHarness:  defaults.HarnessClaudeCode,
			Stats:         schema.SessionStats{TurnCount: 2, InputSubmissionCount: &inputCount},
		},
		Main:                 indexformat.Partition{Entries: entries},
		Content:              []indexformat.ContentRecord{{Ref: refs[0]}, {Ref: refs[1]}, {Ref: refs[2]}, {Ref: refs[3]}},
		Aliases:              []indexformat.NativeAlias{{NativeKey: "native-0", Ref: refs[0]}},
		SourceEvidenceDigest: strings.Repeat("a", 64),
		TitleRefs:            []schema.SourceEntryRef{refs[0]},
	}
	blobs := map[schema.SourceEntryRef][]byte{
		refs[0]: []byte(text),
		refs[1]: []byte(text),
		refs[2]: []byte(toolInput),
		refs[3]: []byte(toolOutput),
	}
	return indexformat.V2{Generation: generation}, blobs
}

func activateBarrierGeneration(t *testing.T, s *store.Store, v2 indexformat.V2, blobs map[schema.SourceEntryRef][]byte) error {
	t.Helper()
	return s.ActivateGeneration(context.Background(), store.GenerationActivation{
		Generation:     v2,
		Blobs:          blobs,
		IndexerVersion: 1,
		IndexedAtMs:    1,
	})
}

func waitForExclusiveAttempt(t *testing.T, attempts <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-attempts:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s never attempted the exclusive session lock", label)
	}
}

// TestExportBoundaryHoldsG1AcrossActivation drives the actual durable export
// boundary (export.ExportSnapshotPayload, which shares
// transcript.BuildSnapshotDetailBytes with the detail reads) through the real
// G1/G2 lock barrier. While the boundary holds the shared lock after
// serialization, a G2 activation with changed content must wait: the export
// sees exactly G1 with its full long bodies. After release G2 commits and a
// fresh detail read sees exactly G2.
func TestExportBoundaryHoldsG1AcrossActivation(t *testing.T) {
	sid, err := schema.NewSessionID("45454545-4545-4545-4545-454545454548")
	if err != nil {
		t.Fatal(err)
	}
	attempts := make(chan struct{}, 4)
	s := openBarrierGenerationStore(t, attempts)
	storetest.SeedSession(t, s, string(sid))

	longText := "user input " + strings.Repeat("x", defaults.ContentPreviewLimit+1024)
	longToolInput := "rg parser " + strings.Repeat("y", defaults.ContentPreviewLimit+1024)
	longToolResult := "parser.go:12 " + strings.Repeat("z", defaults.ContentPreviewLimit+1024)
	g1Text := longText + " G1"

	g1, g1Blobs := buildBarrierGeneration(t, sid, "g-export-1", g1Text, longToolInput, longToolResult)
	if err := activateBarrierGeneration(t, s, g1, g1Blobs); err != nil {
		t.Fatalf("activate G1: %v", err)
	}

	paused := pauseAfterMaterialize{inner: s, entered: make(chan string, 1), release: make(chan struct{})}
	type result struct {
		payload *schema.SessionDetailPayload
		err     error
	}
	exportDone := make(chan result, 1)
	go func() {
		payload, err := export.ExportSnapshotPayload(context.Background(), paused, s, sid)
		exportDone <- result{payload: payload, err: err}
	}()

	select {
	case generation := <-paused.entered:
		if generation != "g-export-1" {
			t.Fatalf("export boundary entered with generation %q, want G1", generation)
		}
	case early := <-exportDone:
		t.Fatalf("export boundary returned before materializing: err=%v payload=%+v", early.err, early.payload)
	case <-time.After(5 * time.Second):
		t.Fatal("export boundary did not materialize a G1 payload")
	}

	g2, g2Blobs := buildBarrierGeneration(t, sid, "g-export-2", longText+" G2", longToolInput, longToolResult)
	activationDone := make(chan error, 1)
	go func() { activationDone <- activateBarrierGeneration(t, s, g2, g2Blobs) }()
	waitForExclusiveAttempt(t, attempts, "activation")
	select {
	case err := <-activationDone:
		close(paused.release)
		t.Fatalf("activation completed while the export boundary still held the shared lock (err=%v)", err)
	case <-time.After(200 * time.Millisecond):
	}

	close(paused.release)
	var materialized result
	select {
	case materialized = <-exportDone:
		if materialized.err != nil {
			t.Fatalf("export boundary: %v", materialized.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("export boundary did not return after release")
	}
	select {
	case err := <-activationDone:
		if err != nil {
			t.Fatalf("activation after release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("activation did not complete after the export boundary released the shared lock")
	}

	if len(materialized.payload.Turns) == 0 || materialized.payload.Turns[0].Content != g1Text {
		t.Fatalf("exported G1 turn content = %+v, want the full G1 body", materialized.payload.Turns)
	}
	var call *schema.ToolCallDetail
	for i := range materialized.payload.Turns {
		for j := range materialized.payload.Turns[i].ToolCalls {
			if materialized.payload.Turns[i].ToolCalls[j].Arguments == longToolInput {
				call = &materialized.payload.Turns[i].ToolCalls[j]
			}
		}
	}
	if call == nil {
		t.Fatal("exported G1 payload lost the full tool arguments")
	}
	if call.Result != longToolResult {
		t.Fatalf("exported G1 tool result length = %d, want full %d bytes", len(call.Result), len(longToolResult))
	}

	// Fresh detail read after activation sees wholly G2, proving the materialized
	// G1 export was a coherent generation read rather than a partial mixture.
	_, g2Payload, err := transcript.BuildSnapshotDetailBytes(context.Background(), s, s, sid)
	if err != nil {
		t.Fatalf("G2 detail read: %v", err)
	}
	if len(g2Payload.Turns) == 0 || g2Payload.Turns[0].Content != longText+" G2" {
		t.Fatalf("G2 detail read content = %+v, want the G2 body", g2Payload.Turns)
	}
}

// corruptBlobResolver delegates to the real resolver but fails one ref like a
// damaged artifact whose integrity digest no longer matches.
type corruptBlobResolver struct {
	inner indexformat.ContentResolver
	ref   schema.SourceEntryRef
}

func (c corruptBlobResolver) ReadFullContent(ctx context.Context, sessionID schema.SessionID, generationID string, record indexformat.ContentRecord) ([]byte, error) {
	if record.Ref == c.ref {
		return nil, fmt.Errorf("managed content for source ref %q failed its integrity digest", record.Ref)
	}
	return c.inner.ReadFullContent(ctx, sessionID, generationID, record)
}

// TestExportBoundaryFailsCorruptArtifact proves the export exit refuses a
// committed generation whose managed blob is corrupt instead of emitting a
// truncated payload.
func TestExportBoundaryFailsCorruptArtifact(t *testing.T) {
	sid, err := schema.NewSessionID("45454545-4545-4545-4545-454545454549")
	if err != nil {
		t.Fatal(err)
	}
	s := openBarrierGenerationStore(t, make(chan struct{}, 4))
	storetest.SeedSession(t, s, string(sid))
	g1, blobs := buildBarrierGeneration(t, sid, "g-corrupt", "hello", "call", "the full tool result")
	if err := activateBarrierGeneration(t, s, g1, blobs); err != nil {
		t.Fatalf("activate G1: %v", err)
	}
	corrupt := corruptBlobResolver{inner: s, ref: "e_result1"}
	if _, err := export.ExportSnapshotPayload(context.Background(), s, corrupt, sid); err == nil {
		t.Fatal("a corrupt managed blob must fail the export")
	}
}

// seedSessionWithSource inserts a minimal session row whose recorded native
// source is a real file the test controls.
func seedSessionWithSource(t *testing.T, s *store.Store, sid schema.SessionID, sourcePath string) {
	t.Helper()
	ingested := int64(3)
	entry := ingest.StoreEntry{Metadata: &schema.UnifiedMetadata{
		SessionID:    sid,
		ModelHarness: defaults.HarnessClaudeCode,
		Model:        schema.ModelID("claude-opus-4-6"),
		HostSlug:     schema.HostSlug("testslug"),
		Project: schema.ProjectContext{
			Hash:     schema.ProjectHash("testprojhash0000000000000000000000000000000000000000000000000000"),
			Name:     "testproj",
			FilePath: sourcePath,
		},
		Timestamp: schema.TimestampInfo{Start: 1, End: 2, Ingested: &ingested},
		Source:    schema.SourceInfo{FilePath: sourcePath, Format: schema.SourceFormatJSONL},
	}}
	if err := s.InsertSessions(context.Background(), []ingest.StoreEntry{entry}); err != nil {
		t.Fatalf("seed session with native source: %v", err)
	}
}

// TestExportBoundaryKeepsBytesAfterNativeDeletion proves the committed managed
// generation is self-contained: once the recorded native source is deleted, the
// export exit still returns the exact full long bodies from the immutable
// managed blobs and never reparses the missing original.
func TestExportBoundaryKeepsBytesAfterNativeDeletion(t *testing.T) {
	sid, err := schema.NewSessionID("45454545-4545-4545-4545-454545454550")
	if err != nil {
		t.Fatal(err)
	}
	s := openBarrierGenerationStore(t, make(chan struct{}, 4))
	sourcePath := filepath.Join(t.TempDir(), "native-session.jsonl")
	if err := os.WriteFile(sourcePath, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write native source: %v", err)
	}
	seedSessionWithSource(t, s, sid, sourcePath)

	longText := "user input " + strings.Repeat("x", defaults.ContentPreviewLimit+1024)
	longToolInput := "rg parser " + strings.Repeat("y", defaults.ContentPreviewLimit+1024)
	longToolResult := "parser.go:12 " + strings.Repeat("z", defaults.ContentPreviewLimit+1024)
	g1, blobs := buildBarrierGeneration(t, sid, "g-native", longText, longToolInput, longToolResult)
	if err := activateBarrierGeneration(t, s, g1, blobs); err != nil {
		t.Fatalf("activate G1: %v", err)
	}
	if err := os.Remove(sourcePath); err != nil {
		t.Fatalf("delete native source: %v", err)
	}

	payload, err := export.ExportSnapshotPayload(context.Background(), s, s, sid)
	if err != nil {
		t.Fatalf("export after native deletion: %v", err)
	}
	if len(payload.Turns) == 0 || payload.Turns[0].Content != longText {
		t.Fatalf("exported content after native deletion = %+v, want the full captured body", payload.Turns)
	}
	var call *schema.ToolCallDetail
	for i := range payload.Turns {
		for j := range payload.Turns[i].ToolCalls {
			if payload.Turns[i].ToolCalls[j].Arguments == longToolInput {
				call = &payload.Turns[i].ToolCalls[j]
			}
		}
	}
	if call == nil || call.Result != longToolResult {
		t.Fatalf("exported tool evidence after native deletion = %+v, want the full captured call and result", call)
	}
}
