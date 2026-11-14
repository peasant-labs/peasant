package store_test

import (
	"context"
	_ "embed"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/publication-library.yaml
var publicationLibraryYAML []byte

func TestPublicationMetadataProjectionRequestedSet(t *testing.T) {
	s := openTestStore(t)
	e := publicationEntry(t, testutil.TestSessionUUID)
	revision := capturePublication(t, s, e)
	unrequested := publicationEntry(t, "33333333-3333-4333-8333-333333333333")
	capturePublication(t, s, unrequested)
	if result := indexPublication(t, s, e, revision, nil); result.Err != nil {
		t.Fatal(result.Err)
	}
	missing, _ := ingest.NewSessionID(testutil.TestSessionUUID2)
	result, err := s.LoadPublicationMetadata(t.Context(), []ingest.SessionID{e.Metadata.SessionID, missing, e.Metadata.SessionID})
	if err != nil || len(result) != 2 {
		t.Fatalf("requested set: %v %v", result, err)
	}
	if result[e.Metadata.SessionID].Readiness != ingest.PublicationReady || result[missing].Readiness != ingest.PublicationNeedsIngest || result[missing].Error == nil {
		t.Fatalf("readiness: %+v", result)
	}
	empty, err := s.LoadPublicationMetadata(t.Context(), nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty request expanded: %+v %v", empty, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if result, err := s.LoadPublicationMetadata(ctx, []ingest.SessionID{e.Metadata.SessionID}); err == nil || result != nil {
		t.Fatalf("cancelled read accepted: %+v %v", result, err)
	}
}

// BenchmarkPublicationLibrary measures the previous list strategy beside the
// production projection on the same real SQLite corpus. Setup is not timed.
func BenchmarkPublicationLibrary(b *testing.B) {
	var fixture struct {
		Name     string `yaml:"name"`
		Sessions int    `yaml:"sessions"`
		Entries  int    `yaml:"entriesPerSession"`
		Bytes    int    `yaml:"entryBytes"`
	}
	if err := yaml.Unmarshal(publicationLibraryYAML, &fixture); err != nil {
		b.Fatal(err)
	}
	if fixture.Name != "large-recorded-library" || fixture.Sessions < 1800 || fixture.Entries < 1 || fixture.Bytes < 1 {
		b.Fatal("invalid large-library measurement fixture")
	}
	s, err := store.Open(filepath.Join(b.TempDir(), "library.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	ids := make([]ingest.SessionID, 0, fixture.Sessions)
	captures := make([]ingest.StoreEntry, 0, fixture.Sessions)
	writes := make([]ingest.SessionEntryWrite, 0, fixture.Sessions)
	text := strings.Repeat("x", fixture.Bytes)
	for i := 0; i < fixture.Sessions; i++ {
		id, err := ingest.NewSessionID(fmt.Sprintf("%08x-0000-4000-8000-%012x", i, i))
		if err != nil {
			b.Fatal(err)
		}
		meta := ingest.NewUnifiedMetadata()
		meta.SessionID = id
		meta.HostSlug = testutil.TestHostSlug
		meta.Model = testutil.TestModel
		meta.ModelHarness = defaults.HarnessClaudeCode
		meta.Project = ingest.ProjectInfo{Hash: testutil.TestProjectHash, Name: "synthetic-library"}
		ingested := int64(1700000001000)
		meta.Timestamp = ingest.TimestampInfo{Start: 1700000000000, End: ingested, Ingested: &ingested}
		meta.Source = ingest.SourceInfo{Format: ingest.SourceFormatJSONL, FilePath: "/synthetic/source.jsonl"}
		meta.ContentHash = schema.ComputeTranscriptHash([]byte(text))
		meta.MetadataHash = schema.ComputeMetadataHash(&meta)
		captures = append(captures, ingest.StoreEntry{Metadata: &meta, PublicationCapture: true, CWDProvenance: ingest.CWDSourceAbsent})
		entries := make([]schema.SessionEntry, fixture.Entries)
		for j := range entries {
			entries[j] = schema.SessionEntry{SessionID: id, EntryIndex: j, Harness: defaults.HarnessClaudeCode, Role: schema.RoleUser, EntryType: schema.EntryTypeText, ContentPreview: &text}
		}
		writes = append(writes, ingest.SessionEntryWrite{SessionID: id, Result: indexformat.V1{Entries: entries}, IndexVersion: 1, IndexerVersion: ingest.HarvesterVersionRegistry[ingest.HarnessClaudeCode].IndexerVersion, IndexedAtMs: ingested})
		ids = append(ids, id)
	}
	revisions, err := s.InsertSessionsWithRevisions(context.Background(), captures)
	if err != nil {
		b.Fatal(err)
	}
	for i := range writes {
		writes[i].CaptureRevision = revisions[writes[i].SessionID]
	}
	for _, result := range s.IndexSessionEntryBatch(context.Background(), writes) {
		if result.Err != nil {
			b.Fatal(result.Err)
		}
	}
	b.Run("metadata-only", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			result, err := s.LoadPublicationMetadata(context.Background(), ids)
			if err != nil || len(result) != len(ids) {
				b.Fatalf("projection: %d %v", len(result), err)
			}
		}
	})
	b.Run("full-bundles", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			for _, id := range ids {
				result, err := s.LoadPublicationInput(context.Background(), id)
				if err != nil || result.Readiness != ingest.PublicationReady {
					b.Fatalf("bundle: %v", err)
				}
			}
		}
	})
}
