package e2e

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
)

// The shipped functional corpus must remain acceptable to authoritative
// indexing, not only the tolerant parser used by older fixture shape tests.
func TestFixture_AuthoritativeCaptureCompatibility(t *testing.T) {
	registry := ingest.NewIndexerRegistry(&ingest.OSFileSystem{}, ingest.IndexerRegistryOptions{})
	for _, corpus := range loadFixtureIndexes(t) {
		for _, fixture := range corpus.Sessions {
			t.Run(corpus.Harness+"/"+fixture.ID, func(t *testing.T) {
				path, err := ingest.NewResolvedPath(filepath.Join(corpus.Root, fixture.Path))
				if err != nil {
					t.Fatal(err)
				}
				id, err := ingest.NewSessionID(fixture.ID)
				if err != nil {
					t.Fatal(err)
				}
				session := ingest.DiscoveredSession{SessionID: id, Harness: ingest.Harness(corpus.Harness), SourcePath: path, SourceFormat: ingest.SourceFormatJSONL}
				indexer, ok := registry[session.Harness].(ingest.AuthoritativeTranscriptIndexer)
				if !ok {
					t.Fatal("fixture harness lacks authoritative indexer")
				}
				result, err := indexer.IndexTranscriptForCapture(t.Context(), session)
				if err != nil || len(result.Entries) == 0 {
					t.Fatalf("existing functional source rejected: %v", err)
				}
				data, err := os.ReadFile(path.String())
				if err != nil {
					t.Fatal(err)
				}
				captured, err := indexer.IndexTranscriptBytesForCapture(t.Context(), session, data)
				if err != nil || len(captured.Entries) != len(result.Entries) {
					t.Fatalf("existing staged source rejected or changed shape: %v", err)
				}
			})
		}
	}
}
