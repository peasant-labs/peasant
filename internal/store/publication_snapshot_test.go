package store_test

import (
	_ "embed"
	"reflect"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
)

//go:embed testdata/publication_snapshot.yaml
var publicationSnapshotYAML []byte

// A metadata SELECT establishes the snapshot before the full reader runs. A
// second real store then commits a replacement while that reader is active.
func TestPublicationBundlePinsMetadataAndFullContentSnapshot(t *testing.T) {
	var fixture struct {
		Name        string `yaml:"name"`
		BeforeModel string `yaml:"before_model"`
		AfterModel  string `yaml:"after_model"`
		BeforeText  string `yaml:"before_text"`
		AfterText   string `yaml:"after_text"`
	}
	if err := yaml.Unmarshal(publicationSnapshotYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Name != "concurrent-capture-after-eligibility" || fixture.BeforeText == fixture.AfterText || fixture.BeforeModel == fixture.AfterModel {
		t.Fatal("snapshot fixture must distinguish both metadata and content generations")
	}
	path := storetest.CopyGoldenDB(t)
	reader, err := store.Open(path, store.WithPoolSize(1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	writer, err := store.Open(path, store.WithPoolSize(1))
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	e := publicationEntry(t, "11111111-1111-4111-8111-111111111111")
	e.Metadata.Model = schema.ModelID(fixture.BeforeModel)
	e.Metadata.MetadataHash = schema.ComputeMetadataHash(e.Metadata)
	id := e.Metadata.SessionID
	before := batchTestEntries(id, fixture.BeforeText, 1)
	beforeRevision := capturePublication(t, reader, e)
	if result := indexPublication(t, reader, e, beforeRevision, before); result.Err != nil {
		t.Fatal(result.Err)
	}
	afterMeta := *e.Metadata
	afterMeta.Model = schema.ModelID(fixture.AfterModel)
	afterMeta.ContentHash = schema.ComputeTranscriptHash([]byte(fixture.AfterText))
	afterMeta.MetadataHash = schema.ComputeMetadataHash(&afterMeta)
	afterEntry := e
	afterEntry.Metadata = &afterMeta
	after := batchTestEntries(id, fixture.AfterText, 1)
	var afterRevision int64
	triggered := false
	observeFullReads(t, reader, func(a sqlite.Action) {
		if triggered || a.Type() != sqlite.OpRead || a.Table() != "session_entries" {
			return
		}
		triggered = true
		afterRevision = capturePublication(t, writer, afterEntry)
		if result := indexPublication(t, writer, afterEntry, afterRevision, after); result.Err != nil {
			t.Error(result.Err)
		}
	})
	bundle, err := reader.LoadPublicationInput(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !triggered || afterRevision <= beforeRevision {
		t.Fatal("concurrent replacement did not commit during the full read")
	}
	if bundle.Readiness != ingest.PublicationReady || bundle.Metadata.Model != e.Metadata.Model || !reflect.DeepEqual(bundle.Entries, before) || bundle.CaptureRevision != beforeRevision || bundle.ContentCapture.PublicationCaptureRevision != beforeRevision {
		t.Fatal("publication mixed metadata and full content from different snapshots")
	}
	latest, err := reader.LoadPublicationInput(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Readiness != ingest.PublicationReady || latest.Metadata.Model != afterMeta.Model || !reflect.DeepEqual(latest.Entries, after) || latest.CaptureRevision != afterRevision || latest.ContentCapture.PublicationCaptureRevision != afterRevision {
		t.Fatal("subsequent publication read missed the coherent replacement capture")
	}
}
