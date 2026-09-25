package export_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/transcript"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/generation_detail_barrier.yaml
var generationDetailBarrierYAML []byte

//go:embed testdata/generation_detail_barrier.manifest.yaml
var generationDetailBarrierManifestYAML []byte

// generationBlobSpec describes one captured managed body. A literal is used
// verbatim; otherwise the body is prefix + fill repeated fillOverPreviewBy
// bytes past the display preview limit + suffix, so a case can prove a body
// longer than any bounded preview arrived whole.
type generationBlobSpec struct {
	Literal           string `yaml:"literal,omitempty"`
	Prefix            string `yaml:"prefix,omitempty"`
	Fill              string `yaml:"fill,omitempty"`
	FillOverPreviewBy *int   `yaml:"fillOverPreviewBy,omitempty"`
	Suffix            string `yaml:"suffix,omitempty"`
}

func (spec generationBlobSpec) resolve() string {
	if spec.Literal != "" {
		return spec.Literal
	}
	var builder strings.Builder
	builder.WriteString(spec.Prefix)
	if spec.Fill != "" && spec.FillOverPreviewBy != nil {
		builder.WriteString(strings.Repeat(spec.Fill, defaults.ContentPreviewLimit+*spec.FillOverPreviewBy))
	}
	builder.WriteString(spec.Suffix)
	return builder.String()
}

func (spec generationBlobSpec) validate(label string) error {
	if spec.Literal != "" {
		if spec.Prefix != "" || spec.Fill != "" || spec.FillOverPreviewBy != nil || spec.Suffix != "" {
			return fmt.Errorf("%s sets literal together with prefix/fill/suffix; use one form", label)
		}
		return nil
	}
	if spec.Fill == "" {
		if spec.FillOverPreviewBy != nil {
			return fmt.Errorf("%s sets fillOverPreviewBy without fill", label)
		}
		if spec.Prefix == "" && spec.Suffix == "" {
			return fmt.Errorf("%s is empty; give a literal or a prefix/suffix", label)
		}
		return nil
	}
	if spec.FillOverPreviewBy == nil {
		return fmt.Errorf("%s sets fill without fillOverPreviewBy", label)
	}
	if *spec.FillOverPreviewBy < 0 {
		return fmt.Errorf("%s fillOverPreviewBy is negative", label)
	}
	return nil
}

// barrierGenerationSpec is one named managed generation in a case.
type barrierGenerationSpec struct {
	ID            string             `yaml:"id"`
	Text          generationBlobSpec `yaml:"text"`
	ToolInput     generationBlobSpec `yaml:"toolInput"`
	ToolOutput    generationBlobSpec `yaml:"toolOutput"`
	IncludeResult bool               `yaml:"includeResult"`
}

func (spec barrierGenerationSpec) validate(label string) error {
	if spec.ID == "" {
		return fmt.Errorf("%s has no id", label)
	}
	for _, part := range []struct {
		name string
		blob generationBlobSpec
	}{{"text", spec.Text}, {"toolInput", spec.ToolInput}} {
		if err := part.blob.validate(label + " " + part.name); err != nil {
			return err
		}
	}
	if spec.IncludeResult {
		if err := spec.ToolOutput.validate(label + " toolOutput"); err != nil {
			return err
		}
		return nil
	}
	if spec.ToolOutput != (generationBlobSpec{}) {
		return fmt.Errorf("%s omits its result but still declares a toolOutput body", label)
	}
	return nil
}

// barrierCorruptionSpec describes the same-length on-disk corruption applied to
// one committed blob.
type barrierCorruptionSpec struct {
	Ref                 string `yaml:"ref"`
	ReplacementFill     string `yaml:"replacementFill"`
	ExpectErrorContains string `yaml:"expectErrorContains"`
}

// barrierExpectations selects the assertions a case makes. Operation-specific
// requirements are enforced when the corpus loads.
type barrierExpectations struct {
	HeldGenerationID   string `yaml:"heldGenerationId,omitempty"`
	HeldCallbackActive bool   `yaml:"heldCallbackActive,omitempty"`
	HeldResultMarker   string `yaml:"heldResultMarker,omitempty"`
	LaterResultRemoved bool   `yaml:"laterResultRemoved,omitempty"`
	RefuseExport       bool   `yaml:"refuseExport,omitempty"`
	KeepCapturedBytes  bool   `yaml:"keepCapturedBytes,omitempty"`
}

