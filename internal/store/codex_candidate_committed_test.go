package store

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/codex_candidate_committed.yaml
var codexCandidateCommittedYAML []byte

//go:embed testdata/codex_candidate_committed.manifest.yaml
var codexCandidateCommittedManifestYAML []byte

type codexCommittedFixture struct {
	Session struct {
		ID      string `yaml:"id"`
		Harness string `yaml:"harness"`
	} `yaml:"session"`
	Generation struct {
		ID               string   `yaml:"id"`
		StableThreadID   string   `yaml:"stableThreadID"`
		Pointer          string   `yaml:"pointer"`
		PhysicalSourceID string   `yaml:"physicalSourceID"`
		HistoryMode      string   `yaml:"historyMode"`
		CopyBoundary     *int64   `yaml:"copyBoundary"`
		OwnershipProven  bool     `yaml:"ownershipProven"`
		Source           string   `yaml:"source"`
		EntryRefs        []string `yaml:"entryRefs"`
		SubmissionRefs   []string `yaml:"submissionRefs"`
	} `yaml:"generation"`
	Expected struct {
		MainRefs      []string `yaml:"mainRefs"`
		RetainedRefs  []string `yaml:"retainedRefs"`
		InheritedText string   `yaml:"inheritedText"`
		TurnCount     int      `yaml:"turnCount"`
		Title         string   `yaml:"title"`
	} `yaml:"expected"`
	Cases []struct {
		Name string `yaml:"name"`
	} `yaml:"cases"`
}

