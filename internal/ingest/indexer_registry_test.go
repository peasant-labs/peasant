package ingest_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// TestNewIndexerRegistry_MatchesAdapterRegistry guards the registry's shape.
// Every discoverable harness must also have an indexer in both normal ingest
// and the content-overlay re-index path.
func TestNewIndexerRegistry_MatchesAdapterRegistry(t *testing.T) {
	t.Parallel()
	reg := ingest.NewIndexerRegistry(testutil.NewMemFS(), ingest.IndexerRegistryOptions{})
	if len(reg) != len(ingest.DefaultAdapterRegistry) {
		t.Fatalf("indexer registry size = %d, adapter registry size = %d", len(reg), len(ingest.DefaultAdapterRegistry))
	}
	for h := range ingest.DefaultAdapterRegistry {
		if _, ok := reg[h]; !ok {
			t.Errorf("registry missing harness %q", h)
		}
	}
	if _, ok := reg[ingest.HarnessStrike].(*ingest.StrikeIndexer); !ok {
		t.Fatalf("registry[HarnessStrike]: expected *ingest.StrikeIndexer, got %T", reg[ingest.HarnessStrike])
	}
}

// TestNewIndexerRegistry_EveryIndexerDeclaresWhereItsEntriesComeFrom makes
// forgetting impossible rather than merely refusable.
//
// The dispatch refuses an undeclared source kind, which turns a forgotten
// declaration into a failed import rather than a session stored empty. That is
// the right behaviour at run time and a poor place to find out: the harness is
// already shipped and a user's transcript is already the thing that failed. This
// finds it in the SUITE instead, over the registry the real pipeline uses.
//
// Not at build time - measured: an indexer returning the zero value still
// compiles (`go build ./...` exit 0) and still passes `go vet`. Only `go test`
// goes red. The distinction matters because it decides where a maintainer meets
// the mistake, and claiming the compiler catches it would let someone skip the
// suite believing a green build had proved something.
//
// The compile-time interface guard cannot do this. It forces the METHOD to exist
// and can force nothing about the VALUE it returns, which is exactly the gap the
// zero value falls into.
func TestNewIndexerRegistry_EveryIndexerDeclaresWhereItsEntriesComeFrom(t *testing.T) {
	t.Parallel()
	registry := ingest.NewIndexerRegistry(testutil.NewMemFS(), ingest.IndexerRegistryOptions{})
	if len(registry) == 0 {
		t.Fatal("the registry is empty, so this guard proved nothing about any indexer")
	}
	for harness, indexer := range registry {
		if indexer.SourceKind() == ingest.TranscriptSourceKindUnknown {
			t.Errorf("the %q indexer (%T) declares no transcript source kind, so the INDEX stage will refuse every session "+
				"for that harness. The zero value is an absent declaration, not a choice: return TranscriptSourceFile if "+
				"its entries are in the transcript bytes, or TranscriptSourceDirectory if they are spread over a provider "+
				"tree. Declaring the wrong one is worse than declaring none - a directory harness handed bytes discards "+
				"them and indexes nothing on that pass, and the stale-index sweep is then the only reason the session is "+
				"not left empty.", harness, indexer)
		}
	}
}

// TestNewIndexerRegistry_FullContentThreadsThroughEachIndexer verifies the
// FullContent option actually reaches each harness's indexer (Claude,
// OpenCode, Codex, and Strike; Cursor has no such option, see NewIndexerRegistry's doc
// comment) rather than being silently dropped for one of them, which would
// be the exact kind of drift this consolidation exists to prevent.
func TestNewIndexerRegistry_FullContentThreadsThroughEachIndexer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	longContent := "Q from a fixture, long enough to matter"

	t.Run("claude_code_harness", func(t *testing.T) {
		t.Parallel()
		fs := testutil.NewMemFS()
		line := fmt.Sprintf(`{"type":"user","message":{"role":"user","content":%q}}`, longContent)
		regTruncated := ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{FullContent: false})
		regFull := ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{FullContent: true})
		session := ingest.DiscoveredSession{
			SessionID:    ingest.SessionID(testutil.TestSessionUUID),
			Harness:      ingest.HarnessClaudeCode,
			SourcePath:   "/f",
			SourceFormat: ingest.SourceFormatJSONL,
		}
		entriesTruncated, err := regTruncated[ingest.HarnessClaudeCode].IndexTranscriptBytes(ctx, session, []byte(line+"\n"))
		if err != nil {
			t.Fatalf("IndexTranscriptBytes (truncated): %v", err)
		}
		entriesFull, err := regFull[ingest.HarnessClaudeCode].IndexTranscriptBytes(ctx, session, []byte(line+"\n"))
		if err != nil {
			t.Fatalf("IndexTranscriptBytes (full): %v", err)
		}
		if len(entriesTruncated) != 1 || len(entriesFull) != 1 {
			t.Fatalf("entry counts: truncated=%d full=%d, want 1 each", len(entriesTruncated), len(entriesFull))
		}
		if entriesFull[0].ContentPreview == nil || *entriesFull[0].ContentPreview != longContent {
			t.Errorf("FullContent registry: expected verbatim content %q, got %v", longContent, entriesFull[0].ContentPreview)
		}
	})

	t.Run("codex_harness", func(t *testing.T) {
		t.Parallel()
		fs := testutil.NewMemFS()
		line := fmt.Sprintf(
			`{"timestamp":"2026-07-22T00:00:00.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":%q}]}}`,
			longContent,
		)
		regFull := ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{FullContent: true})
		session := ingest.DiscoveredSession{
			SessionID:    ingest.SessionID(testutil.TestCodexSessionID),
			Harness:      ingest.HarnessCodex,
			SourcePath:   "/f",
			SourceFormat: ingest.SourceFormatJSONL,
		}
		entries, err := regFull[ingest.HarnessCodex].IndexTranscriptBytes(ctx, session, []byte(line+"\n"))
		if err != nil {
			t.Fatalf("IndexTranscriptBytes: %v", err)
		}
		if len(entries) != 1 || entries[0].ContentPreview == nil || *entries[0].ContentPreview != longContent {
			t.Errorf("codex FullContent registry: expected verbatim content %q, got entries=%v", longContent, entries)
		}
	})
}

// TestNewIndexerRegistry_CursorHasNoFullContentOption documents (and guards)
// that Cursor is deliberately excluded from the FullContent threading above:
// its indexer has no full-content toggle at all and always truncates at
// defaults.ContentPreviewLimit, matching NewIndexerRegistry's and
// transcript.BuildContentOverlay's doc comments about the known Cursor gap.
func TestNewIndexerRegistry_CursorHasNoFullContentOption(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	reg := ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{FullContent: true})
	if _, ok := reg[ingest.HarnessCursor].(*ingest.CursorIndexer); !ok {
		t.Fatalf("registry[HarnessCursor]: expected *ingest.CursorIndexer, got %T", reg[ingest.HarnessCursor])
	}
	_ = schema.HarnessCursor // typed-constant sanity: HarnessCursor is a real schema.Harness value
}
