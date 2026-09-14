package store

// Committed detail/export oracle for the exact pasted wrapper three ways
// corpus. The same native case the classifier corpus asserts at the candidate
// level is activated as a managed generation and read back through the
// committed detail/export reader and the durable snapshot: every literal
// wrapper-looking body survives with its role and stable ref, the injected
// instructions stay a system turn, the input submission count is measured, and
// the title is selected from the eligible user submission only.

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

//go:embed testdata/codex_candidate_committed_wrapper.yaml
var codexCommittedWrapperYAML []byte

//go:embed testdata/codex_candidate_committed_wrapper.manifest.yaml
var codexCommittedWrapperManifestYAML []byte

type codexCommittedWrapperEntry struct {
	Ref       string `yaml:"ref"`
	Role      string `yaml:"role"`
	EntryType string `yaml:"entryType"`
	Body      string `yaml:"body"`
}

type codexCommittedWrapperFixture struct {
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
		Main                 []codexCommittedWrapperEntry `yaml:"main"`
	} `yaml:"expected"`
	Cases []struct {
		Name string `yaml:"name"`
	} `yaml:"cases"`
}

func loadCodexCommittedWrapperFixture(t *testing.T) codexCommittedWrapperFixture {
	t.Helper()
	var fixture codexCommittedWrapperFixture
	decoder := yaml.NewDecoder(bytes.NewReader(codexCommittedWrapperYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode codex_candidate_committed_wrapper.yaml: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("codex_candidate_committed_wrapper.yaml must contain exactly one document: %v", err)
	}
	manifest, err := decodeRecoveryRequiredNames(codexCommittedWrapperManifestYAML)
	if err != nil {
		t.Fatalf("decode codex_candidate_committed_wrapper manifest: %v", err)
	}
	actual := make([]string, 0, len(fixture.Cases))
	for _, testCase := range fixture.Cases {
		actual = append(actual, testCase.Name)
	}
	if err := validateRecoveryRequiredNames(manifest, actual, "codex candidate committed wrapper detail"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// TestCodexCommittedWrapperDetailKeepsLiteralBodies drives the exact pasted
// wrapper three ways native case through the real Codex candidate build, the
// production managed-generation activation, and the committed detail/export
// read. The literal wrapper-looking text is never repaired: all three main
// bodies, roles and stable refs survive the committed read and the durable
// snapshot, the input submission count is measured as one, and the title is
// selected from the eligible user submission's literal body.
func TestCodexCommittedWrapperDetailKeepsLiteralBodies(t *testing.T) {
	fixture := loadCodexCommittedWrapperFixture(t)
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
	history, err := ingest.CaptureCodexHistory(context.Background(), &codexCommittedSource{
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
		Allocator:    &codexCommittedAllocator{entries: fixture.Generation.EntryRefs, submissions: fixture.Generation.SubmissionRefs},
		GenerationID: fixture.Generation.ID,
	})
	if err != nil {
		t.Fatalf("BuildCodexCandidate: %v", err)
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
		t.Errorf("committed detail title = %v, want the literal %q", snapshot.Detail.Title, fixture.Expected.Title)
	}
	if len(snapshot.Entries) != len(fixture.Expected.Main) {
		t.Fatalf("committed entries = %d, want %d", len(snapshot.Entries), len(fixture.Expected.Main))
	}
	for i, want := range fixture.Expected.Main {
		entry := snapshot.Entries[i]
		if string(entry.Role) != want.Role {
			t.Errorf("committed entry %d role = %q, want %q", i, entry.Role, want.Role)
		}
		if string(entry.EntryType) != want.EntryType {
			t.Errorf("committed entry %d entryType = %q, want %q", i, entry.EntryType, want.EntryType)
		}
		if entry.ContentPreview == nil || *entry.ContentPreview != want.Body {
			t.Errorf("committed entry %d body = %v, want the literal %q", i, entry.ContentPreview, want.Body)
		}
	}

	err = s.WithSessionSnapshot(ctx, sessionID, func(read indexformat.ReadSnapshot) error {
		if read.Metadata.Stats.TurnCount != fixture.Expected.TurnCount {
			return fmt.Errorf("durable snapshot turn count = %d, want %d", read.Metadata.Stats.TurnCount, fixture.Expected.TurnCount)
		}
		if fixture.Expected.InputSubmissionCount != nil {
			got := read.Metadata.Stats.InputSubmissionCount
			if got == nil || *got != *fixture.Expected.InputSubmissionCount {
				return fmt.Errorf("durable snapshot input submission count = %v, want %d", got, *fixture.Expected.InputSubmissionCount)
			}
		}
		titleRefs := make([]string, 0, len(read.TitleRefs))
		for _, ref := range read.TitleRefs {
			titleRefs = append(titleRefs, string(ref))
		}
		if strings.Join(titleRefs, ",") != strings.Join(fixture.Expected.TitleRefs, ",") {
			return fmt.Errorf("durable title refs = %v, want the eligible seed %v", titleRefs, fixture.Expected.TitleRefs)
		}
		mainRefs := make([]string, 0, len(read.Main.Entries))
		for _, entry := range read.Main.Entries {
			mainRefs = append(mainRefs, string(entry.SourceEntryRef))
		}
		if strings.Join(mainRefs, ",") != strings.Join(fixture.Expected.MainRefs, ",") {
			return fmt.Errorf("durable main refs = %v, want %v", mainRefs, fixture.Expected.MainRefs)
		}
		if len(read.Main.Entries) != len(fixture.Expected.Main) {
			return fmt.Errorf("durable main entries = %d, want %d", len(read.Main.Entries), len(fixture.Expected.Main))
		}
		for i, want := range fixture.Expected.Main {
			entry := read.Main.Entries[i]
			if string(entry.SourceEntryRef) != want.Ref {
				return fmt.Errorf("durable main entry %d ref = %q, want %q", i, entry.SourceEntryRef, want.Ref)
			}
			if string(entry.Role) != want.Role || string(entry.EntryType) != want.EntryType {
				return fmt.Errorf("durable main entry %d role/type = %q/%q, want %q/%q", i, entry.Role, entry.EntryType, want.Role, want.EntryType)
			}
			if entry.ContentPreview == nil || *entry.ContentPreview != want.Body {
				return fmt.Errorf("durable main entry %d body = %v, want the literal %q", i, entry.ContentPreview, want.Body)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithSessionSnapshot: %v", err)
	}
}