// barrierCase is one named corpus entry. Operation selects which real export
// flow the case runs: the contention-and-activation lock lifetime, the
// same-length on-disk corruption refusal, or the deleted-native-source read.
type barrierCase struct {
	Name         string                 `yaml:"name"`
	Operation    string                 `yaml:"operation"`
	SessionID    string                 `yaml:"sessionId"`
	Baseline     *barrierGenerationSpec `yaml:"baseline,omitempty"`
	Held         *barrierGenerationSpec `yaml:"held,omitempty"`
	Later        *barrierGenerationSpec `yaml:"later,omitempty"`
	Corruption   *barrierCorruptionSpec `yaml:"corruption,omitempty"`
	NativeSource bool                   `yaml:"nativeSource,omitempty"`
	Expectations barrierExpectations    `yaml:"expectations"`
}

func loadGenerationDetailBarrierCorpus(t *testing.T) []barrierCase {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(generationDetailBarrierYAML))
	decoder.KnownFields(true)
	var fixture struct {
		Cases []barrierCase `yaml:"cases"`
	}
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode generation detail barrier corpus: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("generation detail barrier corpus must contain exactly one YAML document: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(generationDetailBarrierManifestYAML, "generation detail barrier")
	if err != nil {
		t.Fatal(err)
	}
	present := make(map[string]bool, len(fixture.Cases))
	names := make([]string, 0, len(fixture.Cases))
	for _, fixtureCase := range fixture.Cases {
		if fixtureCase.Name == "" || present[fixtureCase.Name] {
			t.Fatalf("generation detail barrier case %q is missing or duplicated", fixtureCase.Name)
		}
		present[fixtureCase.Name] = true
		names = append(names, fixtureCase.Name)
		validateBarrierCase(t, fixtureCase)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "generation detail barrier"); err != nil {
		t.Fatal(err)
	}
	return fixture.Cases
}

// validateBarrierCase rejects a malformed corpus entry before any subtest runs
// a real store operation, so a fixture typo reads as a corpus failure rather
// than a mid-run assertion.
func validateBarrierCase(t *testing.T, fixtureCase barrierCase) {
	t.Helper()
	if _, err := schema.NewSessionID(fixtureCase.SessionID); err != nil {
		t.Fatalf("case %q has invalid sessionId: %v", fixtureCase.Name, err)
	}
	switch fixtureCase.Operation {
	case "lockLifetime":
		if fixtureCase.Baseline == nil || fixtureCase.Held == nil || fixtureCase.Later == nil {
			t.Fatalf("case %q operation %q must declare baseline, held and later generations", fixtureCase.Name, fixtureCase.Operation)
		}
		if fixtureCase.Corruption != nil || fixtureCase.NativeSource {
			t.Fatalf("case %q operation %q carries fields of another operation", fixtureCase.Name, fixtureCase.Operation)
		}
		if fixtureCase.Expectations.HeldGenerationID != fixtureCase.Held.ID {
			t.Fatalf("case %q heldGenerationId %q must name the held generation %q", fixtureCase.Name, fixtureCase.Expectations.HeldGenerationID, fixtureCase.Held.ID)
		}
		if !fixtureCase.Expectations.HeldCallbackActive {
			t.Fatalf("case %q operation %q must require the production callback active during hydration", fixtureCase.Name, fixtureCase.Operation)
		}
		if fixtureCase.Expectations.HeldResultMarker == "" || !strings.HasPrefix(fixtureCase.Held.ToolOutput.resolve(), fixtureCase.Expectations.HeldResultMarker) {
			t.Fatalf("case %q heldResultMarker %q must be a prefix of the held tool output", fixtureCase.Name, fixtureCase.Expectations.HeldResultMarker)
		}
		if !fixtureCase.Expectations.LaterResultRemoved {
			t.Fatalf("case %q operation %q must require the removed later result ref to stay removed", fixtureCase.Name, fixtureCase.Operation)
		}
	case "corruptArtifact":
		if fixtureCase.Held == nil || fixtureCase.Corruption == nil {
			t.Fatalf("case %q operation %q must declare a held generation and a corruption spec", fixtureCase.Name, fixtureCase.Operation)
		}
		if fixtureCase.Baseline != nil || fixtureCase.Later != nil || fixtureCase.NativeSource {
			t.Fatalf("case %q operation %q carries fields of another operation", fixtureCase.Name, fixtureCase.Operation)
		}
		if fixtureCase.Corruption.Ref == "" || len(fixtureCase.Corruption.ReplacementFill) != 1 || fixtureCase.Corruption.ExpectErrorContains == "" {
			t.Fatalf("case %q corruption must name a ref, a one-byte replacementFill and an expected error substring", fixtureCase.Name)
		}
		if !fixtureCase.Expectations.RefuseExport {
			t.Fatalf("case %q operation %q must require the export refused", fixtureCase.Name, fixtureCase.Operation)
		}
	case "nativeDeletion":
		if fixtureCase.Held == nil {
			t.Fatalf("case %q operation %q must declare a held generation", fixtureCase.Name, fixtureCase.Operation)
		}
		if fixtureCase.Baseline != nil || fixtureCase.Later != nil || fixtureCase.Corruption != nil {
			t.Fatalf("case %q operation %q carries fields of another operation", fixtureCase.Name, fixtureCase.Operation)
		}
		if !fixtureCase.NativeSource || !fixtureCase.Expectations.KeepCapturedBytes {
			t.Fatalf("case %q operation %q must declare nativeSource and keepCapturedBytes", fixtureCase.Name, fixtureCase.Operation)
		}
	default:
		t.Fatalf("case %q names unknown operation %q; expected lockLifetime, corruptArtifact or nativeDeletion", fixtureCase.Name, fixtureCase.Operation)
	}
	for _, generation := range []struct {
		label string
		spec  *barrierGenerationSpec
	}{
		{"baseline", fixtureCase.Baseline},
		{"held", fixtureCase.Held},
		{"later", fixtureCase.Later},
	} {
		if generation.spec == nil {
			continue
		}
		if err := generation.spec.validate(fmt.Sprintf("case %q %s", fixtureCase.Name, generation.label)); err != nil {
			t.Fatal(err)
		}
	}
}

