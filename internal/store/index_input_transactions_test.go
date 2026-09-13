package store_test

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/index_input_transactions.yaml
var indexInputTransactionsYAML []byte

const indexInputIntervening mixedFormatOperation = "intervening"

type indexInputTransactionCase struct {
	Name         string               `yaml:"name"`
	Operation    mixedFormatOperation `yaml:"operation"`
	Fault        mixedFormatFault     `yaml:"fault"`
	Stale        bool                 `yaml:"stale"`
	UnknownInput bool                 `yaml:"unknownInput"`
	Producer     *int                 `yaml:"producer"`
	MutationSQL  string               `yaml:"mutationSQL"`
}

func loadIndexInputTransactionFixtures(t *testing.T) []indexInputTransactionCase {
	t.Helper()
	var document struct {
		RequiredNames []string                    `yaml:"requiredNames"`
		Cases         []indexInputTransactionCase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(indexInputTransactionsYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatal("index input transactions require one fixture document")
	}
	names := make(map[string]bool)
	for _, row := range document.Cases {
		if row.Name == "" || names[row.Name] {
			t.Fatalf("invalid transaction fixture %q", row.Name)
		}
		if row.Operation != mixedBatch && row.Operation != mixedConvert && row.Operation != indexInputIntervening {
			t.Fatalf("unknown transaction operation %q", row.Operation)
		}
		switch row.Fault {
		case mixedNoFault, mixedWriteError, mixedCommitError, mixedPanic, mixedConvertError, mixedWrongResult:
		default:
			t.Fatalf("unknown transaction fault %q", row.Fault)
		}
		names[row.Name] = true
	}
	if err := testutil.RequireFixtureNames("index input transaction", "case", document.RequiredNames, names); err != nil {
		t.Fatal(err)
	}
	return document.Cases
}

func TestIndexInputStateBatchAndConversionTransactions(t *testing.T) {
	corpus := loadMixedIndexFormatFixtures(t)
	input := loadIndexInputFixtures(t)
	for _, row := range loadIndexInputTransactionFixtures(t) {
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			conversion := mixedConversion(row.Fault)
			convert := conversion.Convert
			conversion.Convert = func(ctx context.Context, conn *sqlite.Conn, sid schema.SessionID) (indexformat.Result, error) {
				result, err := convert(ctx, conn, sid)
				if err == nil && row.MutationSQL != "" {
					err = sqlitex.ExecuteTransient(conn, row.MutationSQL, &sqlitex.ExecOptions{Args: []any{string(sid)}})
				}
				return result, err
			}
			db, err := store.Open(filepath.Join(t.TempDir(), "conditional.db"), store.WithPoolSize(1), store.WithIndexFormats(mixedFormatHandler{fault: row.Fault}), store.WithIndexFormatConversions(conversion))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			conn := takeConn(t, db.Pool())
			if err := sqlitex.ExecuteScript(conn, mixedFormatTables, nil); err != nil {
				t.Fatal(err)
			}
			db.Pool().Put(conn)
			ids := []schema.SessionID{corpus.LegacySession, corpus.TargetSession, corpus.LaterSession}
			producer := 14
			if row.Producer != nil {
				producer = *row.Producer
			}
			for _, sid := range ids {
				seedMixedSession(t, db, sid, schema.HarnessCodex, corpus.Project)
				seedMixedIndex(t, db, sid, schema.HarnessCodex, corpus.Records, producer, 1)
				inputSQL(t, db, sid, "UPDATE sessions SET artifact_hash = ? WHERE session_id = ?", input.ArtifactHash)
				if !row.UnknownInput {
					inputSQL(t, db, sid, "UPDATE sessions SET indexed_input_hash = ? WHERE session_id = ?", input.PriorInputHash)
				}
			}
			writes := make([]ingest.SessionEntryWrite, 0, len(ids))
			for _, sid := range ids {
				writes = append(writes, ingest.SessionEntryWrite{SessionID: sid, Result: indexformat.V1{Entries: mixedEntries(sid, schema.HarnessCodex, corpus.Records)}, IndexVersion: 1, IndexerVersion: 15, IndexedAtMs: 1700000000200, ExpectedState: readInputState(t, db, sid), IndexedInputHash: &input.NewInputHash})
			}
			writes[1].IndexVersion = 2
			writes[1].Result = mixedV2{Harness: schema.HarnessCodex, Encoding: mixedReversedXOR, Records: corpus.Records}
			if row.Stale {
				inputSQL(t, db, corpus.TargetSession, "UPDATE sessions SET indexed_at = indexed_at + 1 WHERE session_id = ?")
			}
			beforeStates := make([]*ingest.SessionIndexState, len(ids))
			beforeRows := make([]mixedStoreSnapshot, len(ids))
			for i, sid := range ids {
				beforeStates[i], beforeRows[i] = readInputState(t, db, sid), mixedSnapshot(t, db, sid)
			}
			if row.Operation == indexInputIntervening {
				assertInterveningIndexWrite(t, db, writes[1])
				return
			}
			if row.Operation == mixedConvert {
				err := db.ConvertIndexFormat(t.Context(), corpus.TargetSession, 2)
				after, rows := readInputState(t, db, corpus.TargetSession), mixedSnapshot(t, db, corpus.TargetSession)
				if row.MutationSQL != "" || row.Fault != mixedNoFault {
					if err == nil || !reflect.DeepEqual(beforeStates[1], after) || !reflect.DeepEqual(beforeRows[1], rows) {
						t.Fatalf("refused conversion changed proof or rows: err=%v", err)
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					want := *beforeStates[1]
					want.IndexVersion = intPtr(2)
					if !reflect.DeepEqual(&want, after) || !reflect.DeepEqual(beforeRows[1].Index.Entries, rows.Index.Entries) {
						t.Fatal("conversion changed historical input, producer, or canonical output")
					}
				}
				return
			}
			var results []ingest.SessionEntryWriteResult
			var panicValue any
			func() {
				defer func() { panicValue = recover() }()
				results = db.IndexSessionEntryBatch(t.Context(), writes)
			}()
			if (panicValue != nil) != (row.Fault == mixedPanic) {
				t.Fatalf("unexpected panic result: %v", panicValue)
			}
			for i, sid := range ids {
				after, rows := readInputState(t, db, sid), mixedSnapshot(t, db, sid)
				refused := row.Fault == mixedCommitError || row.Fault == mixedPanic || i == 1 && (row.Stale || row.Fault == mixedWriteError)
				if refused {
					if !reflect.DeepEqual(beforeStates[i], after) || !reflect.DeepEqual(beforeRows[i], rows) {
						t.Fatalf("failed item %s changed last-good data", sid)
					}
					if row.Fault != mixedPanic && (results[i].Written || results[i].Err == nil) {
						t.Fatalf("failed item reported success: %+v", results[i])
					}
					if row.Stale && i == 1 {
						var stale *ingest.StaleIndexWorkError
						if !errors.As(results[i].Err, &stale) {
							t.Fatalf("stale item error=%v", results[i].Err)
						}
					}
				} else {
					if results[i].Err != nil || !results[i].Written || after.IndexedInputHash == nil || *after.IndexedInputHash != input.NewInputHash || after.IndexerVersion != 15 || after.IndexedAt == nil || *after.IndexedAt != writes[i].IndexedAtMs {
						t.Fatalf("healthy item did not commit complete proof: result=%+v state=%+v", results[i], after)
					}
					if i == 1 && (after.IndexVersion == nil || *after.IndexVersion != 2 || len(rows.Owned) == 0) {
						t.Fatal("equal projection skipped concrete target representation")
					}
				}
			}
		})
	}
}

