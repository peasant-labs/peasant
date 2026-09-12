package ingest_test

import (
	"bytes"
	_ "embed"
	"io"
	"io/fs"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/opencode_index_completion.yaml
var openCodeIndexCompletionData []byte

type openCodeCompletionFixture struct {
	Name          string            `yaml:"name"`
	Files         map[string]string `yaml:"files"`
	Directories   []string          `yaml:"directories"`
	ReadError     string            `yaml:"readError"`
	DirError      string            `yaml:"dirError"`
	Origin        string            `yaml:"origin"`
	Managed       string            `yaml:"managed"`
	Error         bool              `yaml:"error"`
	Entries       int               `yaml:"entries"`
	LegacyEntries *int              `yaml:"legacyEntries"`
	LongContent   bool              `yaml:"longContent"`
}

type completionFaultFS struct {
	*testutil.MemFS
	readError string
	dirError  string
}

func (filesystem *completionFaultFS) ReadFile(path string) ([]byte, error) {
	if path == filesystem.readError {
		return nil, &fs.PathError{Op: "read", Path: path, Err: fs.ErrPermission}
	}
	return filesystem.MemFS.ReadFile(path)
}

func (filesystem *completionFaultFS) ReadDir(path string) ([]fs.DirEntry, error) {
	if path == filesystem.dirError {
		return nil, &fs.PathError{Op: "readdir", Path: path, Err: fs.ErrPermission}
	}
	return filesystem.MemFS.ReadDir(path)
}

var _ ingest.FileSystem = (*completionFaultFS)(nil)

func loadOpenCodeCompletionFixtures(t *testing.T) []openCodeCompletionFixture {
	t.Helper()
	var fixtures struct {
		RequiredNames []string                    `yaml:"requiredNames"`
		Cases         []openCodeCompletionFixture `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(openCodeIndexCompletionData))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("OpenCode completion fixture must contain exactly one YAML document: %v", err)
	}
	names := make(map[string]bool)
	for _, fixture := range fixtures.Cases {
		if fixture.Name == "" || names[fixture.Name] {
			t.Fatalf("absent/duplicate fixture name %q", fixture.Name)
		}
		names[fixture.Name] = true
	}
	for _, name := range fixtures.RequiredNames {
		if !names[name] {
			t.Fatalf("required fixture %q missing", name)
		}
	}
	return fixtures.Cases
}

func setupOpenCodeCompletion(t *testing.T, fixture openCodeCompletionFixture) (*completionFaultFS, ingest.DiscoveredSession, map[string][]byte) {
	t.Helper()
	filesystem := &completionFaultFS{MemFS: testutil.NewMemFS()}
	session := setupOpenCodeFixture(t, filesystem.MemFS, testutil.TestOpenCodeSesID, "synthetic")
	expand := strings.NewReplacer("SESSION_ID", session.SessionID.String(), "LONG_CONTENT", strings.Repeat("x", defaults.ContentPreviewLimit+50))
	sourceBytes := make(map[string][]byte)
	sessionData, err := filesystem.ReadFile(session.SourcePath.String())
	if err != nil {
		t.Fatal(err)
	}
	sourceBytes[session.SourcePath.String()] = sessionData
	for name, content := range fixture.Files {
		path := filepath.Join("/opencode-store/storage", expand.Replace(name))
		data := []byte(expand.Replace(content))
		if err := filesystem.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		sourceBytes[path] = data
	}
	for _, directory := range fixture.Directories {
		if err := filesystem.MkdirAll(filepath.Join("/opencode-store/storage", expand.Replace(directory)), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if fixture.Origin != "" {
		switch fixture.Origin {
		case "current":
			session.TranscriptOrigin = ingest.TranscriptOriginOpenCodeCurrentSQLite
		case "legacy":
			session.TranscriptOrigin = ingest.TranscriptOriginOpenCodeLegacySQLite
		default:
			t.Fatalf("unknown fixture origin %q", fixture.Origin)
		}
		session.SourcePath = "/synthetic/projection.json"
		data := []byte(expand.Replace(fixture.Managed))
		if err := filesystem.WriteFile(session.SourcePath.String(), data, 0600); err != nil {
			t.Fatal(err)
		}
		sourceBytes[session.SourcePath.String()] = data
	}
	if fixture.ReadError != "" {
		filesystem.readError = filepath.Join("/opencode-store/storage", expand.Replace(fixture.ReadError))
	}
	if fixture.DirError != "" {
		filesystem.dirError = filepath.Join("/opencode-store/storage", expand.Replace(fixture.DirError))
	}
	return filesystem, session, sourceBytes
}

func TestOpenCodeConcreteCompletion(t *testing.T) {
	for _, fixture := range loadOpenCodeCompletionFixtures(t) {
		t.Run(fixture.Name, func(t *testing.T) {
			filesystem, session, before := setupOpenCodeCompletion(t, fixture)
			indexer := ingest.NewIndexerRegistry(filesystem, ingest.IndexerRegistryOptions{})[ingest.HarnessOpenCode]
			strict, ok := indexer.(ingest.VersionedTranscriptIndexer)
			if !ok {
				t.Fatal("OpenCode parser cannot verify completion")
			}
			result, err := strict.IndexTranscriptResult(t.Context(), session)
			if fixture.Error {
				if err == nil || result != nil {
					t.Fatalf("incomplete source certified: %#v %v", result, err)
				}
			} else {
				assertCompletionEntries(t, result, err, fixture.Entries)
			}
			if fixture.Origin != "" {
				byteResult, byteErr := strict.IndexTranscriptBytesResult(t.Context(), session, before[session.SourcePath.String()])
				if (byteErr != nil) != fixture.Error || !reflect.DeepEqual(result, byteResult) {
					t.Fatalf("file/byte outcomes differ: %#v %v", byteResult, byteErr)
				}
			}
			if fixture.LegacyEntries != nil {
				entries, legacyErr := indexer.IndexTranscript(t.Context(), session)
				if legacyErr != nil || len(entries) != *fixture.LegacyEntries {
					t.Fatalf("legacy tolerance changed: %d %v", len(entries), legacyErr)
				}
			}
			for path, data := range before {
				after, err := filesystem.MemFS.ReadFile(path)
				if err != nil || !bytes.Equal(data, after) {
					t.Fatalf("source %s changed: %v", path, err)
				}
			}
		})
	}
}

func TestOpenCodeCompletionCoordinates(t *testing.T) {
	for _, fixture := range loadOpenCodeCompletionFixtures(t) {
		if fixture.Error {
			continue
		}
		t.Run(fixture.Name, func(t *testing.T) {
			filesystem, session, _ := setupOpenCodeCompletion(t, fixture)
			bounded, err := ingest.NewIndexerRegistry(filesystem, ingest.IndexerRegistryOptions{})[ingest.HarnessOpenCode].IndexTranscript(t.Context(), session)
			if err != nil {
				t.Fatal(err)
			}
			full, err := ingest.NewIndexerRegistry(filesystem, ingest.IndexerRegistryOptions{FullContent: true})[ingest.HarnessOpenCode].IndexTranscript(t.Context(), session)
			if err != nil {
				t.Fatal(err)
			}
			if len(bounded) != len(full) {
				t.Fatalf("content mode changed entry coordinates: bounded=%d full=%d", len(bounded), len(full))
			}
			if fixture.LongContent && (bounded[0].ContentPreview == nil || full[0].ContentPreview == nil || len(*bounded[0].ContentPreview) != defaults.ContentPreviewLimit || len(*full[0].ContentPreview) <= defaults.ContentPreviewLimit) {
				t.Fatal("fixture did not exercise truncation")
			}
			for n := range bounded {
				bounded[n].ContentPreview, full[n].ContentPreview = nil, nil
				bounded[n].ToolOutput, full[n].ToolOutput = nil, nil
			}
			if !reflect.DeepEqual(bounded, full) {
				t.Fatal("content mode changed stable identities, relationships or semantics")
			}
		})
	}
}