func loadCodexCommittedFixture(t *testing.T) codexCommittedFixture {
	t.Helper()
	var fixture codexCommittedFixture
	decoder := yaml.NewDecoder(bytes.NewReader(codexCandidateCommittedYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode codex_candidate_committed.yaml: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("codex_candidate_committed.yaml must contain exactly one document: %v", err)
	}
	manifest, err := decodeRecoveryRequiredNames(codexCandidateCommittedManifestYAML)
	if err != nil {
		t.Fatalf("decode codex_candidate_committed manifest: %v", err)
	}
	actual := make([]string, 0, len(fixture.Cases))
	for _, c := range fixture.Cases {
		actual = append(actual, c.Name)
	}
	if err := validateRecoveryRequiredNames(manifest, actual, "codex candidate committed detail"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// codexCommittedSource is a deterministic read-only Codex source double. The
// production capture and classifier are the code under test; this only supplies
// the sanitized native bytes and the selected authority.
type codexCommittedSource struct {
	authority ingest.CodexSourceAuthority
	source    []byte
}

func (s *codexCommittedSource) ResolveCodexAuthority(context.Context, ingest.DiscoveredSession) (ingest.CodexSourceAuthority, error) {
	return s.authority, nil
}

func (s *codexCommittedSource) ReadCodexSource(_ context.Context, pointer string) ([]byte, error) {
	if pointer != s.authority.CurrentPointer {
		return nil, fmt.Errorf("fixture has no bytes for pointer %q", pointer)
	}
	return s.source, nil
}

// codexCommittedAllocator returns the fixture's exact opaque refs in allocation
// order so the committed refs bind the expected durable identities.
type codexCommittedAllocator struct {
	entries     []string
	submissions []string
	entryIndex  int
	subIndex    int
}

func (a *codexCommittedAllocator) NewEntryRef() (schema.SourceEntryRef, error) {
	if a.entryIndex >= len(a.entries) {
		return "", fmt.Errorf("entry ref script exhausted after %d allocations", a.entryIndex)
	}
	ref := schema.SourceEntryRef(a.entries[a.entryIndex])
	a.entryIndex++
	return ref, nil
}

func (a *codexCommittedAllocator) NewSubmissionRef() (schema.SubmissionRef, error) {
	if a.subIndex >= len(a.submissions) {
		return "", fmt.Errorf("submission ref script exhausted after %d allocations", a.subIndex)
	}
	ref := schema.SubmissionRef(a.submissions[a.subIndex])
	a.subIndex++
	return ref, nil
}

// TestCodexCandidateCommittedDetailExcludesInherited drives the real Codex
// candidate through the production managed-generation activation and the
// committed export reader: inherited copied prefix content stays recoverable in
// the generation catalog while the committed detail rows, the turn count and
// the derived title carry only the surviving child work.
func TestCodexCandidateCommittedDetailExcludesInherited(t *testing.T) {
	fixture := loadCodexCommittedFixture(t)
	sessionID, err := schema.NewSessionID(fixture.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	source := []byte(fixture.Generation.Source)
	authority := ingest.CodexSourceAuthority{
		StableThreadID:          fixture.Generation.StableThreadID,
		Kind:                    ingest.CodexAuthorityDetachedFile,
		CurrentPointer:          fixture.Generation.Pointer,
		PhysicalSourceID:        fixture.Generation.PhysicalSourceID,
		HistoryMode:             []byte(fmt.Sprintf("%q", fixture.Generation.HistoryMode)),
		CopyBoundary:            fixture.Generation.CopyBoundary,
		OriginalOwnershipProven: fixture.Generation.OwnershipProven,
	}
	history, err := ingest.CaptureCodexHistory(context.Background(), &codexCommittedSource{authority: authority, source: source}, ingest.DiscoveredSession{
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
		Allocator:    &codexCommittedAllocator{entries: fixture.Generation.EntryRefs, submissions: fixture.Generation.SubmissionRefs},
		GenerationID: fixture.Generation.ID,
	})
	if err != nil {
		t.Fatalf("BuildCodexCandidate: %v", err)
	}
	for _, ref := range fixture.Expected.RetainedRefs {
		if candidate.Content[schema.SourceEntryRef(ref)] == nil {
			t.Fatalf("candidate dropped retained inherited content for ref %q", ref)
		}
	}

	dir := t.TempDir()
	s := openGenerationStoreAt(t, dir)
	defer func() { _ = s.Close() }()
	seedGenerationSession(t, s, fixture.Session.ID)

	if err := s.ActivateGeneration(context.Background(), GenerationActivation{
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
	s = openGenerationStoreAt(t, dir)

	ctx := context.Background()
	// The committed read is the reader the detail and export surfaces use. The
	// managed activation on this generation authority certifies a managed
	// generation rather than a strict full transcript capture, so the available
	// reader returns the committed projection.
	snapshot, err := s.ReadSessionAvailable(ctx, sessionID)
	if err != nil {
		t.Fatalf("ReadSessionAvailable: %v", err)
	}
	if snapshot.Detail == nil {
		t.Fatal("committed detail row is absent")
	}
	if snapshot.Detail.TurnCount != fixture.Expected.TurnCount {
		t.Errorf("committed detail turn count = %d, want %d", snapshot.Detail.TurnCount, fixture.Expected.TurnCount)
	}
	if snapshot.Detail.Title == nil || *snapshot.Detail.Title != fixture.Expected.Title {
		t.Errorf("committed detail title = %v, want %q", snapshot.Detail.Title, fixture.Expected.Title)
	}
	// The committed export/detail reader serves the canonical entry rows, and
	// none of them may carry copied parent content.
	for _, entry := range snapshot.Entries {
		if entry.ContentPreview != nil && strings.Contains(*entry.ContentPreview, fixture.Expected.InheritedText) {
			t.Errorf("committed detail leaked inherited content %q in the canonical entry rows", fixture.Expected.InheritedText)
		}
	}

	// The durable generation projection is the ref authority, and the snapshot
	// display map is filtered to emitted refs while the inherited record and its
	// immutable blob stay in the generation catalog.
	err = s.WithSessionSnapshot(ctx, sessionID, func(read indexformat.ReadSnapshot) error {
		committed := make([]string, 0, len(read.Main.Entries))
		for _, entry := range read.Main.Entries {
			committed = append(committed, string(entry.SourceEntryRef))
			if entry.ContentPreview != nil && strings.Contains(*entry.ContentPreview, fixture.Expected.InheritedText) {
				return fmt.Errorf("durable main entry %q leaked inherited content %q", entry.SourceEntryRef, fixture.Expected.InheritedText)
			}
		}
		if strings.Join(committed, ",") != strings.Join(fixture.Expected.MainRefs, ",") {
			return fmt.Errorf("durable main entry refs = %v, want %v", committed, fixture.Expected.MainRefs)
		}
		display := make(map[schema.SourceEntryRef]struct{}, len(read.Content))
		for _, record := range read.Content {
			display[record.Ref] = struct{}{}
		}
		for _, ref := range fixture.Expected.RetainedRefs {
			if _, ok := display[schema.SourceEntryRef(ref)]; ok {
				return fmt.Errorf("display content map carries inherited-only ref %q", ref)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithSessionSnapshot: %v", err)
	}
	retainedRef := schema.SourceEntryRef(fixture.Expected.RetainedRefs[0])
	record := catalogContentRecord(t, s, sessionID, fixture.Generation.ID, retainedRef)
	content, err := s.ReadFullContent(ctx, sessionID, fixture.Generation.ID, record)
	if err != nil {
		t.Fatalf("resolve retained inherited blob: %v", err)
	}
	if string(content) != fixture.Expected.InheritedText {
		t.Fatalf("retained inherited blob = %q, want %q", string(content), fixture.Expected.InheritedText)
	}
}
