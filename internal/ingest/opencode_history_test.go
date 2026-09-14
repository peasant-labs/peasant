package ingest_test

import (
	"bytes"
	_ "embed"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/opencode_history_projection.yaml
var openCodeHistoryYAML []byte

//go:embed testdata/opencode_history_projection.manifest.yaml
var openCodeHistoryManifestYAML []byte

type ocHistFork struct {
	SourceSessionID  string `yaml:"sourceSessionId"`
	BeforeSeq        *int64 `yaml:"beforeSeq"`
	ThroughSeq       *int64 `yaml:"throughSeq"`
	ThroughCompleted bool   `yaml:"throughCompleted"`
}

type ocHistRevert struct {
	TargetSeq int64 `yaml:"targetSeq"`
	Committed bool  `yaml:"committed"`
}

type ocHistRow struct {
	ID          string `yaml:"id"`
	SourceID    string `yaml:"sourceId"`
	Type        string `yaml:"type"`
	Seq         int64  `yaml:"seq"`
	TimeCreated int64  `yaml:"timeCreated"`
	TimeUpdated int64  `yaml:"timeUpdated"`
	Data        string `yaml:"data"`
}

type ocHistWant struct {
	ErrorContains string                `yaml:"errorContains"`
	Completeness  string                `yaml:"completeness"`
	Own           *int64                `yaml:"own"`
	TurnCount     int                   `yaml:"turnCount"`
	TitleRefs     []string              `yaml:"titleRefs"`
	SegmentRefs   [][]string            `yaml:"segmentRefs"`
	EarlierStates []string              `yaml:"earlierStates"`
	Main          []ocProvEntryExpect   `yaml:"main"`
	Earlier       [][]ocProvEntryExpect `yaml:"earlier"`
}

type ocHistStep struct {
	SessionID        string          `yaml:"sessionId"`
	ParentID         string          `yaml:"parentId"`
	HasParent        bool            `yaml:"hasParent"`
	ParentNullProven bool            `yaml:"parentNullProven"`
	Fork             *ocHistFork     `yaml:"fork"`
	Revert           *ocHistRevert   `yaml:"revert"`
	Own              []ocHistRow     `yaml:"own"`
	Copied           []ocHistRow     `yaml:"copied"`
	Allocator        ocProvAllocator `yaml:"allocator"`
	Want             ocHistWant      `yaml:"want"`
}

type ocHistCase struct {
	Name  string       `yaml:"name"`
	Steps []ocHistStep `yaml:"steps"`
}

type ocHistDocument struct {
	Cases []ocHistCase `yaml:"cases"`
}

func loadOpenCodeHistoryCases(t *testing.T) []ocHistCase {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(openCodeHistoryYAML))
	decoder.KnownFields(true)
	var document ocHistDocument
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("decode OpenCode history fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("OpenCode history fixture must contain exactly one YAML document: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(openCodeHistoryManifestYAML, "OpenCode history")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(document.Cases))
	for i, row := range document.Cases {
		names[i] = row.Name
		if len(row.Steps) == 0 {
			t.Fatalf("OpenCode history fixture case %q has no steps; a case without steps passes without invoking the materializer; declare at least one step", row.Name)
		}
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "OpenCode history"); err != nil {
		t.Fatal(err)
	}
	return document.Cases
}

