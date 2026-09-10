package ingest

import (
	"context"
	_ "embed"
	"maps"
	"testing"

	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/index_selection.yaml
var indexSelectionYAML []byte

type indexSelectionFixtures struct {
	Required []string `yaml:"required_names"`
	Cases    []struct {
		Name               string `yaml:"name"`
		Force              bool   `yaml:"force"`
		ProducerDelta      int    `yaml:"producer_delta"`
		MissingInputHash   bool   `yaml:"missing_input_hash"`
		DifferentInputHash bool   `yaml:"different_input_hash"`
		CaptureRevision    *int64 `yaml:"capture_revision"`
		Unbound            bool   `yaml:"unbound"`
		Incomplete         bool   `yaml:"incomplete"`
		DeclaredFormat     int    `yaml:"declared_format"`
		LenientIndexer     bool   `yaml:"lenient_indexer"`
		NeedsWork          bool   `yaml:"needs_work"`
	} `yaml:"cases"`
}

// lenientIndexer is a file indexer without the strict capture capability: it
// can index a projection but can never certify complete content.
type lenientIndexer struct{}

func (lenientIndexer) SourceKind() TranscriptSourceKind { return TranscriptSourceFile }
func (lenientIndexer) IndexTranscript(context.Context, DiscoveredSession) ([]schema.SessionEntry, error) {
	return nil, nil
}
func (lenientIndexer) IndexTranscriptBytes(context.Context, DiscoveredSession, []byte) ([]schema.SessionEntry, error) {
	return nil, nil
}

func TestCapturedInputNeedsWork(t *testing.T) {
	var fixtures indexSelectionFixtures
	if err := yaml.Unmarshal(indexSelectionYAML, &fixtures); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, fixture := range fixtures.Cases {
		if fixture.Name == "" || names[fixture.Name] {
			t.Fatalf("empty or duplicate index selection fixture %q", fixture.Name)
		}
		names[fixture.Name] = true
	}
	for _, name := range fixtures.Required {
		if !names[name] {
			t.Fatalf("missing index selection fixture %s", name)
		}
	}
	const harness = HarnessClaudeCode
	sid, err := NewSessionID("11111111-1111-4111-8111-111111111111")
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range fixtures.Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			versions := maps.Clone(HarvesterVersionRegistry)
			target := versions[harness]
			if fixture.DeclaredFormat != 0 {
				target.IndexVersion = fixture.DeclaredFormat
			}
			versions[harness] = target
			var indexer TranscriptIndexer = NewClaudeIndexer(nil)
			if fixture.LenientIndexer {
				indexer = lenientIndexer{}
			}
			// A decision table over the fields capturedInputNeedsWork and
			// certifiesContent read: config.Force, harvesterVersions, indexers.
			pipeline := &Pipeline{
				config:            PipelineConfig{Force: fixture.Force},
				harvesterVersions: versions,
				indexers:          map[Harness]TranscriptIndexer{harness: indexer},
			}
			artifactHash := "artifact"
			inputHash := "captured-input"
			storedHash := inputHash
			if fixture.DifferentInputHash {
				storedHash = "other-input"
			}
			state := &SessionIndexState{
				SessionID:        sid,
				Harness:          harness,
				ArtifactHash:     &artifactHash,
				IndexerVersion:   target.IndexerVersion + fixture.ProducerDelta,
				IndexedInputHash: &storedHash,
				// The settled default: bound to a current capture, content complete.
				PublicationCaptureRevision: 1,
				PublicationBound:           true,
				ContentStatus:              ContentCaptureComplete,
			}
			if fixture.MissingInputHash {
				state.IndexedInputHash = nil
			}
			if fixture.CaptureRevision != nil {
				state.PublicationCaptureRevision = *fixture.CaptureRevision
			}
			if fixture.Unbound {
				state.PublicationBound = false
			}
			if fixture.Incomplete {
				state.ContentStatus = ContentCaptureIncomplete
			}
			input := &CapturedIndexInput{session: DiscoveredSession{SessionID: sid, Harness: harness}, inputHash: inputHash, expected: state}
			if got := pipeline.capturedInputNeedsWork(input); got != fixture.NeedsWork {
				t.Fatalf("needs work = %t, want %t for state %+v", got, fixture.NeedsWork, *state)
			}
		})
	}
}
