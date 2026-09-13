package store_test

import (
	"bytes"
	_ "embed"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/index_annotation_writes.yaml
var indexAnnotationWritesYAML []byte

type indexAnnotationWriteOperation string

const (
	indexAnnotationCreate          indexAnnotationWriteOperation = "create"
	indexAnnotationSupersede       indexAnnotationWriteOperation = "supersede"
	indexAnnotationBatch           indexAnnotationWriteOperation = "batch"
	indexAnnotationClassifier      indexAnnotationWriteOperation = "classifier"
	indexAnnotationClassifierBatch indexAnnotationWriteOperation = "classifier-batch"
	indexAnnotationRunState        indexAnnotationWriteOperation = "run-state"
)

type indexAnnotationWriteCase struct {
	Name      string                        `yaml:"name"`
	Operation indexAnnotationWriteOperation `yaml:"operation"`
}

func loadIndexAnnotationWriteFixtures(t *testing.T) []indexAnnotationWriteCase {
	t.Helper()
	var document struct {
		RequiredNames []string                   `yaml:"requiredNames"`
		Cases         []indexAnnotationWriteCase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(indexAnnotationWritesYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("annotation writes fixture requires one document: %v", err)
	}
	names := make(map[string]bool)
	for _, row := range document.Cases {
		if row.Name == "" || names[row.Name] {
			t.Fatalf("invalid annotation write case %+v", row)
		}
		switch row.Operation {
		case indexAnnotationCreate, indexAnnotationSupersede, indexAnnotationBatch, indexAnnotationClassifier, indexAnnotationClassifierBatch, indexAnnotationRunState:
		default:
			t.Fatalf("unknown annotation write %q", row.Operation)
		}
		names[row.Name] = true
	}
	if err := testutil.RequireFixtureNames("index annotation writes", "case", document.RequiredNames, names); err != nil {
		t.Fatal(err)
	}
	return document.Cases
}

func TestIndexFormatAnnotationWritersRefuseUnknownCoordinates(t *testing.T) {
	for _, row := range loadIndexAnnotationWriteFixtures(t) {
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			db := openTestStore(t)
			ctx := t.Context()
			sid := schema.SessionID(testutil.TestSessionUUID)
			seedSession(t, db, string(sid))
			if err := db.IndexSessionEntries(ctx, sid, batchTestEntries(sid, "current", 1)); err != nil {
				t.Fatal(err)
			}
			annotator := seedAnnotatorIDForTest(t, db)
			typeID := seedAnnotationTypeIDForTest(t, db, "quality.frustration_signal")
			params := store.CreateAnnotationParams{EntryTarget: &store.EntryTarget{SessionID: string(sid), EntryIndex: 0, EndIndex: 1}, AnnotatorID: annotator, AnnotationTypeID: typeID, Value: "detected"}
			oldID, err := db.CreateAnnotation(ctx, params)
			if err != nil {
				t.Fatal(err)
			}
			conn := takeConn(t, db.Pool())
			if err := sqlitex.ExecuteTransient(conn, "UPDATE sessions SET index_format_version = 99 WHERE session_id = ?", &sqlitex.ExecOptions{Args: []any{string(sid)}}); err != nil {
				t.Fatal(err)
			}
			db.Pool().Put(conn)
			before := indexAnnotationRows(t, db, sid)
			write := classifierEntryWrite(typeID, annotator, string(sid), 0, "updated", strings.Repeat("a", 64))
			state := ingest.AnnotationRunState{SessionID: sid, SessionEntriesHash: strings.Repeat("b", 64), ComputeVersion: 1, ClassifierVersion: 1, AnnotatedAt: time.Unix(1700000000, 0)}
			switch row.Operation {
			case indexAnnotationCreate:
				_, err = db.CreateAnnotation(ctx, params)
			case indexAnnotationSupersede:
				_, err = db.CreateAnnotationAndSupersede(ctx, write.Create, oldID, write.ContentHash)
			case indexAnnotationBatch:
				_, err = db.BatchCreateAnnotations(ctx, []store.CreateAnnotationParams{params})
			case indexAnnotationClassifier:
				results := db.ApplyClassifierAnnotations(ctx, []ingest.ClassifierAnnotationWrite{write})
				err = results[0].Err
			case indexAnnotationClassifierBatch:
				results := db.ApplyClassifierAnnotationBatches(ctx, []ingest.SessionAnnotationBatch{{SessionID: sid, Writes: []ingest.SessionAnnotationWrite{{Write: write}}, RunState: &state}})
				err = results[0].Err
			case indexAnnotationRunState:
				err = db.SaveAnnotationRunState(ctx, state)
			}
			var unsupported *store.UnsupportedIndexFormatError
			if !errors.As(err, &unsupported) {
				t.Fatalf("unknown-coordinate write was not refused: %v", err)
			}
			if after := indexAnnotationRows(t, db, sid); !reflect.DeepEqual(before, after) {
				t.Fatalf("refused write changed annotations/anchors/state: before=%v after=%v", before, after)
			}
		})
	}
}

func indexAnnotationRows(t *testing.T, db *store.Store, sid schema.SessionID) [][]string {
	t.Helper()
	conn := takeConn(t, db.Pool())
	defer db.Pool().Put(conn)
	var rows [][]string
	if err := sqlitex.ExecuteTransient(conn, `SELECT a.id, a.value, a.content_hash, a.superseded_by,
te.session_id, te.entry_index, te.end_index, ata.state, ars.session_entries_hash, ars.annotated_at
FROM annotations a
LEFT JOIN annotation_target_entries te ON te.annotation_id = a.id
LEFT JOIN annotation_target_anchors ata ON ata.annotation_id = a.id
LEFT JOIN annotation_run_state ars ON ars.session_id = ?
ORDER BY a.id`, &sqlitex.ExecOptions{Args: []any{string(sid)}, ResultFunc: func(stmt *sqlite.Stmt) error {
		row := make([]string, stmt.ColumnCount())
		for column := range row {
			row[column] = stmt.ColumnType(column).String() + ":" + stmt.ColumnText(column)
		}
		rows = append(rows, row)
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	return rows
}
