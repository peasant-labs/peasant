package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/schema"
	"github.com/spf13/cobra"
)

// TestOpenRunStoreHonorsConfiguredWriteBudgets proves the production run
// store carries the configured write.* budgets into the staging lane.
// Harvest and push open their stores through openRunStore, so staging
// through that constructor is the production threading, not the option in
// isolation: with a tiny byte budget each body stages alone, while the
// shipped budgets commit the same batch once.
func TestOpenRunStoreHonorsConfiguredWriteBudgets(t *testing.T) {
	t.Parallel()
	tiny := ingest.WriteConfig{BatchBytes: 64, BatchSessions: 64}.WithDefaults(1)
	if got := stageTwoBodiesThroughRunStore(t, tiny); got != 2 {
		t.Fatalf("tiny-budget staging committed %d frames, want 2 alone-sized transactions", got)
	}
	if got := stageTwoBodiesThroughRunStore(t, ingest.DefaultWriteConfig(1)); got != 1 {
		t.Fatalf("shipped-budget staging committed %d frames, want the single batch commit", got)
	}
}

// stageTwoBodiesThroughRunStore opens a run store the way harvest does with
// the given write budgets, inserts one session, stages a two-body
// candidate, and returns the WAL commit frames the staging gained.
func stageTwoBodiesThroughRunStore(t *testing.T, cfg ingest.WriteConfig) int {
	t.Helper()
	dataDir := t.TempDir()
	ownedRoot := t.TempDir()
	cmd := &cobra.Command{}
	cmd.Flags().String("data-dir", "", "")
	if err := cmd.Flags().Set("data-dir", dataDir); err != nil {
		t.Fatal(err)
	}
	dbPath := defaults.ResolveDBFilePathWith(dataDir).String()
	db, err := openRunStore(cmd, false, ownedRoot, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	sid := insertRunStoreSession(t, ctx, db)
	v2, blobs := runStoreCandidate(t, sid)
	before, err := storetest.CountWALCommitFrames(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.StageGeneration(ctx, store.GenerationActivation{
		Generation:     v2,
		Blobs:          blobs,
		IndexerVersion: 1,
		IndexedAtMs:    1,
		ContentCapture: ingest.SessionContentCaptureWrite{
			Status:           ingest.ContentCaptureComplete,
			SourceAuthority:  ingest.ContentSourceNewIngest,
			TranscriptOrigin: ingest.TranscriptOriginFile,
			CaptureFormat:    ingest.ContentCaptureFormatFull,
			CapturedAtMs:     1,
		},
	}); err != nil {
		t.Fatal(err)
	}
	after, err := storetest.CountWALCommitFrames(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	return after - before
}

// insertRunStoreSession inserts the one bare session row the staged bodies
// hang from: identity only, no artifacts and no index state.
func insertRunStoreSession(t *testing.T, ctx context.Context, db *store.Store) schema.SessionID {
	t.Helper()
	rawID := "44444444-4444-4444-8444-444444444444"
	sessionID, err := ingest.NewSessionID(rawID)
	if err != nil {
		t.Fatal(err)
	}
	projectHash, err := ingest.NewProjectHash(strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	hostSlug, err := ingest.NewHostSlug("github.com-run-store-fixture")
	if err != nil {
		t.Fatal(err)
	}
	model, err := ingest.NewModelID("claude-opus-4-6")
	if err != nil {
		t.Fatal(err)
	}
	resolvedPath, err := ingest.NewResolvedPath("/test/path/session.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	ingested := int64(1700000120000)
	if err := db.InsertSessions(ctx, []ingest.StoreEntry{{
		Metadata: &ingest.UnifiedMetadata{
			SchemaVersion: ingest.CurrentSchemaVersion,
			SessionID:     sessionID,
			ModelHarness:  defaults.HarnessClaudeCode,
			Model:         model,
			HostSlug:      hostSlug,
			Timestamp:     ingest.TimestampInfo{Start: 1700000000000, End: 1700000060000, Ingested: &ingested},
			Source:        ingest.SourceInfo{FilePath: string(resolvedPath), Format: ingest.SourceFormatJSONL},
			Project:       ingest.ProjectInfo{Hash: projectHash, Name: "test-project", FilePath: "/home/test/project"},
			Stats:         ingest.StatsInfo{TurnCount: 10, ToolCallCount: 5, DurationMs: 60000, TokensIn: 100, TokensOut: 50},
		},
		Session: ingest.DiscoveredSession{SessionID: sessionID, Harness: defaults.HarnessClaudeCode, SourcePath: resolvedPath, SourceFormat: ingest.SourceFormatJSONL},
	}}); err != nil {
		t.Fatal(err)
	}
	return schema.SessionID(rawID)
}

// runStoreCandidate builds one minimal valid two-body candidate with its
// content bytes, so the run store stages through the production writer
// without a parser.
func runStoreCandidate(t *testing.T, sid schema.SessionID) (indexformat.V2, map[schema.SourceEntryRef][]byte) {
	t.Helper()
	texts := []string{"run store body one....................", "run store body two...................."}
	entries := make([]schema.SessionEntry, 0, len(texts))
	blobs := make(map[schema.SourceEntryRef][]byte, len(texts))
	content := make([]indexformat.ContentRecord, 0, len(texts))
	for i, text := range texts {
		ref := schema.SourceEntryRef("e_runstore")
		if i > 0 {
			ref = schema.SourceEntryRef("e_runstore_two")
		}
		preview := text
		entries = append(entries, schema.SessionEntry{
			SessionID: sid, EntryIndex: i, Harness: schema.HarnessClaudeCode,
			EntryType: schema.EntryTypeText, Role: schema.RoleUser,
			ContentPreview: &preview, SourceEntryRef: ref,
		})
		payload := []byte(text)
		sum := sha256.Sum256(payload)
		content = append(content, indexformat.ContentRecord{
			Ref: ref, RelativeBlob: "c_" + hex.EncodeToString(sum[:]) + ".blob",
			ByteLength: int64(len(payload)), Digest: hex.EncodeToString(sum[:]),
		})
		blobs[ref] = payload
	}
	inputCount := int64(1)
	return indexformat.V2{Generation: indexformat.Generation{
		ID:           "gen_runstore",
		Completeness: indexformat.GenerationCompletenessComplete,
		Metadata: schema.UnifiedMetadata{
			SchemaVersion: ingest.CurrentSchemaVersion,
			SessionID:     sid,
			ModelHarness:  defaults.HarnessClaudeCode,
			Model:         schema.ModelID("run-store-model"),
			Version:       "run-store-version",
			Stats:         schema.SessionStats{TurnCount: len(entries), InputSubmissionCount: &inputCount},
		},
		Main:                 indexformat.Partition{Entries: entries},
		Content:              content,
		SourceEvidenceDigest: strings.Repeat("b", 64),
	}}, blobs
}
