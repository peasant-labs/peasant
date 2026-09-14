package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/opencode_provenance.yaml
var openCodeProvenanceYAML []byte

//go:embed testdata/opencode_provenance.manifest.yaml
var openCodeProvenanceManifestYAML []byte

type ocProvScope struct {
	SessionID        string `yaml:"sessionId"`
	Shape            string `yaml:"shape"`
	ParentNullProven bool   `yaml:"parentNullProven"`
	HasParent        bool   `yaml:"hasParent"`
}

type ocProvRow struct {
	ID          string `yaml:"id"`
	Type        string `yaml:"type"`
	Seq         int64  `yaml:"seq"`
	TimeCreated int64  `yaml:"timeCreated"`
	TimeUpdated int64  `yaml:"timeUpdated"`
	Data        string `yaml:"data"`
}

type ocProvAllocator struct {
	Entries     []string `yaml:"entries"`
	Submissions []string `yaml:"submissions"`
}

type ocProvEntryExpect struct {
	Ref         string `yaml:"ref"`
	Index       int    `yaml:"index"`
	Role        string `yaml:"role"`
	EntryType   string `yaml:"entryType"`
	Depth       int    `yaml:"depth"`
	ParentIndex *int   `yaml:"parentIndex"`
	Content     string `yaml:"content"`
	ToolInput   string `yaml:"toolInput"`
	ToolOutput  string `yaml:"toolOutput"`
	ToolCallID  string `yaml:"toolCallId"`
	Submission  string `yaml:"submission"`
	Origin      string `yaml:"origin"`
	Actor       string `yaml:"actor"`
	Delivery    string `yaml:"delivery"`
	Ownership   string `yaml:"ownership"`
	Evidence    string `yaml:"evidence"`
	Modality    string `yaml:"modality"`
}

type ocProvWant struct {
	Own          *int64              `yaml:"own"`
	TurnCount    int                 `yaml:"turnCount"`
	TitleRefs    []string            `yaml:"titleRefs"`
	SkippedRows  int                 `yaml:"skippedRows"`
	Completeness string              `yaml:"completeness"`
	Main         []ocProvEntryExpect `yaml:"main"`
}

type ocProvCase struct {
	Name         string          `yaml:"name"`
	Scope        ocProvScope     `yaml:"scope"`
	AgentDeliver bool            `yaml:"agentDelivered"`
	Allocator    ocProvAllocator `yaml:"allocator"`
	Rows         []ocProvRow     `yaml:"rows"`
	Want         ocProvWant      `yaml:"want"`
}

type ocProvHistoricalPart struct {
	ID   string `yaml:"id"`
	Data string `yaml:"data"`
}

type ocProvHistoricalMessage struct {
	ID    string                 `yaml:"id"`
	Data  string                 `yaml:"data"`
	Parts []ocProvHistoricalPart `yaml:"parts"`
}

type ocProvHistoricalCase struct {
	Name      string                    `yaml:"name"`
	SessionID string                    `yaml:"sessionId"`
	Allocator ocProvAllocator           `yaml:"allocator"`
	Messages  []ocProvHistoricalMessage `yaml:"messages"`
	Want      ocProvWant                `yaml:"want"`
}

type ocProvDocument struct {
	Cases          []ocProvCase           `yaml:"cases"`
	HistoricalCase []ocProvHistoricalCase `yaml:"historicalCases"`
}

func loadOpenCodeProvenanceDocument(t *testing.T) ocProvDocument {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(openCodeProvenanceYAML))
	decoder.KnownFields(true)
	var document ocProvDocument
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("decode OpenCode provenance fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("OpenCode provenance fixture must contain exactly one YAML document: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(openCodeProvenanceManifestYAML, "OpenCode provenance")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(document.Cases)+len(document.HistoricalCase))
	for _, row := range document.Cases {
		names = append(names, row.Name)
		if len(row.Rows) == 0 {
			t.Fatalf("OpenCode provenance fixture case %q has no rows; a case without native rows passes without invoking a decoder; declare at least one row", row.Name)
		}
	}
	for _, row := range document.HistoricalCase {
		names = append(names, row.Name)
		if len(row.Messages) == 0 {
			t.Fatalf("OpenCode provenance fixture historical case %q has no messages; a case without native messages passes without invoking a decoder; declare at least one message", row.Name)
		}
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "OpenCode provenance"); err != nil {
		t.Fatal(err)
	}
	return document
}

