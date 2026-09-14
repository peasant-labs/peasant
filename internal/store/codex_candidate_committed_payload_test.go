package store_test

// Committed hydrated/serialized payload oracle for the exact pasted wrapper
// three ways corpus. The same native case the classifier corpus asserts at the
// candidate level, and the committed store test asserts through the durable
// snapshot, is driven here through the REAL durable payload boundary: the store
// snapshot reader plus the managed content resolver hydrate and fold the
// generation, and the serialized detail bytes are asserted. Any change that
// blanks or skips the managed hydration now fails this oracle.

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

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

//go:embed testdata/codex_candidate_committed_wrapper.yaml
var codexCommittedPayloadYAML []byte

//go:embed testdata/codex_candidate_committed_wrapper.manifest.yaml
var codexCommittedPayloadManifestYAML []byte

type codexCommittedPayloadEntry struct {
	Ref       string `yaml:"ref"`
	Role      string `yaml:"role"`
	EntryType string `yaml:"entryType"`
	Body      string `yaml:"body"`
}

type codexCommittedPayloadFixture struct {
	Session struct {
		ID      string `yaml:"id"`
		Harness string `yaml:"harness"`
	} `yaml:"session"`
	Generation struct {
		ID               string   `yaml:"id"`
		StableThreadID   string   `yaml:"stableThreadID"`
		Pointer          string   `yaml:"pointer"`
		PhysicalSourceID string   `yaml:"physicalSourceID"`
		Source           string   `yaml:"source"`
		EntryRefs        []string `yaml:"entryRefs"`
		SubmissionRefs   []string `yaml:"submissionRefs"`
	} `yaml:"generation"`
	Expected struct {
		TurnCount            int                          `yaml:"turnCount"`
		InputSubmissionCount *int64                       `yaml:"inputSubmissionCount"`
		Title                string                       `yaml:"title"`
		TitleRefs            []string                     `yaml:"titleRefs"`
		MainRefs             []string                     `yaml:"mainRefs"`
		Main                 []codexCommittedPayloadEntry `yaml:"main"`
	} `yaml:"expected"`
	Cases []struct {
		Name string `yaml:"name"`
	} `yaml:"cases"`
}