func TestOpenCodeHistoryMaterialization(t *testing.T) {
	for _, historyCase := range loadOpenCodeHistoryCases(t) {
		t.Run(historyCase.Name, func(t *testing.T) {
			for stepIndex, step := range historyCase.Steps {
				label := historyCase.Name + "/" + itoa(stepIndex)
				snapshot := buildOpenCodeHistorySnapshot(t, step)
				capture, err := ingest.BuildOpenCodeProvenanceCapture(snapshot, "gen-"+label, schema.UnifiedMetadata{
					SchemaVersion: 1,
					SessionID:     schema.SessionID(step.SessionID),
					ModelHarness:  schema.HarnessOpenCode,
				}, ingest.OpenCodeProvenancePrior{Aliases: ingest.NewProjectionPriorState()})
				if step.Want.ErrorContains != "" {
					if err == nil || !strings.Contains(err.Error(), step.Want.ErrorContains) {
						t.Fatalf("%s BuildOpenCodeProvenanceCapture() = %v, want error containing %q", label, err, step.Want.ErrorContains)
					}
					continue
				}
				if err != nil {
					t.Fatalf("%s BuildOpenCodeProvenanceCapture(): %v", label, err)
				}
				allocator := &queueAllocator{entry: append([]string(nil), step.Allocator.Entries...), submission: append([]string(nil), step.Allocator.Submissions...)}
				built, err := ingest.BuildV2(capture, allocator)
				if err != nil {
					t.Fatalf("%s BuildV2: %v", label, err)
				}
				assertOpenCodeHistoryGeneration(t, label, built.Generation, step.Want)
			}
		})
	}
}

// buildOpenCodeHistorySnapshot assembles the typed snapshot the materializer
// consumes. Own rows decode with the child's scope; copied rows decode with
// their captured source identity, then receive the new child identity the copy
// proof records, exactly as the native fork read does.
func buildOpenCodeHistorySnapshot(t *testing.T, step ocHistStep) ingest.OpenCodeHistorySnapshot {
	t.Helper()
	snapshot := ingest.OpenCodeHistorySnapshot{
		SessionID:        step.SessionID,
		ParentID:         step.ParentID,
		HasParent:        step.HasParent,
		ParentNullProven: step.ParentNullProven,
	}
	if step.Fork != nil {
		snapshot.Fork = &ingest.OpenCodeForkProof{
			SourceSessionID:  step.Fork.SourceSessionID,
			BeforeSeq:        step.Fork.BeforeSeq,
			ThroughSeq:       step.Fork.ThroughSeq,
			ThroughCompleted: step.Fork.ThroughCompleted,
		}
	}
	if step.Revert != nil {
		snapshot.Revert = &ingest.OpenCodeRevertState{TargetSeq: step.Revert.TargetSeq, Committed: step.Revert.Committed}
	}
	scope := ingest.OpenCodeProvenanceScope{
		SessionID:        step.SessionID,
		Shape:            ingest.OpenCodeProvenanceCurrent,
		ParentNullProven: step.ParentNullProven,
		HasParent:        step.HasParent,
	}
	for _, row := range step.Own {
		snapshot.Messages = append(snapshot.Messages, decodeOpenCodeHistoryRow(t, row, row.ID, "", scope))
	}
	for _, row := range step.Copied {
		decoded := decodeOpenCodeHistoryRow(t, row, row.SourceID, step.Fork.SourceSessionID, scope)
		decoded.MessageID = row.ID
		decoded.Message.MessageID = row.ID
		snapshot.Copied = append(snapshot.Copied, decoded)
	}
	snapshot.SourceEvidenceDigest = ingest.DigestOpenCodeSnapshot(snapshot)
	return snapshot
}

func decodeOpenCodeHistoryRow(t *testing.T, row ocHistRow, decodeID, sourceSessionID string, scope ingest.OpenCodeProvenanceScope) ingest.OpenCodeHistoryRow {
	t.Helper()
	decoded, settled, err := ingest.DecodeOpenCodeProvenanceRow(ingest.OpenCodeProvenanceRow{
		ID:          decodeID,
		SessionID:   scope.SessionID,
		Type:        row.Type,
		TimeCreated: row.TimeCreated,
		TimeUpdated: row.TimeUpdated,
		Seq:         row.Seq,
		HasSeq:      true,
		Data:        row.Data,
	}, scope)
	if err != nil {
		t.Fatalf("row %q decode: %v", decodeID, err)
	}
	return ingest.OpenCodeHistoryRow{
		MessageID:       decodeID,
		Shape:           scope.Shape,
		Seq:             row.Seq,
		HasSeq:          true,
		NativeType:      row.Type,
		CreatedMs:       row.TimeCreated,
		UpdatedMs:       row.TimeUpdated,
		CompletedMs:     decoded.TimeCompleted,
		Settled:         settled,
		SourceMessageID: row.SourceID,
		SourceSessionID: sourceSessionID,
		Message:         decoded,
	}
}

