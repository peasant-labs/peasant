package export_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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

// openBarrierGenerationStore opens a real generation-capable store with the
// production artifact and lock implementations behind the attempt barrier. It
// returns the owned-artifact root as well, so a test can reach the committed
// blob files it must corrupt.
func openBarrierGenerationStore(t *testing.T, attempts chan struct{}) (*store.Store, string) {
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
	return s, root
}

// drainExclusiveAttempts clears the setup activations' exclusive-lock signals
// so a later waitForExclusiveAttempt observes only the contended operation.
func drainExclusiveAttempts(attempts chan struct{}) {
	for {
		select {
		case <-attempts:
			continue
		default:
			return
		}
	}
}

// callbackLifetime records whether the production snapshot callback is still
// running. A test resolver reads it while it hydrates captured content: true
// proves hydration happens INSIDE the shared-lock callback, and a mutation that
// moves hydration outside the callback flips it false and fails the test.
type callbackLifetime struct {
	active atomic.Bool
}

func (c *callbackLifetime) enter()         { c.active.Store(true) }
func (c *callbackLifetime) exit()          { c.active.Store(false) }
func (c *callbackLifetime) isActive() bool { return c.active.Load() }

// lifetimeReader wraps the real snapshot reader and marks the callback active
// for exactly its lifetime, so the wrapped resolver can observe whether the
// production boundary is still inside the callback when it hydrates.
type lifetimeReader struct {
	inner indexformat.SnapshotReader
	state *callbackLifetime
}

func (r lifetimeReader) WithSessionSnapshot(ctx context.Context, id schema.SessionID, fn func(indexformat.ReadSnapshot) error) error {
	return r.inner.WithSessionSnapshot(ctx, id, func(snapshot indexformat.ReadSnapshot) error {
		r.state.enter()
		defer r.state.exit()
		return fn(snapshot)
	})
}

// hydrationObservation is what the gated resolver reports when real content
// resolution begins: which generation the boundary captured and whether the
// production callback was still running at that instant.
type hydrationObservation struct {
	generationID   string
	callbackActive bool
}

// hydrationGateResolver delegates to the real resolver but pauses the FIRST
// content read BEFORE any bytes are hydrated. Because the read is invoked from
// inside the production snapshot callback, the shared lock is held while the
// resolver blocks; a concurrent activation or cleanup must therefore wait.
type hydrationGateResolver struct {
	inner   indexformat.ContentResolver
	state   *callbackLifetime
	entered chan hydrationObservation
	release chan struct{}
	once    sync.Once
}

func (g *hydrationGateResolver) ReadFullContent(ctx context.Context, sessionID schema.SessionID, generationID string, record indexformat.ContentRecord) ([]byte, error) {
	g.once.Do(func() {
		g.entered <- hydrationObservation{generationID: generationID, callbackActive: g.state.isActive()}
		<-g.release
	})
	return g.inner.ReadFullContent(ctx, sessionID, generationID, record)
}

// buildBarrierGeneration builds a self-contained V2 candidate: a user text
// record, an assistant reply carrying a folded tool call and its result, with
// full bodies longer than the display preview limit. Content records carry only
// their refs; activation fills the managed path, byte length and integrity
// digest. Tool rows are depth-one children of the assistant turn, so folding
// attaches them instead of emitting a second owner for the same ref.
func buildBarrierGeneration(t *testing.T, sid schema.SessionID, genID, text, toolInput, toolOutput string) (indexformat.V2, map[schema.SourceEntryRef][]byte) {
	t.Helper()
	return buildBarrierGenerationWithResult(t, sid, genID, text, toolInput, toolOutput, true)
}

// buildBarrierGenerationWithoutResult builds the same candidate with the tool
// result ref REMOVED, so a later generation overlaps the user/assistant/call
// refs while dropping one ref the prior generation owned. A fresh read of the
// later generation must not carry the removed ref's bytes.
func buildBarrierGenerationWithoutResult(t *testing.T, sid schema.SessionID, genID, text, toolInput string) (indexformat.V2, map[schema.SourceEntryRef][]byte) {
	t.Helper()
	return buildBarrierGenerationWithResult(t, sid, genID, text, toolInput, "", false)
}