func loadCodexCommittedPayloadFixture(t *testing.T) codexCommittedPayloadFixture {
	t.Helper()
	var fixture codexCommittedPayloadFixture
	decoder := yaml.NewDecoder(bytes.NewReader(codexCommittedPayloadYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode codex_candidate_committed_wrapper.yaml: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("codex_candidate_committed_wrapper.yaml must contain exactly one document: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(codexCommittedPayloadManifestYAML, "codex committed payload")
	if err != nil {
		t.Fatalf("decode codex_candidate_committed_wrapper manifest: %v", err)
	}
	actual := make([]string, 0, len(fixture.Cases))
	for _, testCase := range fixture.Cases {
		actual = append(actual, testCase.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, actual, "codex committed payload"); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Expected.Main) == 0 || len(fixture.Expected.Main) != len(fixture.Expected.MainRefs) {
		t.Fatalf("fixture main entries %d disagree with main refs %d", len(fixture.Expected.Main), len(fixture.Expected.MainRefs))
	}
	return fixture
}

// codexCommittedPayloadSource is a deterministic read-only Codex source double.
// The production capture and classifier are the code under test; this only
// supplies the sanitized native bytes and the selected authority.
type codexCommittedPayloadSource struct {
	authority ingest.CodexSourceAuthority
	source    []byte
}

func (s *codexCommittedPayloadSource) ResolveCodexAuthority(context.Context, ingest.DiscoveredSession) (ingest.CodexSourceAuthority, error) {
	return s.authority, nil
}

func (s *codexCommittedPayloadSource) ReadCodexSource(_ context.Context, pointer string) ([]byte, error) {
	if pointer != s.authority.CurrentPointer {
		return nil, fmt.Errorf("fixture has no bytes for pointer %q", pointer)
	}
	return s.source, nil
}

// codexCommittedPayloadAllocator returns the fixture's exact opaque refs in
// allocation order so the committed refs bind the expected durable identities.
type codexCommittedPayloadAllocator struct {
	entries     []string
	submissions []string
	entryIndex  int
	subIndex    int
}

func (a *codexCommittedPayloadAllocator) NewEntryRef() (schema.SourceEntryRef, error) {
	if a.entryIndex >= len(a.entries) {
		return "", fmt.Errorf("entry ref script exhausted after %d allocations", a.entryIndex)
	}
	ref := schema.SourceEntryRef(a.entries[a.entryIndex])
	a.entryIndex++
	return ref, nil
}

func (a *codexCommittedPayloadAllocator) NewSubmissionRef() (schema.SubmissionRef, error) {
	if a.subIndex >= len(a.submissions) {
		return "", fmt.Errorf("submission ref script exhausted after %d allocations", a.subIndex)
	}
	ref := schema.SubmissionRef(a.submissions[a.subIndex])
	a.subIndex++
	return ref, nil
}

// codexCommittedPayloadCountingResolver records every managed content read and
// delegates to the real store resolver. A bypassed hydration makes no read and
// an empty read set fails the oracle, independently of content equality.
type codexCommittedPayloadCountingResolver struct {
	inner indexformat.ContentResolver
	reads []schema.SourceEntryRef
}

func (r *codexCommittedPayloadCountingResolver) ReadFullContent(ctx context.Context, id schema.SessionID, generationID string, record indexformat.ContentRecord) ([]byte, error) {
	r.reads = append(r.reads, record.Ref)
	return r.inner.ReadFullContent(ctx, id, generationID, record)
}

var _ indexformat.ContentResolver = (*codexCommittedPayloadCountingResolver)(nil)

// openCodexCommittedPayloadStore opens the real generation-capable store with
// the production artifact file store and session lock.
func openCodexCommittedPayloadStore(t *testing.T, dir string) *store.Store {
	t.Helper()
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
		store.WithGenerationArtifacts(artifacts, locker),
	)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func assertCodexCommittedPayload(t *testing.T, label string, payload *schema.SessionDetailPayload, fixture codexCommittedPayloadFixture) {
	t.Helper()
	if payload.TurnCount != fixture.Expected.TurnCount {
		t.Errorf("%s turn count = %d, want %d", label, payload.TurnCount, fixture.Expected.TurnCount)
	}
	if len(payload.Turns) != len(fixture.Expected.Main) {
		t.Fatalf("%s turns = %d, want %d", label, len(payload.Turns), len(fixture.Expected.Main))
	}
	for i, want := range fixture.Expected.Main {
		turn := payload.Turns[i]
		if string(turn.SourceEntryRef) != want.Ref {
			t.Errorf("%s turn %d ref = %q, want %q", label, i, turn.SourceEntryRef, want.Ref)
		}
		if string(turn.Role) != want.Role {
			t.Errorf("%s turn %d role = %q, want %q", label, i, turn.Role, want.Role)
		}
		if string(turn.EntryType) != want.EntryType {
			t.Errorf("%s turn %d entry type = %q, want %q", label, i, turn.EntryType, want.EntryType)
		}
		if turn.Content != want.Body {
			t.Errorf("%s turn %d body = %q, want the literal %q", label, i, turn.Content, want.Body)
		}
	}
	if fixture.Expected.InputSubmissionCount == nil {
		t.Fatal("fixture declares no expected input submission count")
	}
	if payload.InputSubmissionCount == nil || *payload.InputSubmissionCount != *fixture.Expected.InputSubmissionCount {
		t.Errorf("%s input submission count = %v, want %d", label, payload.InputSubmissionCount, *fixture.Expected.InputSubmissionCount)
	}
	// The eligible title seed is the first main owned block, so the serialized
	// payload must carry its exact ref and literal body.
	if len(fixture.Expected.TitleRefs) == 0 {
		t.Fatal("fixture declares no title refs")
	}
	first := payload.Turns[0]
	if string(first.SourceEntryRef) != fixture.Expected.TitleRefs[0] {
		t.Errorf("%s title seed ref = %q, want %q", label, first.SourceEntryRef, fixture.Expected.TitleRefs[0])
	}
	if first.Content != fixture.Expected.Title {
		t.Errorf("%s title seed body = %q, want the literal %q", label, first.Content, fixture.Expected.Title)
	}
}

// TestCodexCommittedWrapperPayloadKeepsLiteralBodies drives the exact pasted
// wrapper three ways native case through the real Codex candidate build, the
// production managed-generation activation, and the durable detail/export
// payload construction. The serialized bytes must carry every literal body,
// role, entry type and ref, the measured turn and input submission counts, and
// the literal title seed, and the resolver must actually hydrate each main
// content ref.
func TestCodexCommittedWrapperPayloadKeepsLiteralBodies(t *testing.T) {
	fixture := loadCodexCommittedPayloadFixture(t)
	sessionID, err := schema.NewSessionID(fixture.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	authority := ingest.CodexSourceAuthority{
		StableThreadID:   fixture.Generation.StableThreadID,
		Kind:             ingest.CodexAuthorityDetachedFile,
		CurrentPointer:   fixture.Generation.Pointer,
		PhysicalSourceID: fixture.Generation.PhysicalSourceID,
	}
	history, err := ingest.CaptureCodexHistory(context.Background(), &codexCommittedPayloadSource{
		authority: authority,
		source:    []byte(fixture.Generation.Source),
	}, ingest.DiscoveredSession{
		SessionID:  schema.SessionID(fixture.Generation.StableThreadID),
		Harness:    ingest.HarnessCodex,
		SourcePath: ingest.ResolvedPath(fixture.Generation.Pointer),
	}, nil)
	if err != nil {
		t.Fatalf("CaptureCodexHistory: %v", err)
	}
	base := schema.NewUnifiedMetadata()
	base.SessionID = schema.SessionID(fixture.Generation.StableThreadID)
	base.ModelHarness = ingest.HarnessCodex
	candidate, err := ingest.BuildCodexCandidate(ingest.CodexCandidateInput{
		History:      history,
		Base:         base,
		PriorState:   ingest.NewProjectionPriorState(),
		Allocator:    &codexCommittedPayloadAllocator{entries: fixture.Generation.EntryRefs, submissions: fixture.Generation.SubmissionRefs},
		GenerationID: fixture.Generation.ID,
	})
	if err != nil {
		t.Fatalf("BuildCodexCandidate: %v", err)
	}

	dir := t.TempDir()
	s := openCodexCommittedPayloadStore(t, dir)
	storetest.SeedSession(t, s, fixture.Session.ID)
	if err := s.ActivateGeneration(context.Background(), store.GenerationActivation{
		Generation:     candidate.V2,
		Blobs:          candidate.Content,
		IndexerVersion: 1,
		IndexedAtMs:    1,
	}); err != nil {
		t.Fatalf("ActivateGeneration: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close store before reopen: %v", err)
	}
	s = openCodexCommittedPayloadStore(t, dir)
	defer func() { _ = s.Close() }()

	ctx := context.Background()
	resolver := &codexCommittedPayloadCountingResolver{inner: s}
	owned, payload, err := transcript.BuildSnapshotDetailBytes(ctx, s, resolver, sessionID)
	if err != nil {
		t.Fatalf("BuildSnapshotDetailBytes: %v", err)
	}
	if len(owned) == 0 {
		t.Fatal("BuildSnapshotDetailBytes returned no serialized bytes")
	}
	var serialized schema.SessionDetailPayload
	if err := json.Unmarshal(owned, &serialized); err != nil {
		t.Fatalf("unmarshal serialized detail: %v", err)
	}
	assertCodexCommittedPayload(t, "serialized detail", &serialized, fixture)
	// The returned payload is the same validated value that was serialized.
	if payload == nil || payload.TurnCount != serialized.TurnCount {
		t.Fatalf("returned payload turn count = %v, want %d", payload, serialized.TurnCount)
	}
	readRefs := make([]string, 0, len(resolver.reads))
	for _, ref := range resolver.reads {
		readRefs = append(readRefs, string(ref))
	}
	if strings.Join(readRefs, ",") != strings.Join(fixture.Expected.MainRefs, ",") {
		t.Fatalf("managed hydration reads = %v, want every main ref %v", readRefs, fixture.Expected.MainRefs)
	}

	// The export exit shares the same payload construction boundary.
	exported, err := export.ExportSession(ctx, s, nil, fixture.Session.ID)
	if err != nil {
		t.Fatalf("ExportSession: %v", err)
	}
	assertCodexCommittedPayload(t, "exported detail", exported, fixture)

	// The committed detail title and the durable title refs are the same corpus
	// evidence carried beside the serialized payload.
	snapshot, err := s.ReadSessionAvailable(ctx, sessionID)
	if err != nil {
		t.Fatalf("ReadSessionAvailable: %v", err)
	}
	if snapshot.Detail == nil || snapshot.Detail.Title == nil || *snapshot.Detail.Title != fixture.Expected.Title {
		t.Fatalf("committed detail title = %v, want the literal %q", snapshot.Detail, fixture.Expected.Title)
	}
	err = s.WithSessionSnapshot(ctx, sessionID, func(read indexformat.ReadSnapshot) error {
		titleRefs := make([]string, 0, len(read.TitleRefs))
		for _, ref := range read.TitleRefs {
			titleRefs = append(titleRefs, string(ref))
		}
		if strings.Join(titleRefs, ",") != strings.Join(fixture.Expected.TitleRefs, ",") {
			return fmt.Errorf("durable title refs = %v, want the eligible seed %v", titleRefs, fixture.Expected.TitleRefs)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithSessionSnapshot: %v", err)
	}
}
