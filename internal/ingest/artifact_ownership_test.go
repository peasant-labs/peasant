package ingest_test

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/artifact_ownership.yaml
var artifactOwnershipYAML []byte

func TestPipelineParentPublicationPreservesUnownedFiles(t *testing.T) {
	t.Parallel()
	var fixture struct {
		RequiredNames    []string `yaml:"requiredNames"`
		ParentID         string   `yaml:"parentID"`
		ChildID          string   `yaml:"childID"`
		HostSlug         string   `yaml:"hostSlug"`
		ProjectHash      string   `yaml:"projectHash"`
		Transcript       string   `yaml:"transcript"`
		UnrelatedFile    string   `yaml:"unrelatedFile"`
		UnrelatedContent string   `yaml:"unrelatedContent"`
		Cases            []struct {
			Name        string `yaml:"name"`
			ChildSchema int    `yaml:"childSchema"`
			Force       bool   `yaml:"force"`
			HideChild   bool   `yaml:"hideChild"`
		} `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(artifactOwnershipYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatal("artifact ownership fixture requires one YAML document")
	}
	required := []string{"future-child-survives-parent-force", "retained-child-survives-parent-update"}
	if !reflect.DeepEqual(required, fixture.RequiredNames) {
		t.Fatal("artifact ownership required-name manifest changed")
	}
	seen := make(map[string]bool)
	for _, row := range fixture.Cases {
		if row.Name == "" || seen[row.Name] {
			t.Fatalf("invalid ownership case %q", row.Name)
		}
		seen[row.Name] = true
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			output := filepath.Join(root, "managed")
			native := filepath.Join(root, "native")
			if err := os.MkdirAll(native, 0700); err != nil {
				t.Fatal(err)
			}
			parentPath, childPath := filepath.Join(native, fixture.ParentID+".jsonl"), filepath.Join(native, fixture.ChildID+".jsonl")
			if err := os.WriteFile(parentPath, []byte(fixture.Transcript), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(childPath, []byte(fixture.Transcript), 0600); err != nil {
				t.Fatal(err)
			}
			old := time.Now().Add(-2 * time.Hour)
			parent := makeDiscoveredSession(t, fixture.ParentID, parentPath, old)
			child := makeDiscoveredSession(t, fixture.ChildID, childPath, old)
			child.ParentUUID = &parent.SessionID
			parentMeta := makeReindexMeta(t, fixture.ParentID, parentPath)
			parentMeta.HostSlug = ingest.HostSlug(fixture.HostSlug)
			parentMeta.Project.Hash = ingest.ProjectHash(fixture.ProjectHash)
			childMeta := makeReindexMeta(t, fixture.ChildID, childPath)
			childMeta.HostSlug, childMeta.Project.Hash = parentMeta.HostSlug, parentMeta.Project.Hash
			childMeta.ParentUUID = &parent.SessionID
			adapter := &testutil.StubAdapter{ProviderValue: ingest.HarnessClaudeCode, Sessions: []ingest.DiscoveredSession{parent, child}, Metadata: map[ingest.SessionID]*ingest.UnifiedMetadata{parent.SessionID: parentMeta, child.SessionID: childMeta}}
			registry := map[ingest.Harness]ingest.AdapterFactory{ingest.HarnessClaudeCode: func(ingest.FileSystem, ingest.GitResolver, salt.Salt) ingest.SourceAdapter { return adapter }}
			config := makePipelineConfig(output)
			config.StalenessThreshold = 0
			filesystem := &ingest.OSFileSystem{}
			pipeline, err := ingest.NewPipeline(filesystem, testutil.DefaultGitResolver(), registry, config)
			if err != nil {
				t.Fatal(err)
			}
			first, err := pipeline.Run(t.Context())
			if err != nil || first.Summary.New != 2 || first.Summary.Errors != 0 {
				t.Fatalf("seed parent/child: %+v %v", first, err)
			}
			childMetadataPath := ingest.SessionMetadataPath(output, fixture.HostSlug, fixture.ChildID, fixture.ParentID)
			childData, err := os.ReadFile(childMetadataPath)
			if err != nil {
				t.Fatal(err)
			}
			var retained ingest.UnifiedMetadata
			if err := json.Unmarshal(childData, &retained); err != nil {
				t.Fatal(err)
			}
			retained.SchemaVersion = row.ChildSchema
			retained.MetadataHash = schema.ComputeMetadataHash(&retained)
			childData, err = json.Marshal(retained)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(childMetadataPath, childData, 0600); err != nil {
				t.Fatal(err)
			}
			childTranscriptPath := filepath.Join(filepath.Dir(childMetadataPath), fixture.ChildID+"--transcript.jsonl")
			childTranscript, err := os.ReadFile(childTranscriptPath)
			if err != nil {
				t.Fatal(err)
			}
			notePath := filepath.Join(ingest.SessionDir(output, fixture.HostSlug, fixture.ParentID, ""), fixture.UnrelatedFile)
			if err := os.WriteFile(notePath, []byte(fixture.UnrelatedContent), 0600); err != nil {
				t.Fatal(err)
			}
			config.Force = row.Force
			parent.ModTime = time.Now().Add(time.Minute)
			adapter.Sessions = []ingest.DiscoveredSession{parent}
			if !row.HideChild {
				adapter.Sessions = append(adapter.Sessions, child)
			}
			pipeline, err = ingest.NewPipeline(filesystem, testutil.DefaultGitResolver(), registry, config)
			if err != nil {
				t.Fatal(err)
			}
			second, err := pipeline.Run(t.Context())
			if err != nil || second.Summary.Errors != 0 {
				t.Fatalf("refresh parent: %+v %v", second, err)
			}
			if second.Summary.New+second.Summary.Updated != 1 {
				t.Fatalf("fixture did not replace the parent artifact: %+v", second.Summary)
			}
			if data, err := os.ReadFile(childMetadataPath); err != nil || !bytes.Equal(data, childData) {
				t.Errorf("parent publication destroyed child metadata: %v", err)
			}
			if data, err := os.ReadFile(childTranscriptPath); err != nil || !bytes.Equal(data, childTranscript) {
				t.Errorf("parent publication destroyed child transcript: %v", err)
			}
			if data, err := os.ReadFile(notePath); err != nil || string(data) != fixture.UnrelatedContent {
				t.Errorf("parent publication removed unrelated file: %v", err)
			}
			if data, err := os.ReadFile(parentPath); err != nil || string(data) != fixture.Transcript {
				t.Errorf("native parent source changed: %v", err)
			}
			if data, err := os.ReadFile(childPath); err != nil || string(data) != fixture.Transcript {
				t.Errorf("native child source changed: %v", err)
			}
		})
	}
	for _, name := range required {
		if !seen[name] {
			t.Fatalf("missing ownership case %q", name)
		}
	}
}
