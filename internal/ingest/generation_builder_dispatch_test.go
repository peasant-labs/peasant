package ingest

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/schema"
)

// The managed-generation dispatch is a capability assertion, not a concrete
// type switch. These subtests pin the three arms: an indexer that implements
// the builder is dispatched through the production contract with no pipeline
// edit; a builder-less indexer keeps the retained parse path; and a
// versioned-only indexer keeps the versioned exit. The doubles implement only
// the surfaces their arm needs.

// dispatchBuilderIndexer is a base indexer plus a generation builder and
// deliberately NOT a VersionedTranscriptIndexer, so the construction gate must
// admit it through the builder alone.
type dispatchBuilderIndexer struct {
	built int
}

func (dispatchBuilderIndexer) SourceKind() TranscriptSourceKind { return TranscriptSourceFile }

func (dispatchBuilderIndexer) IndexTranscript(context.Context, DiscoveredSession) ([]schema.SessionEntry, error) {
	return nil, nil
}

func (dispatchBuilderIndexer) IndexTranscriptBytes(context.Context, DiscoveredSession, []byte) ([]schema.SessionEntry, error) {
	return nil, nil
}

func (d *dispatchBuilderIndexer) BuildGeneration(_ context.Context, in GenerationBuildInput) (GenerationCandidate, error) {
	d.built++
	return GenerationCandidate{Result: indexformat.V2{Generation: indexformat.Generation{ID: in.GenerationID}}}, nil
}

// dispatchPlainIndexer is a base indexer with neither optional surface.
type dispatchPlainIndexer struct{}

func (dispatchPlainIndexer) SourceKind() TranscriptSourceKind { return TranscriptSourceFile }

func (dispatchPlainIndexer) IndexTranscript(context.Context, DiscoveredSession) ([]schema.SessionEntry, error) {
	return nil, nil
}

func (dispatchPlainIndexer) IndexTranscriptBytes(context.Context, DiscoveredSession, []byte) ([]schema.SessionEntry, error) {
	return nil, nil
}

// dispatchVersionedIndexer is a base indexer plus the versioned result exit and
// deliberately NOT a generation builder.
type dispatchVersionedIndexer struct{}

func (dispatchVersionedIndexer) SourceKind() TranscriptSourceKind { return TranscriptSourceFile }

func (dispatchVersionedIndexer) IndexTranscript(context.Context, DiscoveredSession) ([]schema.SessionEntry, error) {
	return nil, nil
}

func (dispatchVersionedIndexer) IndexTranscriptBytes(context.Context, DiscoveredSession, []byte) ([]schema.SessionEntry, error) {
	return nil, nil
}

func (dispatchVersionedIndexer) IndexTranscriptResult(context.Context, DiscoveredSession) (indexformat.Result, error) {
	return nil, nil
}

func (dispatchVersionedIndexer) IndexTranscriptBytesResult(context.Context, DiscoveredSession, []byte) (indexformat.Result, error) {
	return nil, nil
}

var (
	_ GenerationBuilder          = (*dispatchBuilderIndexer)(nil)
	_ TranscriptIndexer          = (*dispatchPlainIndexer)(nil)
	_ VersionedTranscriptIndexer = (*dispatchVersionedIndexer)(nil)
)

// TestGenerationBuilderDispatch pins the builder dispatch arms on the real
// pipeline method, so the builder is reached through the production contract
// rather than a concrete harness case.
func TestGenerationBuilderDispatch(t *testing.T) {
	t.Parallel()
	sessionID, err := NewSessionID("99d59925-36bc-424c-a789-8be54d9702ba")
	if err != nil {
		t.Fatal(err)
	}
	input := &CapturedIndexInput{
		session:    DiscoveredSession{SessionID: sessionID, Harness: HarnessCursor, SourceFormat: SourceFormatJSONL},
		kind:       TranscriptSourceFile,
		transcript: []byte(`{"dispatch":"probe"}`),
	}

	t.Run("builder-double-dispatch", func(t *testing.T) {
		t.Parallel()
		pipeline := &Pipeline{generationIDs: func(DiscoveredSession) string { return "gen-dispatch-double" }}
		double := &dispatchBuilderIndexer{}
		candidate, used, err := pipeline.buildGenerationCandidate(context.Background(), input, double)
		if err != nil {
			t.Fatalf("buildGenerationCandidate: %v", err)
		}
		if !used {
			t.Fatal("a GenerationBuilder indexer was not dispatched through the builder")
		}
		if double.built != 1 {
			t.Fatalf("builder calls = %d, want 1", double.built)
		}
		if candidate.Result.Generation.ID != "gen-dispatch-double" {
			t.Fatalf("candidate generation id = %q, want the injected identity", candidate.Result.Generation.ID)
		}
	})

	t.Run("builder-double-admitted-for-declared-two", func(t *testing.T) {
		t.Parallel()
		if !indexerDeclaresManagedOutput(&dispatchBuilderIndexer{}) {
			t.Fatal("a base indexer plus GenerationBuilder was not admitted for a declared format 2")
		}
	})

	t.Run("builder-less-indexer-keeps-stored-path", func(t *testing.T) {
		t.Parallel()
		pipeline := &Pipeline{}
		_, used, err := pipeline.buildGenerationCandidate(context.Background(), input, dispatchPlainIndexer{})
		if err != nil {
			t.Fatalf("buildGenerationCandidate: %v", err)
		}
		if used {
			t.Fatal("a builder-less indexer was dispatched through the builder")
		}
	})

	t.Run("versioned-only-indexer-keeps-versioned-exit", func(t *testing.T) {
		t.Parallel()
		pipeline := &Pipeline{}
		_, used, err := pipeline.buildGenerationCandidate(context.Background(), input, dispatchVersionedIndexer{})
		if err != nil {
			t.Fatalf("buildGenerationCandidate: %v", err)
		}
		if used {
			t.Fatal("a versioned-only indexer was dispatched through the builder")
		}
		if !indexerDeclaresManagedOutput(dispatchVersionedIndexer{}) {
			t.Fatal("a versioned-only indexer was not admitted for its declared format")
		}
	})
}