func mustBarrierSessionID(t *testing.T, fixtureCase barrierCase) schema.SessionID {
	t.Helper()
	sid, err := schema.NewSessionID(fixtureCase.SessionID)
	if err != nil {
		t.Fatalf("case %q session id: %v", fixtureCase.Name, err)
	}
	return sid
}

// TestGenerationDetailBarrier drives every named corpus case through the real
// generation-capable store and the production export exit.
func TestGenerationDetailBarrier(t *testing.T) {
	for _, fixtureCase := range loadGenerationDetailBarrierCorpus(t) {
		t.Run(fixtureCase.Name, func(t *testing.T) {
			switch fixtureCase.Operation {
			case "lockLifetime":
				runBarrierLockLifetime(t, fixtureCase)
			case "corruptArtifact":
				runBarrierCorruptArtifact(t, fixtureCase)
			case "nativeDeletion":
				runBarrierNativeDeletion(t, fixtureCase)
			default:
				t.Fatalf("case %q names unknown operation %q", fixtureCase.Name, fixtureCase.Operation)
			}
		})
	}
}

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

// buildBarrierGeneration builds a self-contained managed candidate from a
// corpus generation spec: a user text record, an assistant reply carrying a
// folded tool call and optionally its result, with full bodies selected by the
// fixture. Content records carry only their refs; activation fills the managed
// path, byte length and integrity digest. Tool rows are depth-one children of
// the assistant turn, so folding attaches them instead of emitting a second
// owner for the same ref.
func buildBarrierGeneration(t *testing.T, sid schema.SessionID, spec barrierGenerationSpec) (indexformat.V2, map[schema.SourceEntryRef][]byte) {
	t.Helper()
	refs := []schema.SourceEntryRef{"e_u1", "e_a1", "e_call1", "e_result1"}
	callID := "call-barrier"
	parent := 1
	text := spec.Text.resolve()
	toolInput := spec.ToolInput.resolve()
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
	if spec.IncludeResult {
		toolOutput := spec.ToolOutput.resolve()
		entries = append(entries, schema.SessionEntry{SessionID: sid, EntryIndex: 3, Harness: defaults.HarnessClaudeCode, EntryType: schema.EntryTypeToolResult, Role: schema.RoleTool, Depth: 1, ParentIndex: &parent, ToolOutput: &toolOutput, ToolCallID: &callID, SourceEntryRef: refs[3]})
		content = append(content, indexformat.ContentRecord{Ref: refs[3]})
		blobs[refs[3]] = []byte(toolOutput)
	}
	inputCount := int64(1)
	generation := indexformat.Generation{
		ID:           spec.ID,
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
	_, err := s.ActivateGeneration(context.Background(), store.GenerationActivation{
		Generation:     v2,
		Blobs:          blobs,
		IndexerVersion: 1,
		IndexedAtMs:    1,
	})
	return err
}

func waitForExclusiveAttempt(t *testing.T, attempts <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-attempts:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s never attempted the exclusive session lock", label)
	}
}