func loadOpenCodeProvenanceCases(t *testing.T) []ocProvCase {
	t.Helper()
	return loadOpenCodeProvenanceDocument(t).Cases
}

func TestOpenCodeProvenanceClassificationAndCounts(t *testing.T) {
	for _, row := range loadOpenCodeProvenanceCases(t) {
		t.Run(row.Name, func(t *testing.T) {
			blocks, skipped := decodeOpenCodeProvenanceBlocks(t, row)
			capture := ingest.ClassifiedCapture{
				ID:                   "gen-" + row.Name,
				SessionID:            ingest.SessionID(row.Scope.SessionID),
				Harness:              ingest.HarnessOpenCode,
				SourceEvidenceDigest: "synthetic-evidence-digest",
				Completeness:         indexformat.GenerationCompletenessComplete,
				Metadata: schema.UnifiedMetadata{
					SchemaVersion: 1,
					SessionID:     schema.SessionID(row.Scope.SessionID),
					ModelHarness:  schema.HarnessOpenCode,
				},
				Blocks: blocks,
			}
			allocator := &queueAllocator{entry: append([]string(nil), row.Allocator.Entries...), submission: append([]string(nil), row.Allocator.Submissions...)}
			built, err := ingest.BuildV2(capture, allocator)
			if err != nil {
				t.Fatalf("BuildV2: %v", err)
			}
			if skipped != row.Want.SkippedRows {
				t.Fatalf("skipped unfinished rows = %d, want %d", skipped, row.Want.SkippedRows)
			}
			assertOpenCodeCounts(t, built.Generation, row.Want)
			assertOpenCodeEntries(t, built.Generation.Main.Entries, row.Want.Main)
		})
	}
}