func assertInterveningIndexWrite(t *testing.T, db *store.Store, write ingest.SessionEntryWrite) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	initial := db.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{write})[0]
	if initial.Err != nil || !initial.Written {
		t.Fatalf("initial proof write failed: %+v", initial)
	}
	ready, resume := make(chan struct{}), make(chan struct{})
	finished := make(chan ingest.SessionEntryWriteResult, 1)
	// The parser's captured state remains owned by this goroutine. A completed
	// competing writer must invalidate it even when producer/format/output match.
	go func() {
		state, err := db.ReadIndexState(ctx, write.SessionID)
		if err != nil {
			finished <- ingest.SessionEntryWriteResult{Err: err}
			return
		}
		write.ExpectedState = state
		close(ready)
		select {
		case <-resume:
		case <-ctx.Done():
			finished <- ingest.SessionEntryWriteResult{Err: ctx.Err()}
			return
		}
		finished <- db.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{write})[0]
	}()
	select {
	case <-ready:
	case result := <-finished:
		t.Fatalf("capture failed before barrier: %v", result.Err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	newer := write
	newer.IndexedAtMs++
	result := db.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{newer})[0]
	if result.Err != nil || !result.Written {
		t.Fatalf("competing writer failed: %+v", result)
	}
	state, rows := readInputState(t, db, write.SessionID), mixedSnapshot(t, db, write.SessionID)
	close(resume)
	select {
	case staleResult := <-finished:
		var stale *ingest.StaleIndexWorkError
		if !errors.As(staleResult.Err, &stale) || staleResult.Written {
			t.Fatalf("obsolete captured parser result was accepted: %+v", staleResult)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if !reflect.DeepEqual(state, readInputState(t, db, write.SessionID)) || !reflect.DeepEqual(rows, mixedSnapshot(t, db, write.SessionID)) {
		t.Fatal("obsolete parser changed the completed writer's output or proof")
	}
}