func buildBarrierGenerationWithResult(t *testing.T, sid schema.SessionID, genID, text, toolInput, toolOutput string, includeResult bool) (indexformat.V2, map[schema.SourceEntryRef][]byte) {
	t.Helper()
	refs := []schema.SourceEntryRef{"e_u1", "e_a1", "e_call1", "e_result1"}
	callID := "call-barrier"
	parent := 1
	entries := []schema.SessionEntry{
		{SessionID: sid, EntryIndex: 0, Harness: defaults.HarnessClaudeCode, EntryType: schema.EntryTypeText, Role: schema.RoleUser, ContentPreview: &text, SourceEntryRef: refs[0]},
		{SessionID: sid, EntryIndex: 1, Harness: defaults.HarnessClaudeCode, EntryType: schema.EntryTypeText, Role: schema.RoleAssistant, ContentPreview: &text, SourceEntryRef: refs[1]},
		{SessionID: sid, EntryIndex: 2, Harness: defaults.HarnessClaudeCode, EntryType: schema.EntryTypeToolUse, Role: schema.RoleAssistant, Depth: 1, ParentIndex: &parent, ToolInput: &toolInput, ToolCallID: &callID, SourceEntryRef: refs[2]},
	}
	content := []indexformat.ContentRecord{{Ref: refs[0]}, {Ref: refs[1]}, {Ref: refs[2]}}
	blobs := map[schema.SourceEntryRef][]byte{
		refs[0]: []byte(text),
		refs[1]: []byte(text),
		refs[2]: []byte(toolInput),
	}
	if includeResult {
		entries = append(entries, schema.SessionEntry{SessionID: sid, EntryIndex: 3, Harness: defaults.HarnessClaudeCode, EntryType: schema.EntryTypeToolResult, Role: schema.RoleTool, Depth: 1, ParentIndex: &parent, ToolOutput: &toolOutput, ToolCallID: &callID, SourceEntryRef: refs[3]})
		content = append(content, indexformat.ContentRecord{Ref: refs[3]})
		blobs[refs[3]] = []byte(toolOutput)
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
		Content:              content,
		Aliases:              []indexformat.NativeAlias{{NativeKey: "native-0", Ref: refs[0]}},
		SourceEvidenceDigest: strings.Repeat("a", 64),
		TitleRefs:            []schema.SourceEntryRef{refs[0]},
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

// TestExportBoundaryHydratesInsideSnapshotCallbackAcrossActivationAndCleanup
// drives the actual durable export boundary (export.ExportSnapshotPayload,
// which shares transcript.BuildSnapshotDetailBytes with the detail reads)
// through the real G1/G2 OS-lock barrier. It pauses BEFORE any captured content
// is hydrated and asserts the production callback is still running at that
// instant, so a mutation that moves hydration or serialization OUTSIDE
// WithSessionSnapshot is caught: the shared lock would already be released and
// both the activation and the cleanup below would complete while the export is
// still materializing. G2 overlaps one ref and removes another, so the G1 read
// must carry the full G1 bytes and the later fresh read must be wholly G2.
func TestExportBoundaryHydratesInsideSnapshotCallbackAcrossActivationAndCleanup(t *testing.T) {
	sid, err := schema.NewSessionID("45454545-4545-4545-4545-454545454548")
	if err != nil {
		t.Fatal(err)
	}
	attempts := make(chan struct{}, 8)
	s, _ := openBarrierGenerationStore(t, attempts)
	storetest.SeedSession(t, s, string(sid))

	longText := "user input " + strings.Repeat("x", defaults.ContentPreviewLimit+1024)
	longToolInput := "rg parser " + strings.Repeat("y", defaults.ContentPreviewLimit+1024)
	const g1ResultMarker = "g1-result-sentinel "
	longToolResult := g1ResultMarker + strings.Repeat("z", defaults.ContentPreviewLimit+1024)
	g1Text := longText + " G1"

	// G0 is committed and then superseded by G1, so it is a real inactive
	// generation that cleanup can remove while the boundary holds the lock.
	g0, g0Blobs := buildBarrierGeneration(t, sid, "g-export-0", "baseline", "baseline call", "baseline result")
	if err := activateBarrierGeneration(t, s, g0, g0Blobs); err != nil {
		t.Fatalf("activate G0: %v", err)
	}
	g1, g1Blobs := buildBarrierGeneration(t, sid, "g-export-1", g1Text, longToolInput, longToolResult)
	if err := activateBarrierGeneration(t, s, g1, g1Blobs); err != nil {
		t.Fatalf("activate G1: %v", err)
	}
	// The setup activations each took the exclusive lock; clear their signals so
	// the waits below observe only the contended activation and cleanup.
	drainExclusiveAttempts(attempts)

	lifetime := &callbackLifetime{}
	reader := lifetimeReader{inner: s, state: lifetime}
	gate := &hydrationGateResolver{
		inner:   s,
		state:   lifetime,
		entered: make(chan hydrationObservation, 1),
		release: make(chan struct{}),
	}
	type result struct {
		payload *schema.SessionDetailPayload
		err     error
	}
	exportDone := make(chan result, 1)
	go func() {
		payload, err := export.ExportSnapshotPayload(context.Background(), reader, gate, sid)
		exportDone <- result{payload: payload, err: err}
	}()

	var observation hydrationObservation
	select {
	case observation = <-gate.entered:
	case early := <-exportDone:
		t.Fatalf("export boundary returned before hydrating captured content: err=%v payload=%+v", early.err, early.payload)
	case <-time.After(5 * time.Second):
		t.Fatal("export boundary never began hydrating captured content")
	}
	if observation.generationID != "g-export-1" {
		t.Fatalf("content hydration began for generation %q, want G1", observation.generationID)
	}
	if !observation.callbackActive {
		t.Fatal("content hydration began OUTSIDE the snapshot callback; the shared lock no longer covers hydration, so a concurrent activation or cleanup could remove the bytes mid-read")
	}

	// Cleanup of the inactive G0 takes the exclusive lock, so it must wait for
	// the reader that is still hydrating inside the callback.
	cleanupDone := make(chan error, 1)
	go func() { cleanupDone <- s.CleanupInactiveGeneration(context.Background(), sid, "g-export-0") }()
	waitForExclusiveAttempt(t, attempts, "cleanup")

	// G2 shares the user/assistant/call refs with G1, changes their bodies, and
	// removes the tool result ref entirely.
	g2Text := longText + " G2"
	g2ToolInput := "rg parser g2 " + strings.Repeat("w", defaults.ContentPreviewLimit+1024)
	g2, g2Blobs := buildBarrierGenerationWithoutResult(t, sid, "g-export-2", g2Text, g2ToolInput)
	activationDone := make(chan error, 1)
	go func() { activationDone <- activateBarrierGeneration(t, s, g2, g2Blobs) }()
	waitForExclusiveAttempt(t, attempts, "activation")

	select {
	case err := <-cleanupDone:
		close(gate.release)
		t.Fatalf("cleanup removed the inactive generation while the export boundary still held the shared lock (err=%v)", err)
	case <-time.After(200 * time.Millisecond):
	}
	select {
	case err := <-activationDone:
		close(gate.release)
		t.Fatalf("activation completed while the export boundary still held the shared lock (err=%v)", err)
	case <-time.After(200 * time.Millisecond):
	}

	close(gate.release)
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
	select {
	case err := <-cleanupDone:
		if err != nil {
			t.Fatalf("cleanup after release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup did not complete after the export boundary released the shared lock")
	}

	// Exact G1 output: the boundary materialized the full captured generation.
	if len(materialized.payload.Turns) == 0 || materialized.payload.Turns[0].Content != g1Text {
		t.Fatalf("exported G1 turn content = %+v, want the full G1 body", materialized.payload.Turns)
	}
	g1Call := findBarrierToolCall(materialized.payload, longToolInput)
	if g1Call == nil {
		t.Fatal("exported G1 payload lost the full tool arguments")
	}
	if g1Call.Result != longToolResult {
		t.Fatalf("exported G1 tool result length = %d, want full %d bytes", len(g1Call.Result), len(longToolResult))
	}
	g1Bytes, err := json.Marshal(materialized.payload)
	if err != nil {
		t.Fatalf("marshal G1 payload: %v", err)
	}
	if !bytes.Contains(g1Bytes, []byte(g1ResultMarker)) {
		t.Fatal("exported G1 payload lost the captured tool result marker")
	}

	// Fresh read after activation sees wholly G2: the overlapping refs carry the
	// changed G2 bytes and the removed ref is gone, never a partial mixture.
	g2Bytes, g2Payload, err := transcript.BuildSnapshotDetailBytes(context.Background(), s, s, sid)
	if err != nil {
		t.Fatalf("G2 detail read: %v", err)
	}
	if len(g2Payload.Turns) == 0 || g2Payload.Turns[0].Content != g2Text {
		t.Fatalf("G2 detail read content = %+v, want the G2 body", g2Payload.Turns)
	}
	g2Call := findBarrierToolCall(g2Payload, g2ToolInput)
	if g2Call == nil {
		t.Fatal("G2 detail read lost the overlapping tool call ref with its G2 body")
	}
	if g2Call.Result != "" {
		t.Fatalf("G2 detail read carried a tool result for a removed ref: %q", g2Call.Result)
	}
	if bytes.Contains(g2Bytes, []byte(g1ResultMarker)) {
		t.Fatal("G2 detail read leaked the removed G1 tool result ref's bytes")
	}
}

func findBarrierToolCall(payload *schema.SessionDetailPayload, arguments string) *schema.ToolCallDetail {
	for i := range payload.Turns {
		for j := range payload.Turns[i].ToolCalls {
			if payload.Turns[i].ToolCalls[j].Arguments == arguments {
				return &payload.Turns[i].ToolCalls[j]
			}
		}
	}
	return nil
}

// corruptCommittedBlob overwrites one committed managed blob with the SAME
// number of bytes but different content. The byte length still matches the
// generation's content record, so only the sha256 integrity digest can catch
// the corruption; the corruption is real on-disk bytes, not a fabricated
// resolver error.
func corruptCommittedBlob(t *testing.T, root string, sid schema.SessionID, generationID string, ref schema.SourceEntryRef) {
	t.Helper()
	generationDir := filepath.Join(root, string(sid), "generations", generationID)
	manifestBytes, err := os.ReadFile(filepath.Join(generationDir, "manifest.json"))
	if err != nil {
		t.Fatalf("read committed manifest for generation %s: %v", generationID, err)
	}
	var generation indexformat.Generation
	if err := json.Unmarshal(manifestBytes, &generation); err != nil {
		t.Fatalf("decode committed manifest for generation %s: %v", generationID, err)
	}
	relativeBlob := ""
	for _, record := range generation.Content {
		if record.Ref == ref {
			relativeBlob = record.RelativeBlob
		}
	}
	if relativeBlob == "" {
		t.Fatalf("committed generation %s has no content record for ref %q", generationID, ref)
	}
	blobPath := filepath.Join(generationDir, relativeBlob)
	original, err := os.ReadFile(blobPath)
	if err != nil {
		t.Fatalf("read committed blob for ref %q: %v", ref, err)
	}
	// Same length, different bytes, still valid UTF-8, so the length check and
	// the UTF-8 guard both pass and only the integrity digest refuses the read.
	corrupted := bytes.Repeat([]byte("X"), len(original))
	if bytes.Equal(corrupted, original) {
		t.Fatalf("fixture blob for ref %q is already %q; the corruption cannot be distinguished", ref, corrupted)
	}
	if err := os.WriteFile(blobPath, corrupted, 0o600); err != nil {
		t.Fatalf("corrupt committed blob for ref %q: %v", ref, err)
	}
}

// TestExportBoundaryFailsCorruptArtifact proves the export exit refuses a
// committed generation whose managed blob is corrupt instead of emitting a
// truncated payload. It corrupts a same-length on-disk blob and exercises the
// export through the REAL resolver, so disabling the artifact digest
// verification would serve the altered bytes and pass the export: the test
// fails unless real integrity verification runs.
func TestExportBoundaryFailsCorruptArtifact(t *testing.T) {
	sid, err := schema.NewSessionID("45454545-4545-4545-4545-454545454549")
	if err != nil {
		t.Fatal(err)
	}
	s, root := openBarrierGenerationStore(t, make(chan struct{}, 4))
	storetest.SeedSession(t, s, string(sid))
	g1, blobs := buildBarrierGeneration(t, sid, "g-corrupt", "hello", "call", "the full tool result")
	if err := activateBarrierGeneration(t, s, g1, blobs); err != nil {
		t.Fatalf("activate G1: %v", err)
	}
	corruptCommittedBlob(t, root, sid, "g-corrupt", "e_result1")

	payload, err := export.ExportSnapshotPayload(context.Background(), s, s, sid)
	if err == nil {
		t.Fatalf("a same-length corrupt managed blob must fail the export; got payload %+v", payload)
	}
	if payload != nil {
		t.Fatalf("a corrupt managed blob emitted a partial payload: %+v", payload)
	}
	if !strings.Contains(err.Error(), "integrity digest") {
		t.Fatalf("corrupt-artifact failure = %v, want an actionable integrity-digest error", err)
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
	s, _ := openBarrierGenerationStore(t, make(chan struct{}, 4))
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
