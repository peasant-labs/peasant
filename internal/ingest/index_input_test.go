package ingest

import (
	"bytes"
	"context"
	_ "embed"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/index_input.yaml
var capturedIndexInputYAML []byte

type capturedInputFixtureKind string

const (
	capturedInputFile capturedInputFixtureKind = "file"
	capturedInputTree capturedInputFixtureKind = "tree"
)

type capturedInputFixture struct {
	SessionID       string   `yaml:"sessionID"`
	Message         string   `yaml:"message"`
	Part            string   `yaml:"part"`
	Replacement     string   `yaml:"replacement"`
	LaterFile       string   `yaml:"laterFile"`
	ExpectedContent string   `yaml:"expectedContent"`
	ExpectedCallID  string   `yaml:"expectedCallID"`
	RequiredNames   []string `yaml:"requiredNames"`
	Cases           []struct {
		Name string                   `yaml:"name"`
		Kind capturedInputFixtureKind `yaml:"kind"`
	} `yaml:"cases"`
}

func loadCapturedInputFixture(t *testing.T) capturedInputFixture {
	t.Helper()
	var fixture capturedInputFixture
	decoder := yaml.NewDecoder(bytes.NewReader(capturedIndexInputYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatal("captured input fixture requires one document")
	}
	names := make(map[string]bool)
	for _, row := range fixture.Cases {
		if row.Name == "" || names[row.Name] || (row.Kind != capturedInputFile && row.Kind != capturedInputTree) {
			t.Fatalf("invalid captured input case %+v", row)
		}
		names[row.Name] = true
	}
	if len(fixture.RequiredNames) == 0 {
		t.Fatal("missing required-name manifest")
	}
	for _, name := range fixture.RequiredNames {
		if !names[name] {
			t.Fatalf("missing capture case %q", name)
		}
	}
	return fixture
}

type inputReadCounter struct {
	*OSFileSystem
	reads int
}

func (filesystem *inputReadCounter) ReadFile(path string) ([]byte, error) {
	filesystem.reads++
	return filesystem.OSFileSystem.ReadFile(path)
}

var _ FileSystem = (*inputReadCounter)(nil)

func TestIndexerUsesCapturedInput(t *testing.T) {
	fixture := loadCapturedInputFixture(t)
	for _, row := range fixture.Cases {
		t.Run(row.Name, func(t *testing.T) {
			root := t.TempDir()
			filesystem := &inputReadCounter{OSFileSystem: &OSFileSystem{}}
			sid, err := NewSessionID(fixture.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			session := DiscoveredSession{SessionID: sid, Harness: HarnessClaudeCode, SourcePath: ResolvedPath(filepath.Join(root, "changed.jsonl"))}
			if row.Kind == capturedInputFile {
				if err := os.WriteFile(session.SourcePath.String(), []byte(fixture.LaterFile), 0600); err != nil {
					t.Fatal(err)
				}
				result, err := indexWithSourceKind(t.Context(), NewClaudeIndexer(filesystem), session, []byte{})
				if err != nil {
					t.Fatal(err)
				}
				if filesystem.reads != 0 || len(result.(indexformat.V1).Entries) != 0 {
					t.Fatal("captured empty bytes reopened a later source")
				}
				return
			}
			session.Harness, session.OriginalRoot = HarnessOpenCode, ResolvedPath(root)
			messagePath := filepath.Join(root, "storage", "message", session.SessionID.String(), "msg_1.json")
			partPath := filepath.Join(root, "storage", "part", "msg_1", "part_1.json")
			for path, data := range map[string]string{messagePath: fixture.Message, partPath: fixture.Part} {
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			indexer := NewOpenCodeIndexer(filesystem, WithOpenCodeFullDepth(true))
			capturedIndexer, ok := any(indexer).(interface {
				captureJSONInput(context.Context, DiscoveredSession) (*openCodeJSONInput, error)
				indexJSONInput(context.Context, DiscoveredSession, *openCodeJSONInput) (indexformat.Result, error)
			})
			if !ok {
				t.Fatal("OpenCode indexer cannot parse an owned native input capture")
			}
			captured, err := capturedIndexer.captureJSONInput(t.Context(), session)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(messagePath, []byte(fixture.Replacement), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(partPath); err != nil {
				t.Fatal(err)
			}
			reads := filesystem.reads
			result, err := capturedIndexer.indexJSONInput(t.Context(), session, captured)
			if err != nil {
				t.Fatal(err)
			}
			entries := result.(indexformat.V1).Entries
			if filesystem.reads != reads || len(entries) != 2 || entries[0].ContentPreview == nil || *entries[0].ContentPreview != fixture.ExpectedContent || entries[1].ToolCallID == nil || *entries[1].ToolCallID != fixture.ExpectedCallID {
				t.Fatalf("indexing used later native state: %+v", entries)
			}
			later, err := capturedIndexer.captureJSONInput(t.Context(), session)
			if err != nil {
				t.Fatal(err)
			}
			if indexInputDigest(session, nil, captured) == indexInputDigest(session, nil, later) {
				t.Fatal("changed native records retained the previous input identity")
			}
		})
	}
}
