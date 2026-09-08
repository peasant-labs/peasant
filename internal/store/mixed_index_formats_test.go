package store_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/codemap"
	"github.com/peasant-labs/peasant/internal/export"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

type mixedStoreSnapshot struct {
	Index       indexStateSnapshot
	Annotations [][]string
	Owned       [][]string
	Ingested    int64
	Logs        [][]string
}

func mixedSnapshot(t *testing.T, db *store.Store, sid schema.SessionID) mixedStoreSnapshot {
	t.Helper()
	result := mixedStoreSnapshot{Index: readIndexSnapshot(t, db, sid)}
	result.Annotations = indexAnnotationRows(t, db, sid)
	conn := takeConn(t, db.Pool())
	defer db.Pool().Put(conn)
	result.Owned = mixedRawRows(t, conn, "SELECT * FROM test_packed_index WHERE session_id = ? ORDER BY ordinal", sid)
	result.Logs = mixedRawRows(t, conn, "SELECT * FROM index_log WHERE session_id = ? ORDER BY id", sid)
	if err := sqlitex.ExecuteTransient(conn, "SELECT ingested_ms FROM sessions WHERE session_id = ?", &sqlitex.ExecOptions{Args: []any{string(sid)}, ResultFunc: func(stmt *sqlite.Stmt) error { result.Ingested = stmt.ColumnInt64(0); return nil }}); err != nil {
		t.Fatal(err)
	}
	return result
}
func mixedRawRows(t *testing.T, conn *sqlite.Conn, query string, sid schema.SessionID) [][]string {
	t.Helper()
	var result [][]string
	if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{Args: []any{string(sid)}, ResultFunc: func(stmt *sqlite.Stmt) error {
		row := make([]string, stmt.ColumnCount())
		for column := range row {
			if stmt.ColumnType(column) == sqlite.TypeBlob {
				data := make([]byte, stmt.ColumnLen(column))
				stmt.ColumnBytes(column, data)
				row[column] = "blob:" + hex.EncodeToString(data)
			} else {
				row[column] = stmt.ColumnType(column).String() + ":" + stmt.ColumnText(column)
			}
		}
		result = append(result, row)
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	return result
}

func seedMixedIndex(t *testing.T, db *store.Store, sid schema.SessionID, harness schema.Harness, records []mixedFormatRecord, producer int, version int) {
	t.Helper()
	var result indexformat.Result = indexformat.V1{Entries: mixedEntries(sid, harness, records)}
	if version == 2 {
		result = mixedV2{Harness: harness, Encoding: mixedReversed, Records: records}
	}
	writes := db.IndexSessionEntryBatch(t.Context(), []ingest.SessionEntryWrite{{SessionID: sid, Result: result, IndexVersion: version, IndexerVersion: producer, IndexedAtMs: 1700000000100}})
	if !writes[0].Written || writes[0].Err != nil {
		t.Fatalf("seed mixed index: %+v", writes[0])
	}
}

func TestMixedIndexFormatsPersistConvertAndRollback(t *testing.T) {
	document := loadMixedIndexFormatFixtures(t)
	for _, row := range document.Cases {
		if row.Operation == mixedPipeline || row.Operation == mixedRegistration {
			continue
		}
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "mixed.db")
			options := []store.OpenOption{store.WithPoolSize(1), store.WithIndexFormats(mixedFormatHandler{fault: row.Fault})}
			if row.Fault != mixedNoEdge {
				options = append(options, store.WithIndexFormatConversions(mixedConversion(row.Fault)))
			}
			db, err := store.Open(path, options...)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			conn := takeConn(t, db.Pool())
			if err := sqlitex.ExecuteScript(conn, mixedFormatTables, nil); err != nil {
				t.Fatal(err)
			}
			db.Pool().Put(conn)
			seedMixedSession(t, db, document.LegacySession, schema.HarnessClaudeCode, document.Project)
			seedMixedSession(t, db, document.TargetSession, schema.HarnessCodex, document.Project)
			seedMixedIndex(t, db, document.LegacySession, schema.HarnessClaudeCode, document.Records, 15, 1)
			producer := row.Producer
			if producer == 0 && !row.UnknownProducer {
				producer = 14
			}
			records := document.Records
			if row.Empty {
				records = nil
			}
			version := 2
			if row.Operation == mixedConvert || row.Operation == mixedBatch {
				version = 1
			}
			if row.Fault != mixedNoIndex {
				seedMixedIndex(t, db, document.TargetSession, schema.HarnessCodex, records, producer, version)
			}
			if row.Fault != mixedNoIndex && len(records) > 0 {
				annotator, err := db.GetAnnotatorIDByName(t.Context(), "frustration-classifier")
				if err != nil {
					t.Fatal(err)
				}
				typeID, err := db.GetAnnotationTypeID(t.Context(), "quality.frustration_signal")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.CreateEntryAnnotation(t.Context(), ingest.EntryAnnotationParams{SessionID: string(document.TargetSession), EntryIndex: records[0].Ordinal, EndIndex: records[0].Ordinal + 1, AnnotatorID: annotator, AnnotationTypeID: typeID, Value: "detected"}); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.LogIndexEntry(t.Context(), ingest.IndexLogEntry{SessionID: document.TargetSession, Harness: schema.HarnessCodex, Outcome: ingest.IndexOutcomeIndexed, IndexerVersion: producer, IndexVersion: intPtr(version), EntriesCount: len(records), StartedAt: 100, FinishedAt: int64Ptr(101)}); err != nil {
				t.Fatal(err)
			}
			if row.Fault == mixedUnknownSource {
				conn := takeConn(t, db.Pool())
				if err := sqlitex.ExecuteTransient(conn, "UPDATE sessions SET index_format_version = 99 WHERE session_id = ?", &sqlitex.ExecOptions{Args: []any{string(document.TargetSession)}}); err != nil {
					t.Fatal(err)
				}
				db.Pool().Put(conn)
			}
			before, legacyBefore := mixedSnapshot(t, db, document.TargetSession), mixedSnapshot(t, db, document.LegacySession)
			switch row.Operation {
			case mixedReopen:
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				db, err = store.Open(path, store.WithPoolSize(1), store.WithIndexFormats(mixedFormatHandler{}))
				if err != nil {
					t.Fatal(err)
				}
				assertMixedReads(t, db, document)
				if after := mixedSnapshot(t, db, document.TargetSession); !reflect.DeepEqual(before, after) {
					t.Fatal("reopen/read changed stored evidence")
				}
			case mixedRewrite, mixedRepair:
				if row.Operation == mixedRepair {
					conn := takeConn(t, db.Pool())
					if err := sqlitex.ExecuteTransient(conn, "DELETE FROM session_entries_ext WHERE session_id = ?", &sqlitex.ExecOptions{Args: []any{string(document.TargetSession)}}); err != nil {
						t.Fatal(err)
					}
					db.Pool().Put(conn)
				}
				result := mixedV2{Harness: schema.HarnessCodex, Encoding: mixedReversedXOR, Records: records}
				writes := db.IndexSessionEntryBatch(t.Context(), []ingest.SessionEntryWrite{{SessionID: document.TargetSession, Result: result, IndexVersion: 2, IndexerVersion: producer, IndexedAtMs: 1700000000200}})
				if !writes[0].Written || writes[0].Err != nil {
					t.Fatalf("packed rewrite failed: %+v", writes[0])
				}
				after := mixedSnapshot(t, db, document.TargetSession)
				if reflect.DeepEqual(before.Owned, after.Owned) || !reflect.DeepEqual(before.Index.Hash, after.Index.Hash) || !reflect.DeepEqual(before.Index.Entries, after.Index.Entries) {
					t.Fatal("equal projection skipped owned representation or changed semantic data")
				}
				if row.Operation == mixedRewrite && !writes[0].Skipped {
					t.Fatal("equal canonical rows were replaced")
				}
				if row.Operation == mixedRepair && writes[0].Stats.ProjectionRepairRewrites == 0 {
					t.Fatal("missing derived rows were not repaired")
				}
				assertMixedReads(t, db, document)
			case mixedConvert:
				err := db.ConvertIndexFormat(t.Context(), document.TargetSession, 2)
				after := mixedSnapshot(t, db, document.TargetSession)
				if row.Fault != mixedNoFault {
					if err == nil || !reflect.DeepEqual(before, after) {
						t.Fatalf("failed conversion changed data: err=%v before=%+v after=%+v", err, before, after)
					}
					if row.Fault == mixedWrongResult && !strings.Contains(err.Error(), "declared format 2") {
						t.Fatalf("wrong result missed concrete validation: %v", err)
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					if after.Index.Format == nil || *after.Index.Format != 2 || after.Index.Producer != before.Index.Producer || !reflect.DeepEqual(after.Index.At, before.Index.At) || !reflect.DeepEqual(after.Index.Hash, before.Index.Hash) || !reflect.DeepEqual(after.Index.Entries, before.Index.Entries) || !reflect.DeepEqual(before.Logs, after.Logs) {
						t.Fatal("conversion changed parser history or canonical semantics")
					}
					if len(after.Owned) != len(records) {
						t.Fatal("conversion did not persist its actual representation")
					}
				}
			case mixedBatch:
				assertMixedBatchFailure(t, db, document, row, before)
				return
			case mixedDowngrade:
				write := db.IndexSessionEntryBatch(t.Context(), []ingest.SessionEntryWrite{{SessionID: document.TargetSession, Result: indexformat.V1{Entries: mixedEntries(document.TargetSession, schema.HarnessCodex, records)}, IndexVersion: 1, IndexerVersion: 99, IndexedAtMs: 1700000000999}})
				if write[0].Err == nil || write[0].Written || !reflect.DeepEqual(before, mixedSnapshot(t, db, document.TargetSession)) {
					t.Fatal("a newer parser silently downgraded the stored format")
				}
			case mixedUnsupported:
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				db, err = store.Open(path, store.WithPoolSize(1))
				if err != nil {
					t.Fatal(err)
				}
				_, err = db.ListEntries(t.Context(), document.TargetSession)
				var unsupported *store.UnsupportedIndexFormatError
				if !errors.As(err, &unsupported) || unsupported.Version != 2 {
					t.Fatalf("default reader accepted test-only V2: %v", err)
				}
				if _, err := db.SessionByID(t.Context(), string(document.TargetSession)); err != nil {
					t.Fatal("metadata-only access was blocked")
				}
				if _, err := db.ListEntries(t.Context(), document.LegacySession); err != nil {
					t.Fatal("unrelated V1 scope was blocked")
				}
			}
			if after := mixedSnapshot(t, db, document.LegacySession); !reflect.DeepEqual(legacyBefore, after) {
				t.Fatal("other harness index changed")
			}
			if ingest.HarvesterVersionRegistry[ingest.HarnessCodex].IndexVersion != 1 {
				t.Fatal("test advertised production V2")
			}
		})
	}
}

func assertMixedBatchFailure(t *testing.T, db *store.Store, document mixedFormatDocument, row mixedFormatCase, targetBefore mixedStoreSnapshot) {
	t.Helper()
	seedMixedSession(t, db, document.LaterSession, schema.HarnessClaudeCode, document.Project)
	seedMixedIndex(t, db, document.LaterSession, schema.HarnessClaudeCode, document.Records, 15, 1)
	legacyBefore, laterBefore := mixedSnapshot(t, db, document.LegacySession), mixedSnapshot(t, db, document.LaterSession)
	earlier := mixedEntries(document.LegacySession, schema.HarnessClaudeCode, document.Records)
	later := mixedEntries(document.LaterSession, schema.HarnessClaudeCode, document.Records)
	earlier[0].ContentPreview = strPtr(document.Records[0].Text + " earlier update")
	later[0].ContentPreview = strPtr(document.Records[0].Text + " later update")
	packed := mixedV2{Harness: schema.HarnessCodex, Encoding: mixedReversedXOR, Records: append([]mixedFormatRecord(nil), document.Records...)}
	if row.Fault == mixedInvalidPayload {
		packed.Records[0].Speaker = 99
	}
	var writes []ingest.SessionEntryWriteResult
	var panicValue any
	func() {
		defer func() { panicValue = recover() }()
		writes = db.IndexSessionEntryBatch(t.Context(), []ingest.SessionEntryWrite{
			{SessionID: document.LegacySession, Result: indexformat.V1{Entries: earlier}, IndexVersion: 1, IndexerVersion: 15, IndexedAtMs: 1700000000200},
			{SessionID: document.TargetSession, Result: packed, IndexVersion: 2, IndexerVersion: 16, IndexedAtMs: 1700000000200},
			{SessionID: document.LaterSession, Result: indexformat.V1{Entries: later}, IndexVersion: 1, IndexerVersion: 15, IndexedAtMs: 1700000000200},
		})
	}()
	if row.Fault == mixedPanic {
		if panicValue == nil {
			t.Fatal("handler panic was swallowed")
		}
	} else {
		if panicValue != nil {
			t.Fatalf("unexpected panic: %v", panicValue)
		}
		if len(writes) != 3 || writes[1].Err == nil || writes[1].Written {
			t.Fatalf("failed packed write was reported successful: %+v", writes)
		}
	}
	if after := mixedSnapshot(t, db, document.TargetSession); !reflect.DeepEqual(targetBefore, after) {
		t.Fatalf("failure committed partial owned data or changed target history: before=%+v after=%+v", targetBefore, after)
	}
	legacyAfter, laterAfter := mixedSnapshot(t, db, document.LegacySession), mixedSnapshot(t, db, document.LaterSession)
	if row.Fault == mixedCommitError || row.Fault == mixedPanic {
		if !reflect.DeepEqual(legacyBefore, legacyAfter) || !reflect.DeepEqual(laterBefore, laterAfter) {
			t.Fatal("outer transaction failure committed an earlier or later batch item")
		}
		if row.Fault == mixedCommitError {
			for _, result := range writes {
				if result.Written || result.Err == nil {
					t.Fatalf("commit failure retained completion: %+v", result)
				}
			}
		}
	} else if !writes[0].Written || !writes[2].Written || reflect.DeepEqual(legacyBefore, legacyAfter) || reflect.DeepEqual(laterBefore, laterAfter) {
		t.Fatal("per-session rollback discarded independent successful writes")
	}
}

func assertMixedReads(t *testing.T, db *store.Store, document mixedFormatDocument) {
	t.Helper()
	ctx := t.Context()
	entries, err := db.ListEntries(ctx, document.TargetSession)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(entries, mixedEntries(document.TargetSession, schema.HarnessCodex, document.Records)) {
		t.Fatalf("packed projection differs: %+v", entries)
	}
	bounded, err := db.ListEntriesRange(ctx, document.TargetSession, document.Records[0].Ordinal, document.Records[0].Ordinal)
	if err != nil || len(bounded) != 1 || bounded[0].EntryIndex != document.Records[0].Ordinal {
		t.Fatalf("packed range: %+v %v", bounded, err)
	}
	first, err := db.FirstUserMessage(ctx, string(document.TargetSession))
	if err != nil || first != document.Records[0].Text {
		t.Fatalf("packed preview: %q %v", first, err)
	}
	maximum, err := db.MaxEntryIndex(ctx, document.TargetSession)
	if err != nil || maximum != document.Records[len(document.Records)-1].Ordinal {
		t.Fatalf("packed maximum: %d %v", maximum, err)
	}
	annotations, err := db.GetAnnotationsForEntry(ctx, string(document.TargetSession), document.Records[0].Ordinal)
	if err != nil || len(annotations) != 1 || annotations[0].TargetEntryIndex == nil || *annotations[0].TargetEntryIndex != document.Records[0].Ordinal {
		t.Fatalf("packed annotation anchor: %+v %v", annotations, err)
	}
	conn := takeConn(t, db.Pool())
	wantOwned := make(map[int]string)
	for _, record := range document.Records {
		wantOwned[record.Ordinal] = record.Text
	}
	gotOwned := make(map[int]string)
	if err := sqlitex.ExecuteTransient(conn, "SELECT ordinal, body, encoding FROM test_packed_index WHERE session_id = ? ORDER BY ordinal", &sqlitex.ExecOptions{Args: []any{string(document.TargetSession)}, ResultFunc: func(stmt *sqlite.Stmt) error {
		body := make([]byte, stmt.ColumnLen(1))
		stmt.ColumnBytes(1, body)
		ordinal := stmt.ColumnInt(0)
		if bytes.Equal(body, []byte(wantOwned[ordinal])) {
			t.Error("test payload was not an incompatible stored encoding")
		}
		gotOwned[ordinal] = string(encodeMixedBody(string(body), mixedEncoding(stmt.ColumnInt(2))))
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	db.Pool().Put(conn)
	if !maps.Equal(gotOwned, wantOwned) {
		t.Fatalf("reopened owned representation=%v, want %v", gotOwned, wantOwned)
	}
	search, err := codemap.NewService(db, nil, nil, sessionvisibility.All()).Search(ctx, "needle", 50)
	if err != nil {
		t.Fatal(err)
	}
	want := make(map[string]bool)
	for _, record := range document.Records {
		want[fmt.Sprintf("%s/%d", document.LegacySession, record.Ordinal)] = true
		want[fmt.Sprintf("%s/%d", document.TargetSession, record.Ordinal)] = true
	}
	got := make(map[string]bool)
	for _, hit := range search.Results {
		got[fmt.Sprintf("%s/%d", hit.SessionID, hit.EntryIndex)] = true
	}
	if !maps.Equal(got, want) {
		t.Fatalf("mixed FTS coordinates=%v, want %v", got, want)
	}
	if _, err := export.ExportSession(ctx, db, testutil.NewMemFS(), string(document.TargetSession)); err != nil {
		t.Fatal(err)
	}
}

func TestMixedIndexFormatRegistrationRejectsInvalidEdgesBeforeOpening(t *testing.T) {
	for _, row := range loadMixedIndexFormatFixtures(t).Cases {
		if row.Operation != mixedRegistration {
			continue
		}
		t.Run(row.Name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "must-not-exist.db")
			edge := mixedConversion(mixedNoFault)
			options := []store.OpenOption{store.WithIndexFormats(mixedFormatHandler{})}
			switch row.Fault {
			case mixedDuplicateHandler:
				options = append(options, store.WithIndexFormats(mixedFormatHandler{}))
			case mixedDuplicateEdge:
				options = append(options, store.WithIndexFormatConversions(edge, edge))
			case mixedMissingHandler:
				options = []store.OpenOption{store.WithIndexFormatConversions(edge)}
			case mixedReverse:
				edge.FromVersion, edge.ToVersion = 2, 1
				options = append(options, store.WithIndexFormatConversions(edge))
			case mixedNilConverter:
				edge.Convert = nil
				options = append(options, store.WithIndexFormatConversions(edge))
			}
			db, err := store.Open(path, options...)
			if err == nil {
				db.Close()
				t.Fatal("invalid registry opened a database")
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid registration created the database: %v", err)
			}
		})
	}
}

type mixedCodexIndexer struct{ *ingest.CodexIndexer }

var _ ingest.VersionedTranscriptIndexer = (*mixedCodexIndexer)(nil)

func (indexer *mixedCodexIndexer) IndexTranscriptResult(ctx context.Context, session ingest.DiscoveredSession) (indexformat.Result, error) {
	entries, err := indexer.CodexIndexer.IndexTranscript(ctx, session)
	if err != nil {
		return nil, err
	}
	return packCodexFixtureEntries(entries)
}
func (indexer *mixedCodexIndexer) IndexTranscriptBytesResult(ctx context.Context, session ingest.DiscoveredSession, data []byte) (indexformat.Result, error) {
	entries, err := indexer.CodexIndexer.IndexTranscriptBytes(ctx, session, data)
	if err != nil {
		return nil, err
	}
	return packCodexFixtureEntries(entries)
}
func packCodexFixtureEntries(entries []schema.SessionEntry) (indexformat.Result, error) {
	if len(entries) == 0 {
		return nil, fmt.Errorf("fixture Codex wrapper cannot certify an empty legacy parser result")
	}
	result := mixedV2{Harness: schema.HarnessCodex, Encoding: mixedReversed}
	for _, entry := range entries {
		if entry.EntryType != schema.EntryTypeText || entry.Role != schema.RoleUser || entry.ContentPreview == nil {
			return nil, fmt.Errorf("fixture format only packs the fixture's plain Codex user text")
		}
		result.Records = append(result.Records, mixedFormatRecord{Ordinal: entry.EntryIndex, Speaker: mixedSpeakerUser, Text: *entry.ContentPreview, Timestamp: entry.TimestampMs, RawBytes: entry.RawByteLength})
	}
	return result, nil
}

func TestMixedIndexFormatsPipelineUpgradesOnlyItsDeclaringHarness(t *testing.T) {
	document := loadMixedIndexFormatFixtures(t)
	for _, row := range document.Cases {
		if row.Operation != mixedPipeline {
			continue
		}
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			db, err := store.Open(filepath.Join(t.TempDir(), "pipeline.db"), store.WithPoolSize(1), store.WithIndexFormats(mixedFormatHandler{}))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			conn := takeConn(t, db.Pool())
			if err := sqlitex.ExecuteScript(conn, mixedFormatTables, nil); err != nil {
				t.Fatal(err)
			}
			db.Pool().Put(conn)
			legacyMeta := seedMixedSession(t, db, document.LegacySession, schema.HarnessClaudeCode, document.Project)
			targetMeta := seedMixedSession(t, db, document.TargetSession, schema.HarnessCodex, document.Project)
			// This session represents an unrelated current parser, not historical output.
			legacyTarget := ingest.HarvesterVersionRegistry[schema.HarnessClaudeCode]
			seedMixedIndex(t, db, document.LegacySession, schema.HarnessClaudeCode, document.Records, legacyTarget.IndexerVersion, 1)
			seedMixedIndex(t, db, document.TargetSession, schema.HarnessCodex, document.Records, 14, 1)
			fs := testutil.NewMemFS()
			files := make(map[string][]byte)
			writeManaged := func(meta *schema.UnifiedMetadata, transcript string) {
				basePath := filepath.Join("/managed", string(meta.HostSlug), string(meta.SessionID), string(meta.SessionID))
				data, err := json.Marshal(meta)
				if err != nil {
					t.Fatal(err)
				}
				files[basePath+"--metadata.json"] = data
				files[basePath+"--transcript.jsonl"] = []byte(transcript)
			}
			writeManaged(legacyMeta, `{"type":"user","message":{"role":"user","content":"unchanged legacy fixture"}}`)
			writeManaged(targetMeta, document.CodexTranscript)
			for path, data := range files {
				if err := fs.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			versions := maps.Clone(ingest.HarvesterVersionRegistry)
			target := versions[ingest.HarnessCodex]
			target.IndexerVersion, target.IndexVersion = 16, 2
			versions[ingest.HarnessCodex] = target
			indexers := map[ingest.Harness]ingest.TranscriptIndexer{
				ingest.HarnessClaudeCode: ingest.NewClaudeIndexer(fs),
				ingest.HarnessCodex:      &mixedCodexIndexer{ingest.NewCodexIndexer(fs)},
			}
			// First establish real file/SQL mirrors and the unrelated parser's
			// actual input proof. Unknown historical input requires verification;
			// this case starts after that work, before Codex adopts its new format.
			publisher, err := ingest.NewArtifactPublisher(fs, "/managed", ingest.ArtifactPublisherOptions{Mirror: db})
			if err != nil {
				t.Fatal(err)
			}
			for _, meta := range []*schema.UnifiedMetadata{legacyMeta, targetMeta} {
				path := ingest.SessionMetadataPath("/managed", string(meta.HostSlug), string(meta.SessionID), "")
				if _, _, err := publisher.ReconcileStored(t.Context(), meta.SessionID, path, nil); err != nil {
					t.Fatal(err)
				}
			}
			legacyHarness := schema.HarnessClaudeCode
			prepare, err := ingest.NewPipeline(fs, testutil.DefaultGitResolver(), ingest.DefaultAdapterRegistry, ingest.PipelineConfig{Reindex: true, Force: true, Harness: &legacyHarness, OutputDir: "/managed"}, ingest.WithStore(db), ingest.WithMetricsStore(db), ingest.WithIndexers(indexers), ingest.WithHarvesterVersions(versions))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := prepare.Run(t.Context()); err != nil {
				t.Fatalf("establish unrelated current input: %v", err)
			}
			verified, err := db.ReadIndexState(t.Context(), document.LegacySession)
			if err != nil || verified == nil || verified.IndexedInputHash == nil || verified.IndexerVersion != legacyTarget.IndexerVersion {
				t.Fatalf("unrelated input is not verified at its current producer: %+v %v", verified, err)
			}
			legacyBefore := mixedSnapshot(t, db, document.LegacySession)
			for path := range files {
				files[path], err = fs.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
			}
			pipeline, err := ingest.NewPipeline(fs, testutil.DefaultGitResolver(), ingest.DefaultAdapterRegistry, ingest.PipelineConfig{Reindex: true, OutputDir: "/managed"}, ingest.WithStore(db), ingest.WithMetricsStore(db), ingest.WithIndexLogger(db), ingest.WithIndexers(indexers), ingest.WithHarvesterVersions(versions))
			if err != nil {
				t.Fatal(err)
			}
			result, err := pipeline.Run(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if result.Summary.Indexed != 1 || len(result.IndexLog) != 1 || result.IndexLog[0].SessionID != document.TargetSession || result.IndexLog[0].IndexerVersion != 16 || result.IndexLog[0].IndexVersion == nil || *result.IndexLog[0].IndexVersion != 2 {
				t.Fatalf("mixed pipeline output did not respect declarations: %+v", result)
			}
			after := mixedSnapshot(t, db, document.TargetSession)
			if after.Index.Format == nil || *after.Index.Format != 2 || after.Index.Producer != 16 || len(after.Owned) != 1 {
				t.Fatalf("actual format/producer not persisted: %+v", after)
			}
			if !reflect.DeepEqual(legacyBefore, mixedSnapshot(t, db, document.LegacySession)) {
				t.Fatal("other harness changed because Codex adopted a new format")
			}
			for path, before := range files {
				after, err := fs.ReadFile(path)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatalf("index maintenance changed managed input %s: %v", path, err)
				}
			}
			if ingest.HarvesterVersionRegistry[ingest.HarnessCodex].IndexVersion != 1 {
				t.Fatal("fixture changed global production declaration")
			}
		})
	}
}
