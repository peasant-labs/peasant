package ingest_test

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/pi_capture.yaml
var piCaptureYAML []byte

type piCaptureCase struct {
	Name        string `yaml:"name"`
	Tail        string `yaml:"tail"`
	Reject      bool   `yaml:"reject"`
	Incomplete  bool   `yaml:"incomplete"`
	InvalidUTF8 bool   `yaml:"invalid_utf8"`
}

// Only acquisition timing is replaced: discovery and capture both read real files.
type piCaptureTimingFS struct {
	*ingest.OSFileSystem
	path        string
	replacement []byte
}

var _ ingest.FileSystem = (*piCaptureTimingFS)(nil)

func (f *piCaptureTimingFS) ReadSourcePrefix(path string) ([]byte, error) {
	if path == f.path && f.replacement != nil {
		if err := os.WriteFile(path, f.replacement, 0600); err != nil {
			return nil, err
		}
	}
	return f.OSFileSystem.ReadSourcePrefix(path)
}

func TestPiCapturedAdmission(t *testing.T) {
	var corpus struct {
		SessionID      string          `yaml:"session_id"`
		OtherSessionID string          `yaml:"other_session_id"`
		Initial        string          `yaml:"initial"`
		ValidAppend    string          `yaml:"valid_append"`
		Cases          []piCaptureCase `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(piCaptureYAML, &corpus); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, c := range corpus.Cases {
		if c.Name == "" || names[c.Name] {
			t.Fatal("empty or duplicate capture case")
		}
		names[c.Name] = true
	}
	if err := testutil.RequireFixtureNames("pi_capture.yaml", "cases", []string{
		"malformed-final-newline", "malformed-final-no-newline", "malformed-before-incomplete-tail",
		"incomplete-before-blank-final", "incomplete-before-whitespace-final", "duplicate-complete", "duplicate-incomplete",
		"null-complete", "unknown-type", "dangling-parent", "duplicate-entry-id", "invalid-utf8",
		"incomplete-final", "incomplete-final-newline", "valid-append", "valid-blank-lines",
		"invalid-utf8-incomplete", "invalid-surrogate-incomplete",
	}, names); err != nil {
		t.Fatal(err)
	}
	for _, c := range corpus.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Run("new", func(t *testing.T) {
				testPiCapture(t, c, corpus.SessionID, corpus.OtherSessionID, corpus.Initial, corpus.ValidAppend, false)
			})
			t.Run("stored", func(t *testing.T) {
				testPiCapture(t, c, corpus.SessionID, corpus.OtherSessionID, corpus.Initial, corpus.ValidAppend, true)
			})
		})
	}
}

func testPiCapture(t *testing.T, c piCaptureCase, id, otherID, initial, validAppend string, seed bool) {
	root := t.TempDir()
	source := filepath.Join(root, "source.jsonl")
	other := filepath.Join(root, "other.jsonl")
	output := filepath.Join(root, "managed")
	write := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(source, initial)
	sid, err := ingest.NewSessionID(id)
	if err != nil {
		t.Fatal(err)
	}
	otherSID, err := ingest.NewSessionID(otherID)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := ingest.NewResolvedPath(source)
	if err != nil {
		t.Fatal(err)
	}
	otherResolved, err := ingest.NewResolvedPath(other)
	if err != nil {
		t.Fatal(err)
	}
	out, err := ingest.NewResolvedPath(output)
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(root, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	filesystem := &piCaptureTimingFS{OSFileSystem: &ingest.OSFileSystem{}, path: source}
	cfg := ingest.PipelineConfig{Sources: map[ingest.Harness]ingest.SourceConfig{ingest.HarnessPi: {Enabled: true, Paths: []ingest.ResolvedPath{resolved}}}, OutputDir: out, Parallelism: 1}
	run := func() *ingest.PipelineResult {
		t.Helper()
		p, err := ingest.NewPipeline(filesystem, testutil.NoGitResolver(), ingest.DefaultAdapterRegistry, cfg, ingest.WithStore(db), ingest.WithMetricsStore(db), ingest.WithIndexers(map[ingest.Harness]ingest.TranscriptIndexer{ingest.HarnessPi: ingest.NewPiIndexer(filesystem)}))
		if err != nil {
			t.Fatal(err)
		}
		result, err := p.Run(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if result.Summary.StoreError != nil {
			t.Fatal(result.Summary.StoreError)
		}
		return result
	}
	location := func() ingest.SessionLocation {
		t.Helper()
		locations, err := db.BulkLookupSessionLocations(t.Context(), []ingest.SessionID{sid})
		if err != nil {
			t.Fatal(err)
		}
		return locations[sid]
	}
	artifacts := func() map[string]string {
		t.Helper()
		files := make(map[string]string)
		err := filepath.WalkDir(output, func(path string, entry fs.DirEntry, err error) error {
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return err
			}
			if !entry.IsDir() && strings.Contains(path, id) {
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				files[path] = string(data)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return files
	}
	if seed {
		result := run()
		if result.Summary.New != 1 || result.Summary.Indexed != 1 {
			t.Fatalf("seed: %+v", result.Summary)
		}
	}
	before := location()
	beforeEntries, err := db.ListEntries(t.Context(), sid)
	if err != nil {
		t.Fatal(err)
	}
	beforeFiles := artifacts()
	if seed && (len(before.SourceFingerprint) == 0 || len(beforeFiles) == 0 || len(beforeEntries) != 1) {
		t.Fatal("vacuous seed")
	}
	write(other, strings.ReplaceAll(initial, id, otherID))
	cfg.Sources[ingest.HarnessPi] = ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{resolved, otherResolved}}
	acquired := initial + validAppend + c.Tail
	if c.InvalidUTF8 {
		acquired = strings.ReplaceAll(acquired, "INVALID_UTF8", string([]byte{0xff}))
	}
	filesystem.replacement = []byte(acquired)
	result := run()
	sourceAfter, err := os.ReadFile(source)
	if err != nil || string(sourceAfter) != acquired {
		t.Fatal("pipeline modified acquired original")
	}
	otherEntries, err := db.ListEntries(t.Context(), otherSID)
	if err != nil || len(otherEntries) != 1 {
		t.Fatal("other session did not continue")
	}
	after := location()
	entries, err := db.ListEntries(t.Context(), sid)
	if err != nil {
		t.Fatal(err)
	}
	if c.Reject {
		if result.Summary.Errors != 1 || result.Summary.New != 1 || result.Summary.Updated != 0 || result.Summary.Indexed != 1 {
			t.Fatalf("invalid capture admitted: %+v", result.Summary)
		}
		if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(beforeEntries, entries) || !reflect.DeepEqual(beforeFiles, artifacts()) {
			t.Fatal("rejected capture changed persisted state or managed artifacts")
		}
		return
	}
	if result.Summary.Errors != 0 || result.Summary.Indexed != 2 || len(entries) != 2 {
		t.Fatalf("valid capture: %+v entries=%d", result.Summary, len(entries))
	}
	expected := acquired
	if c.Incomplete {
		expected = initial + validAppend
	}
	hash := sha256.Sum256([]byte(expected))
	if !bytes.Equal(hash[:], after.SourceFingerprint) {
		t.Fatal("fingerprint is not exact consumed prefix")
	}
	transcript := filepath.Join(output, after.HostSlug, id, id+"--transcript.jsonl")
	data, err := os.ReadFile(transcript)
	if err != nil || string(data) != expected {
		t.Fatal("managed bytes differ from accepted capture")
	}
	if seed && bytes.Equal(before.SourceFingerprint, after.SourceFingerprint) {
		t.Fatal("valid acquired append did not advance fingerprint")
	}
	metadata, err := os.ReadFile(filepath.Join(output, after.HostSlug, id, id+"--metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	var meta ingest.UnifiedMetadata
	if err := json.Unmarshal(metadata, &meta); err != nil {
		t.Fatal(err)
	}
	foundTail := false
	for _, warning := range meta.Diagnostics.Warnings {
		if warning.ErrorType == "incomplete_tail" {
			foundTail = true
			if warning.Location == "" || warning.Remediation == "" {
				t.Fatal("tail diagnostic is not actionable")
			}
		}
	}
	if foundTail != c.Incomplete {
		t.Fatalf("captured tail diagnostic=%v want=%v", foundTail, c.Incomplete)
	}
	// Discovery stays older than acquisition; freshness follows consumed bytes.
	write(source, initial)
	repeat := run()
	if repeat.Summary.Unchanged != 2 || repeat.Summary.Indexed != 0 {
		t.Fatalf("stable capture reindexed: %+v", repeat.Summary)
	}
}