func assertOpenCodeHistoryGeneration(t *testing.T, label string, generation indexformat.Generation, want ocHistWant) {
	t.Helper()
	if want.Completeness != "" && string(generation.Completeness) != want.Completeness {
		t.Fatalf("%s completeness = %q, want %q", label, generation.Completeness, want.Completeness)
	}
	assertOpenCodeCounts(t, generation, ocProvWant{Own: want.Own, TurnCount: want.TurnCount, TitleRefs: want.TitleRefs})
	assertOpenCodeEntries(t, generation.Main.Entries, want.Main)
	states := make([]string, len(generation.Earlier))
	for i := range generation.Earlier {
		states[i] = string(generation.Earlier[i].State)
	}
	if !equalStringSlices(states, want.EarlierStates) {
		t.Fatalf("%s earlierStates = %v, want %v", label, states, want.EarlierStates)
	}
	if len(generation.Earlier) != len(want.Earlier) {
		t.Fatalf("%s earlier sections = %d, want %d", label, len(generation.Earlier), len(want.Earlier))
	}
	for i := range want.Earlier {
		assertOpenCodeEntries(t, generation.Earlier[i].Content.Entries, want.Earlier[i])
	}
	if len(generation.Segments) != len(want.SegmentRefs) {
		t.Fatalf("%s segments = %d, want %d", label, len(generation.Segments), len(want.SegmentRefs))
	}
	for i := range want.SegmentRefs {
		got := make([]string, len(generation.Segments[i].CapturedRefs))
		for j, ref := range generation.Segments[i].CapturedRefs {
			got[j] = string(ref)
		}
		if !equalStringSlices(got, want.SegmentRefs[i]) {
			t.Fatalf("%s segment[%d] capturedRefs = %v, want %v", label, i, got, want.SegmentRefs[i])
		}
	}
	assertOpenCodeCapturedEvidenceLocal(t, label, generation)
}

// assertOpenCodeCapturedEvidenceLocal proves inherited context is retained
// locally as content and alias evidence, and is never also emitted as child
// main or earlier chat.
func assertOpenCodeCapturedEvidenceLocal(t *testing.T, label string, generation indexformat.Generation) {
	t.Helper()
	contentRefs := make(map[schema.SourceEntryRef]struct{}, len(generation.Content))
	for _, record := range generation.Content {
		contentRefs[record.Ref] = struct{}{}
	}
	aliasRefs := make(map[schema.SourceEntryRef]struct{}, len(generation.Aliases))
	for _, alias := range generation.Aliases {
		aliasRefs[alias.Ref] = struct{}{}
	}
	emitted := make(map[schema.SourceEntryRef]struct{}, len(generation.Main.Entries))
	for _, entry := range generation.Main.Entries {
		emitted[entry.SourceEntryRef] = struct{}{}
	}
	for i := range generation.Earlier {
		for _, entry := range generation.Earlier[i].Content.Entries {
			emitted[entry.SourceEntryRef] = struct{}{}
		}
	}
	for i := range generation.Segments {
		for _, ref := range generation.Segments[i].CapturedRefs {
			if _, ok := contentRefs[ref]; !ok {
				t.Fatalf("%s captured ref %q has no retained content record; inherited evidence was not preserved locally", label, ref)
			}
			if _, ok := aliasRefs[ref]; !ok {
				t.Fatalf("%s captured ref %q has no retained native alias; a reopen could not reuse the identity", label, ref)
			}
			if _, ok := emitted[ref]; ok {
				t.Fatalf("%s captured ref %q is also emitted as child chat; inherited context must stay local, not become main", label, ref)
			}
		}
	}
}