// runBarrierLockLifetime drives the actual durable export boundary through the
// real OS-lock barrier. It pauses BEFORE any captured content is hydrated and
// asserts the production callback is still running at that instant, so a
// mutation that moves hydration OUTSIDE WithSessionSnapshot is caught: the
// shared lock would already be released and both the activation and the
// cleanup below would complete while the export is still materializing. G2
// overlaps one ref and removes another, so the held read must carry the full
// held bytes and the later fresh read must be wholly the later generation.
func runBarrierLockLifetime(t *testing.T, fixtureCase barrierCase) {
	t.Helper()
	sid := mustBarrierSessionID(t, fixtureCase)
	heldText := fixtureCase.Held.Text.resolve()
	heldToolInput := fixtureCase.Held.ToolInput.resolve()
	heldToolOutput := fixtureCase.Held.ToolOutput.resolve()
	laterText := fixtureCase.Later.Text.resolve()
	laterToolInput := fixtureCase.Later.ToolInput.resolve()

	attempts := make(chan struct{}, 8)
	s, _ := openBarrierGenerationStore(t, attempts)
	storetest.SeedSession(t, s, string(sid))

	// The baseline is committed and then superseded by the held generation, so
	// it is a real inactive generation that cleanup can remove while the
	// boundary holds the lock.
	baseline, baselineBlobs := buildBarrierGeneration(t, sid, *fixtureCase.Baseline)
	if err := activateBarrierGeneration(t, s, baseline, baselineBlobs); err != nil {
		t.Fatalf("activate baseline: %v", err)
	}
	held, heldBlobs := buildBarrierGeneration(t, sid, *fixtureCase.Held)
	if err := activateBarrierGeneration(t, s, held, heldBlobs); err != nil {
		t.Fatalf("activate held generation: %v", err)
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
	if observation.generationID != fixtureCase.Expectations.HeldGenerationID {
		t.Fatalf("content hydration began for generation %q, want %q", observation.generationID, fixtureCase.Expectations.HeldGenerationID)
	}
	if observation.callbackActive != fixtureCase.Expectations.HeldCallbackActive {
		t.Fatal("content hydration began OUTSIDE the snapshot callback; the shared lock no longer covers hydration, so a concurrent activation or cleanup could remove the bytes mid-read")
	}

	// Cleanup of the inactive baseline takes the exclusive lock, so it must wait
	// for the reader that is still hydrating inside the callback.
	cleanupDone := make(chan error, 1)
	go func() { cleanupDone <- s.CleanupInactiveGeneration(context.Background(), sid, fixtureCase.Baseline.ID) }()
	waitForExclusiveAttempt(t, attempts, "cleanup")

	// The later generation shares the user/assistant/call refs with the held
	// generation, changes their bodies, and removes the tool result ref entirely.
	later, laterBlobs := buildBarrierGeneration(t, sid, *fixtureCase.Later)
	activationDone := make(chan error, 1)
	go func() { activationDone <- activateBarrierGeneration(t, s, later, laterBlobs) }()
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

	// Exact held output: the boundary materialized the full captured generation.
	if len(materialized.payload.Turns) == 0 || materialized.payload.Turns[0].Content != heldText {
		t.Fatalf("exported held turn content = %+v, want the full held body", materialized.payload.Turns)
	}
	heldCall := findBarrierToolCall(materialized.payload, heldToolInput)
	if heldCall == nil {
		t.Fatal("exported held payload lost the full tool arguments")
	}
	if heldCall.Result != heldToolOutput {
		t.Fatalf("exported held tool result length = %d, want full %d bytes", len(heldCall.Result), len(heldToolOutput))
	}
	heldBytes, err := json.Marshal(materialized.payload)
	if err != nil {
		t.Fatalf("marshal held payload: %v", err)
	}
	if !bytes.Contains(heldBytes, []byte(fixtureCase.Expectations.HeldResultMarker)) {
		t.Fatal("exported held payload lost the captured tool result marker")
	}

	// Fresh read after activation sees wholly the later generation: the
	// overlapping refs carry the changed later bytes and the removed ref is
	// gone, never a partial mixture.
	laterBytes, laterPayload, err := transcript.BuildSnapshotDetailBytes(context.Background(), s, s, sid)
	if err != nil {
		t.Fatalf("later detail read: %v", err)
	}
	if len(laterPayload.Turns) == 0 || laterPayload.Turns[0].Content != laterText {
		t.Fatalf("later detail read content = %+v, want the later body", laterPayload.Turns)
	}
	laterCall := findBarrierToolCall(laterPayload, laterToolInput)
	if laterCall == nil {
		t.Fatal("later detail read lost the overlapping tool call ref with its later body")
	}
	if fixtureCase.Expectations.LaterResultRemoved && laterCall.Result != "" {
		t.Fatalf("later detail read carried a tool result for a removed ref: %q", laterCall.Result)
	}
	if bytes.Contains(laterBytes, []byte(fixtureCase.Expectations.HeldResultMarker)) {
		t.Fatal("later detail read leaked the removed held tool result ref's bytes")
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
func corruptCommittedBlob(t *testing.T, root string, sid schema.SessionID, generationID string, ref schema.SourceEntryRef, replacementFill string) {
	t.Helper()
	if len(replacementFill) != 1 {
		t.Fatalf("corruption replacement fill %q must be exactly one byte", replacementFill)
	}
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
	corrupted := bytes.Repeat([]byte(replacementFill), len(original))
	if bytes.Equal(corrupted, original) {
		t.Fatalf("fixture blob for ref %q is already %q; the corruption cannot be distinguished", ref, corrupted)
	}
	if err := os.WriteFile(blobPath, corrupted, 0o600); err != nil {
		t.Fatalf("corrupt committed blob for ref %q: %v", ref, err)
	}
}

// runBarrierCorruptArtifact proves the export exit refuses a committed
// generation whose managed blob is corrupt instead of emitting a truncated
// payload. It corrupts a same-length on-disk blob and exercises the export
// through the REAL resolver, so disabling the artifact digest verification
// would serve the altered bytes and pass the export: the test fails unless real
// integrity verification runs.
func runBarrierCorruptArtifact(t *testing.T, fixtureCase barrierCase) {
	t.Helper()
	sid := mustBarrierSessionID(t, fixtureCase)
	s, root := openBarrierGenerationStore(t, make(chan struct{}, 4))
	storetest.SeedSession(t, s, string(sid))
	held, blobs := buildBarrierGeneration(t, sid, *fixtureCase.Held)
	if err := activateBarrierGeneration(t, s, held, blobs); err != nil {
		t.Fatalf("activate held generation: %v", err)
	}
	corruptCommittedBlob(t, root, sid, fixtureCase.Held.ID, schema.SourceEntryRef(fixtureCase.Corruption.Ref), fixtureCase.Corruption.ReplacementFill)

	payload, err := export.ExportSnapshotPayload(context.Background(), s, s, sid)
	if err == nil {
		t.Fatalf("a same-length corrupt managed blob must fail the export; got payload %+v", payload)
	}
	if payload != nil {
		t.Fatalf("a corrupt managed blob emitted a partial payload: %+v", payload)
	}
	if !strings.Contains(err.Error(), fixtureCase.Corruption.ExpectErrorContains) {
		t.Fatalf("corrupt-artifact failure = %v, want it to contain %q", err, fixtureCase.Corruption.ExpectErrorContains)
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

// runBarrierNativeDeletion proves the committed managed generation is
// self-contained: once the recorded native source is deleted, the export exit
// still returns the exact full long bodies from the immutable managed blobs and
// never reparses the missing original.
func runBarrierNativeDeletion(t *testing.T, fixtureCase barrierCase) {
	t.Helper()
	sid := mustBarrierSessionID(t, fixtureCase)
	s, _ := openBarrierGenerationStore(t, make(chan struct{}, 4))
	sourcePath := filepath.Join(t.TempDir(), "native-session.jsonl")
	if err := os.WriteFile(sourcePath, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write native source: %v", err)
	}
	seedSessionWithSource(t, s, sid, sourcePath)

	held, blobs := buildBarrierGeneration(t, sid, *fixtureCase.Held)
	if err := activateBarrierGeneration(t, s, held, blobs); err != nil {
		t.Fatalf("activate held generation: %v", err)
	}
	if err := os.Remove(sourcePath); err != nil {
		t.Fatalf("delete native source: %v", err)
	}

	payload, err := export.ExportSnapshotPayload(context.Background(), s, s, sid)
	if err != nil {
		t.Fatalf("export after native deletion: %v", err)
	}
	heldText := fixtureCase.Held.Text.resolve()
	heldToolInput := fixtureCase.Held.ToolInput.resolve()
	heldToolOutput := fixtureCase.Held.ToolOutput.resolve()
	if len(payload.Turns) == 0 || payload.Turns[0].Content != heldText {
		t.Fatalf("exported content after native deletion = %+v, want the full captured body", payload.Turns)
	}
	call := findBarrierToolCall(payload, heldToolInput)
	if call == nil || call.Result != heldToolOutput {
		t.Fatalf("exported tool evidence after native deletion = %+v, want the full captured call and result", call)
	}
}
