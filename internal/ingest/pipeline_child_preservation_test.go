package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/pipeline_child_preservation.yaml
var pipelineChildPreservationYAML []byte

var requiredPipelineChildPreservationCases = map[string]bool{
	"changed_parent_preserves_filtered_child": true,
}

type pipelineChildPreservationDocument struct {
	Cases []pipelineChildPreservationCase `yaml:"cases"`
}

type pipelineChildPreservationCase struct {
	Name            string `yaml:"name"`
	ParentSessionID string `yaml:"parent_session_id"`
	ChildSessionID  string `yaml:"child_session_id"`
	InitialParent   string `yaml:"initial_parent"`
	UpdatedParent   string `yaml:"updated_parent"`
	Child           string `yaml:"child"`
}

func loadPipelineChildPreservationCases(t *testing.T) []pipelineChildPreservationCase {
	t.Helper()
	var document pipelineChildPreservationDocument
	decoder := yaml.NewDecoder(bytes.NewReader(pipelineChildPreservationYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("decode child preservation fixture: %v", err)
	}
	seen := make(map[string]bool, len(document.Cases))
	for _, testCase := range document.Cases {
		seen[testCase.Name] = true
		if _, err := ingest.NewSessionID(testCase.ParentSessionID); err != nil {
			t.Fatalf("fixture %q parent ID: %v", testCase.Name, err)
		}
		if _, err := ingest.NewSessionID(testCase.ChildSessionID); err != nil {
			t.Fatalf("fixture %q child ID: %v", testCase.Name, err)
		}
	}
	for required := range requiredPipelineChildPreservationCases {
		if !seen[required] {
			t.Fatalf("child preservation fixture missing required case %q", required)
		}
	}
	return document.Cases
}

func TestPipelineParentRefreshPreservesFilteredChildOutput(t *testing.T) {
	for _, testCase := range loadPipelineChildPreservationCases(t) {
		t.Run(testCase.Name, func(t *testing.T) {
			mfs := testutil.NewMemFS()
			parentSource := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testCase.ParentSessionID)
			childSource := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testCase.ChildSessionID)
			if err := mfs.WriteFile(parentSource, []byte(testCase.InitialParent), 0600); err != nil {
				t.Fatal(err)
			}
			if err := mfs.WriteFile(childSource, []byte(testCase.Child), 0600); err != nil {
				t.Fatal(err)
			}
			old := time.Now().Add(-2 * time.Hour)
			mfs.ModTimes[parentSource], mfs.ModTimes[childSource] = old, old
			parent := makeDiscoveredSession(t, testCase.ParentSessionID, parentSource, old)
			child := makeDiscoveredSession(t, testCase.ChildSessionID, childSource, old)
			parentID := parent.SessionID
			child.ParentUUID = &parentID
			metas := map[ingest.SessionID]*ingest.UnifiedMetadata{
				parent.SessionID: makeMinimalMeta(t, testCase.ParentSessionID),
				child.SessionID:  makeMinimalMeta(t, testCase.ChildSessionID),
			}
			run := func(sessions []ingest.DiscoveredSession) *ingest.PipelineResult {
				pipeline, err := ingest.NewPipeline(mfs, testutil.DefaultGitResolver(), map[ingest.Harness]ingest.AdapterFactory{
					ingest.HarnessClaudeCode: makeStubAdapter(sessions, metas),
				}, makePipelineConfig(testOutputDir))
				if err != nil {
					t.Fatal(err)
				}
				result, err := pipeline.Run(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				return result
			}
			if result := run([]ingest.DiscoveredSession{parent, child}); result.Summary.Errors != 0 {
				t.Fatalf("initial ingest: %+v", result)
			}

			root := expectedOutputBase(testOutputDir, testCase.ParentSessionID)
			childRoot := fmt.Sprintf("%s/%s/%s", root, defaults.DirSubagents.String(), testCase.ChildSessionID)
			childTranscript := fmt.Sprintf("%s/%s--transcript.jsonl", childRoot, testCase.ChildSessionID)
			childMetadata := fmt.Sprintf("%s/%s--metadata.json", childRoot, testCase.ChildSessionID)
			beforeTranscript, err := mfs.ReadFile(childTranscript)
			if err != nil {
				t.Fatal(err)
			}
			beforeMetadata, err := mfs.ReadFile(childMetadata)
			if err != nil {
				t.Fatal(err)
			}
			parentMetadata := fmt.Sprintf("%s/%s--metadata.json", root, testCase.ParentSessionID)
			parentMetadataBytes, err := mfs.ReadFile(parentMetadata)
			if err != nil {
				t.Fatal(err)
			}
			var storedParent ingest.UnifiedMetadata
			if err := json.Unmarshal(parentMetadataBytes, &storedParent); err != nil {
				t.Fatal(err)
			}
			backdated := time.Now().Add(-3 * time.Hour).UnixMilli()
			storedParent.Timestamp.Ingested = &backdated
			parentMetadataBytes, err = json.Marshal(storedParent)
			if err != nil {
				t.Fatal(err)
			}
			if err := mfs.WriteFile(parentMetadata, parentMetadataBytes, 0600); err != nil {
				t.Fatal(err)
			}

			if err := mfs.WriteFile(parentSource, []byte(testCase.UpdatedParent), 0600); err != nil {
				t.Fatal(err)
			}
			newer := time.Now().Add(-time.Hour)
			mfs.ModTimes[parentSource], parent.ModTime = newer, newer
			result := run([]ingest.DiscoveredSession{parent, child})
			if result.Summary.Updated != 1 || result.Summary.Unchanged != 1 || result.Summary.Errors != 0 {
				t.Fatalf("refresh summary = %+v, want one updated parent and one unchanged child", result.Summary)
			}
			assertFileBytes(t, mfs, childTranscript, beforeTranscript)
			assertFileBytes(t, mfs, childMetadata, beforeMetadata)
			assertFileBytes(t, mfs, fmt.Sprintf("%s/%s--transcript.jsonl", root, testCase.ParentSessionID), []byte(testCase.UpdatedParent))
			assertFileBytes(t, mfs, parentSource, []byte(testCase.UpdatedParent))
			assertFileBytes(t, mfs, childSource, []byte(testCase.Child))
		})
	}
}

func assertFileBytes(t *testing.T, filesystem ingest.FileSystem, path string, want []byte) {
	t.Helper()
	got, err := filesystem.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != string(want) {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}