func TestOpenCodeHistoricalProvenanceThroughFiles(t *testing.T) {
	for _, row := range loadOpenCodeProvenanceDocument(t).HistoricalCase {
		t.Run(row.Name, func(t *testing.T) {
			root := t.TempDir()
			storage := filepath.Join(root, "storage")
			sessionDir := filepath.Join(storage, "session", "synthetic")
			if err := os.MkdirAll(sessionDir, 0o700); err != nil {
				t.Fatal(err)
			}
			sessionJSON := fmt.Sprintf(`{"id":%q,"time":{"created":1000}}`, row.SessionID)
			if err := os.WriteFile(filepath.Join(sessionDir, row.SessionID+".json"), []byte(sessionJSON), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, message := range row.Messages {
				msgDir := filepath.Join(storage, "message", row.SessionID)
				if err := os.MkdirAll(msgDir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(msgDir, message.ID+".json"), []byte(message.Data), 0o600); err != nil {
					t.Fatal(err)
				}
				if len(message.Parts) == 0 {
					continue
				}
				partDir := filepath.Join(storage, "part", message.ID)
				if err := os.MkdirAll(partDir, 0o700); err != nil {
					t.Fatal(err)
				}
				for _, part := range message.Parts {
					if err := os.WriteFile(filepath.Join(partDir, part.ID+".json"), []byte(part.Data), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			indexer := ingest.NewOpenCodeIndexer(&ingest.OSFileSystem{})
			session := ingest.DiscoveredSession{
				SessionID:        ingest.SessionID(row.SessionID),
				Harness:          ingest.HarnessOpenCode,
				SourcePath:       ingest.ResolvedPath(filepath.Join(sessionDir, row.SessionID+".json")),
				OriginalRoot:     ingest.ResolvedPath(root),
				TranscriptOrigin: ingest.TranscriptOriginFile,
			}
			blocks, err := indexer.OpenCodeHistoricalProvenanceBlocks(context.Background(), session)
			if err != nil {
				t.Fatalf("OpenCodeHistoricalProvenanceBlocks: %v", err)
			}
			capture := ingest.ClassifiedCapture{
				ID:                   "gen-" + row.Name,
				SessionID:            ingest.SessionID(row.SessionID),
				Harness:              ingest.HarnessOpenCode,
				SourceEvidenceDigest: "synthetic-evidence-digest",
				Completeness:         indexformat.GenerationCompletenessComplete,
				Metadata: schema.UnifiedMetadata{
					SchemaVersion: 1,
					SessionID:     schema.SessionID(row.SessionID),
					ModelHarness:  schema.HarnessOpenCode,
				},
				Blocks: blocks,
			}
			allocator := &queueAllocator{entry: append([]string(nil), row.Allocator.Entries...), submission: append([]string(nil), row.Allocator.Submissions...)}
			built, err := ingest.BuildV2(capture, allocator)
			if err != nil {
				t.Fatalf("BuildV2: %v", err)
			}
			assertOpenCodeCounts(t, built.Generation, row.Want)
			assertOpenCodeEntries(t, built.Generation.Main.Entries, row.Want.Main)
		})
	}
}

// decodeOpenCodeProvenanceBlocks drives each fixture row through the real
// provenance row decoder and classifier, applying the case's native delivery
// correlation. Unsettled rows are skipped exactly as the production snapshot
// reader skips them.
func decodeOpenCodeProvenanceBlocks(t *testing.T, row ocProvCase) ([]ingest.ClassifiedBlock, int) {
	t.Helper()
	scope := ingest.OpenCodeProvenanceScope{
		SessionID:        row.Scope.SessionID,
		Shape:            ingest.OpenCodeProvenanceShape(row.Scope.Shape),
		ParentNullProven: row.Scope.ParentNullProven,
		HasParent:        row.Scope.HasParent,
	}
	attribution := ingest.LocalOpenCodeAttribution()
	var blocks []ingest.ClassifiedBlock
	skipped := 0
	for _, native := range row.Rows {
		decoded, settled, err := ingest.DecodeOpenCodeProvenanceRow(ingest.OpenCodeProvenanceRow{
			ID:          native.ID,
			SessionID:   row.Scope.SessionID,
			Type:        native.Type,
			TimeCreated: native.TimeCreated,
			TimeUpdated: native.TimeUpdated,
			Seq:         native.Seq,
			HasSeq:      true,
			Data:        native.Data,
		}, scope)
		if err != nil {
			t.Fatalf("row %q decode: %v", native.ID, err)
		}
		if !settled {
			skipped++
			continue
		}
		decoded.AgentDelivered = row.AgentDeliver
		classified, err := ingest.ClassifyOpenCodeMessage(decoded, attribution)
		if err != nil {
			t.Fatalf("row %q classify: %v", native.ID, err)
		}
		blocks = append(blocks, classified...)
	}
	return blocks, skipped
}

func assertOpenCodeCounts(t *testing.T, generation indexformat.Generation, want ocProvWant) {
	t.Helper()
	if want.Own == nil {
		if generation.Metadata.Stats.InputSubmissionCount != nil {
			t.Fatalf("inputSubmissionCount = %d, want absent", *generation.Metadata.Stats.InputSubmissionCount)
		}
	} else if generation.Metadata.Stats.InputSubmissionCount == nil {
		t.Fatalf("inputSubmissionCount = absent, want %d", *want.Own)
	} else if *generation.Metadata.Stats.InputSubmissionCount != *want.Own {
		t.Fatalf("inputSubmissionCount = %d, want %d", *generation.Metadata.Stats.InputSubmissionCount, *want.Own)
	}
	if generation.Metadata.Stats.TurnCount != want.TurnCount {
		t.Fatalf("turnCount = %d, want %d", generation.Metadata.Stats.TurnCount, want.TurnCount)
	}
	gotTitle := make([]string, len(generation.TitleRefs))
	for i, ref := range generation.TitleRefs {
		gotTitle[i] = string(ref)
	}
	if !equalStringSlices(gotTitle, want.TitleRefs) {
		t.Fatalf("titleRefs = %v, want %v", gotTitle, want.TitleRefs)
	}
}

func assertOpenCodeEntries(t *testing.T, entries []schema.SessionEntry, want []ocProvEntryExpect) {
	t.Helper()
	if len(entries) != len(want) {
		t.Fatalf("main entries = %d, want %d (%+v)", len(entries), len(want), entries)
	}
	for i, expected := range want {
		entry := entries[i]
		label := "main[" + itoa(i) + "]"
		if string(entry.SourceEntryRef) != expected.Ref {
			t.Fatalf("%s ref = %q, want %q", label, entry.SourceEntryRef, expected.Ref)
		}
		if entry.EntryIndex != expected.Index {
			t.Fatalf("%s index = %d, want %d", label, entry.EntryIndex, expected.Index)
		}
		if string(entry.Role) != expected.Role {
			t.Fatalf("%s role = %q, want %q", label, entry.Role, expected.Role)
		}
		if string(entry.EntryType) != expected.EntryType {
			t.Fatalf("%s entryType = %q, want %q", label, entry.EntryType, expected.EntryType)
		}
		if entry.Depth != expected.Depth {
			t.Fatalf("%s depth = %d, want %d", label, entry.Depth, expected.Depth)
		}
		assertOptionalInt(t, label+" parentIndex", entry.ParentIndex, expected.ParentIndex)
		if expected.EntryType != "tool_use" && expected.EntryType != "tool_result" {
			if got := previewOrEmpty(entry.ContentPreview); got != expected.Content {
				t.Fatalf("%s content = %q, want %q", label, got, expected.Content)
			}
		}
		if got := previewOrEmpty(entry.ToolInput); got != expected.ToolInput {
			t.Fatalf("%s toolInput = %q, want %q", label, got, expected.ToolInput)
		}
		if got := previewOrEmpty(entry.ToolOutput); got != expected.ToolOutput {
			t.Fatalf("%s toolOutput = %q, want %q", label, got, expected.ToolOutput)
		}
		if got := previewOrEmpty(entry.ToolCallID); got != expected.ToolCallID {
			t.Fatalf("%s toolCallId = %q, want %q", label, got, expected.ToolCallID)
		}
		if entry.Provenance == nil {
			t.Fatalf("%s has no provenance, want %+v", label, expected)
		}
		provenance := entry.Provenance
		if string(provenance.SubmissionRef) != expected.Submission {
			t.Fatalf("%s submissionRef = %q, want %q", label, provenance.SubmissionRef, expected.Submission)
		}
		if string(provenance.Origin) != expected.Origin {
			t.Fatalf("%s origin = %q, want %q", label, provenance.Origin, expected.Origin)
		}
		if string(provenance.Actor) != expected.Actor {
			t.Fatalf("%s actor = %q, want %q", label, provenance.Actor, expected.Actor)
		}
		if string(provenance.Delivery) != expected.Delivery {
			t.Fatalf("%s delivery = %q, want %q", label, provenance.Delivery, expected.Delivery)
		}
		if string(provenance.Ownership) != expected.Ownership {
			t.Fatalf("%s ownership = %q, want %q", label, provenance.Ownership, expected.Ownership)
		}
		if string(provenance.Evidence) != expected.Evidence {
			t.Fatalf("%s evidence = %q, want %q", label, provenance.Evidence, expected.Evidence)
		}
		if string(provenance.InputModality) != expected.Modality {
			t.Fatalf("%s modality = %q, want %q", label, provenance.InputModality, expected.Modality)
		}
	}
}

func previewOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func assertOptionalInt(t *testing.T, label string, got, want *int) {
	t.Helper()
	if got == nil && want == nil {
		return
	}
	if got == nil || want == nil {
		t.Fatalf("%s = %v, want %v", label, got, want)
	}
	if *got != *want {
		t.Fatalf("%s = %d, want %d", label, *got, *want)
	}
}

func equalStringSlices(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
