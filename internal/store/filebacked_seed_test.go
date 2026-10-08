package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// seedFileBackedGeneration installs one candidate into the file-backed
// storage Release N still reads: the projection generation row with its
// flattened JSON documents, the entry rows, the content records, the
// aliases, the sections, and the owned generation directory with its
// manifest and content blobs. The reshaped children (segments, evidence)
// stay empty: the snapshot reader does not serve them. The session points
// at the new generation with the same mirrors the activation writes, so
// file-backed reads observe exactly what a pre-harmonized activation left
// behind. Harmonized reads (the new catalog) belong to the readers change,
// which wires them to the same consumers.
func seedFileBackedGeneration(t *testing.T, s *Store, root string, sid schema.SessionID, v2 indexformat.V2, blobs map[schema.SourceEntryRef][]byte, full bool) {
	t.Helper()
	ctx := context.Background()
	filled := filledCandidateForValidation(t, v2, blobs)
	generation := filled.Generation
	genDir := filepath.Join(root, string(sid), "generations", generation.ID)
	if err := os.MkdirAll(genDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal(generation)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(genDir, "manifest.json"), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, record := range generation.Content {
		payload, ok := blobs[record.Ref]
		if !ok {
			t.Fatalf("no staged bytes for content record %q", record.Ref)
		}
		blobPath := filepath.Join(genDir, record.RelativeBlob)
		if err := os.MkdirAll(filepath.Dir(blobPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(blobPath, payload, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	metadataJSON, err := json.Marshal(generation.Metadata)
	if err != nil {
		s.pool.Put(conn)
		t.Fatal(err)
	}
	titleRefsJSON, err := json.Marshal(generation.TitleRefs)
	if err != nil {
		t.Fatal(err)
	}
	var inputCount any
	if generation.Metadata.Stats.InputSubmissionCount != nil {
		inputCount = *generation.Metadata.Stats.InputSubmissionCount
	}
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_projection_generations(session_id, generation_id, metadata_json, title_refs_json, input_submission_count, source_evidence_digest, completeness, index_format_version, installed_at_ms, activated_at_ms) VALUES (?, ?, ?, ?, ?, ?, ?, 2, 1, 1)`, &sqlitex.ExecOptions{Args: []any{
		string(sid), generation.ID, string(metadataJSON), string(titleRefsJSON), inputCount, generation.SourceEvidenceDigest, string(generation.Completeness),
	}}); err != nil {
		t.Fatalf("seed file-backed generation row: %v", err)
	}
	insertEntries := func(partition int, entries []schema.SessionEntry) {
		t.Helper()
		for i := range entries {
			entry := entries[i]
			encoded, err := json.Marshal(entry)
			if err != nil {
				t.Fatal(err)
			}
			var ref any
			if entry.SourceEntryRef != "" {
				ref = string(entry.SourceEntryRef)
			}
			if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_projection_entries(session_id, generation_id, partition_id, entry_index, source_entry_ref, entry_json) VALUES (?, ?, ?, ?, ?, ?)`, &sqlitex.ExecOptions{Args: []any{
				string(sid), generation.ID, partition, entry.EntryIndex, ref, string(encoded),
			}}); err != nil {
				t.Fatalf("seed file-backed entry: %v", err)
			}
		}
	}
	insertEntries(0, generation.Main.Entries)
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_projection_sections(session_id, generation_id, partition_id, earlier_state) VALUES (?, ?, 0, NULL)`, &sqlitex.ExecOptions{Args: []any{
		string(sid), generation.ID,
	}}); err != nil {
		t.Fatalf("seed file-backed main section: %v", err)
	}
	for i := range generation.Earlier {
		section := generation.Earlier[i]
		if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_projection_sections(session_id, generation_id, partition_id, earlier_state) VALUES (?, ?, ?, ?)`, &sqlitex.ExecOptions{Args: []any{
			string(sid), generation.ID, i + 1, string(section.State),
		}}); err != nil {
			t.Fatalf("seed file-backed earlier section: %v", err)
		}
		insertEntries(i+1, section.Content.Entries)
	}
	for _, record := range generation.Content {
		if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_projection_content(session_id, generation_id, source_entry_ref, relative_blob, byte_length, integrity_digest) VALUES (?, ?, ?, ?, ?, ?)`, &sqlitex.ExecOptions{Args: []any{
			string(sid), generation.ID, string(record.Ref), record.RelativeBlob, record.ByteLength, record.Digest,
		}}); err != nil {
			t.Fatalf("seed file-backed content: %v", err)
		}
	}
	for _, alias := range generation.Aliases {
		if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_projection_aliases(session_id, generation_id, native_key, source_entry_ref) VALUES (?, ?, ?, ?)`, &sqlitex.ExecOptions{Args: []any{
			string(sid), generation.ID, alias.NativeKey, string(alias.Ref),
		}}); err != nil {
			t.Fatalf("seed file-backed alias: %v", err)
		}
	}
	if err := pointSessionAtGenerationOnConn(conn, sid, generation); err != nil {
		t.Fatalf("point session at seeded generation: %v", err)
	}
	// The v62 backfill gives every native session its captured-stats row
	// before Release N; a synthetic file-backed seed mirrors that so the
	// moved readers find the capture's measurements. The stamp stays at the
	// install time (1) so any later activation merge wins.
	seedDoc, err := json.Marshal(generation.Metadata.Stats)
	if err != nil {
		t.Fatal(err)
	}
	seedDocText := string(seedDoc)
	turnCount := generation.Metadata.Stats.TurnCount
	toolCallCount := generation.Metadata.Stats.ToolCallCount
	subagentCount := generation.Metadata.Stats.SubagentCount
	durationMs := generation.Metadata.Stats.DurationMs
	tokensIn := generation.Metadata.Stats.TokensIn
	tokensOut := generation.Metadata.Stats.TokensOut
	if _, err := upsertCapturedStatsOnConn(conn, CapturedStats{
		SessionID:            sid,
		TurnCount:            &turnCount,
		InputSubmissionCount: generation.Metadata.Stats.InputSubmissionCount,
		ToolCallCount:        &toolCallCount,
		SubagentCount:        &subagentCount,
		DurationMs:           &durationMs,
		TokensIn:             &tokensIn,
		TokensOut:            &tokensOut,
		ThoughtTokens:        generation.Metadata.Stats.ThoughtTokens,
		CachedReadTokens:     generation.Metadata.Stats.CachedReadTokens,
		CachedWriteTokens:    generation.Metadata.Stats.CachedWriteTokens,
		SeedJSON:             &seedDocText,
		Source:               StatsSourceHarness,
		UpdatedAtMs:          1,
	}); err != nil {
		t.Fatalf("seed captured stats row: %v", err)
	}
	// The mirror batch takes its own connection; release this one first.
	s.pool.Put(conn)
	// The mirror rows come from the production V1 batch write over the main
	// entries: the same rows, ext keys, commands, hashes, and (for a full
	// capture) chunks and capture certificate the pre-harmonized V2
	// activation persisted beside its projection tables.
	capture := ingest.SessionContentCaptureWrite{
		Status:          ingest.ContentCaptureIncomplete,
		SourceAuthority: ingest.ContentSourceNone,
		CaptureFormat:   ingest.ContentCaptureFormatPreviewOnly,
	}
	if full {
		capture = ingest.SessionContentCaptureWrite{
			Status:           ingest.ContentCaptureComplete,
			SourceAuthority:  ingest.ContentSourceNewIngest,
			TranscriptOrigin: ingest.TranscriptOriginFile,
			CaptureFormat:    ingest.ContentCaptureFormatFull,
			CapturedAtMs:     1,
		}
	}
	results := s.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{
		SessionID:          sid,
		Result:             indexformat.V1{Entries: generation.Main.Entries},
		IndexVersion:       1,
		IndexerVersion:     1,
		IndexedAtMs:        1,
		RequireFullContent: full,
		ContentCapture:     capture,
	}})
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("seed file-backed mirror rows: %+v", results)
	}
}
