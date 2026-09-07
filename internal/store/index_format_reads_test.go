package store_test

import (
	"bytes"
	_ "embed"
	"errors"
	"io"
	"testing"

	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/codemap"
	"github.com/peasant-labs/peasant/internal/export"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/index_format_reads.yaml
var indexFormatReadsYAML []byte

type indexReadOperation string

const (
	indexReadEntries            indexReadOperation = "entries"
	indexReadRange              indexReadOperation = "range"
	indexReadMaximum            indexReadOperation = "maximum"
	indexReadExists             indexReadOperation = "exists"
	indexReadFirstMessage       indexReadOperation = "first-message"
	indexReadBulkPreview        indexReadOperation = "bulk-preview"
	indexReadEntryAnnotations   indexReadOperation = "entry-annotations"
	indexReadEntryAnnotation    indexReadOperation = "entry-annotation"
	indexReadAnnotationInputs   indexReadOperation = "annotation-inputs"
	indexReadEntriesHash        indexReadOperation = "entries-hash"
	indexReadAPIDetail          indexReadOperation = "api-detail"
	indexReadExport             indexReadOperation = "export"
	indexReadSearch             indexReadOperation = "search"
	indexReadMetadata           indexReadOperation = "metadata"
	indexReadSessionAnnotations indexReadOperation = "session-annotations"
)

type indexFormatReadCase struct {
	Name          string               `yaml:"name"`
	Format        *int                 `yaml:"format"`
	Entries       bool                 `yaml:"entries"`
	OtherSession  bool                 `yaml:"otherSession"`
	Refuse        bool                 `yaml:"refuse"`
	Operations    []indexReadOperation `yaml:"operations"`
	PreviewIDs    int                  `yaml:"previewIDs"`
	VariableLimit int32                `yaml:"variableLimit"`
}

func loadIndexFormatReadFixtures(t *testing.T) []indexFormatReadCase {
	t.Helper()
	var document struct {
		RequiredNames      []string              `yaml:"requiredNames"`
		RequiredOperations []string              `yaml:"requiredOperations"`
		Cases              []indexFormatReadCase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(indexFormatReadsYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("index read fixtures require one document: %v", err)
	}
	names, operations := make(map[string]bool), make(map[string]bool)
	for _, row := range document.Cases {
		if row.Name == "" || names[row.Name] || len(row.Operations) == 0 {
			t.Fatalf("invalid index read fixture: %+v", row)
		}
		if row.PreviewIDs < 0 || row.VariableLimit < 0 || (row.VariableLimit > 0 && row.PreviewIDs <= int(row.VariableLimit)) {
			t.Fatalf("bounded preview fixture must exceed its synthetic SQLite variable limit: %+v", row)
		}
		names[row.Name] = true
		for _, operation := range row.Operations {
			switch operation {
			case indexReadEntries, indexReadRange, indexReadMaximum, indexReadExists, indexReadFirstMessage, indexReadBulkPreview, indexReadEntryAnnotations, indexReadEntryAnnotation, indexReadAnnotationInputs, indexReadEntriesHash, indexReadAPIDetail, indexReadExport, indexReadSearch, indexReadMetadata, indexReadSessionAnnotations:
			default:
				t.Fatalf("unknown index read operation %q", operation)
			}
			operations[string(operation)] = true
		}
	}
	if err := testutil.RequireFixtureNames("index format reads", "case", document.RequiredNames, names); err != nil {
		t.Fatal(err)
	}
	if err := testutil.RequireFixtureNames("index format reads", "operation", document.RequiredOperations, operations); err != nil {
		t.Fatal(err)
	}
	return document.Cases
}

func TestIndexFormatReadsRefuseUnknownProjectionWithinScope(t *testing.T) {
	for _, row := range loadIndexFormatReadFixtures(t) {
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			var db *store.Store
			if row.VariableLimit > 0 {
				var err error
				db, err = store.Open(storetest.CopyGoldenDB(t), store.WithSkipMigrations(), store.WithPoolSize(1))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = db.Close() })
			} else {
				db = openTestStore(t)
			}
			sid := schema.SessionID(testutil.TestSessionUUID)
			otherID := schema.SessionID(testutil.TestSessionUUID2)
			seedSession(t, db, string(sid))
			seedSession(t, db, string(otherID))
			if row.Entries {
				if err := db.IndexSessionEntries(t.Context(), sid, batchTestEntries(sid, "hidden result", 1)); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.IndexSessionEntries(t.Context(), otherID, batchTestEntries(otherID, "searchable healthy", 1)); err != nil {
				t.Fatal(err)
			}
			conn := takeConn(t, db.Pool())
			var format any
			if row.Format != nil {
				format = *row.Format
			}
			if err := sqlitex.ExecuteTransient(conn, "UPDATE sessions SET index_format_version = ? WHERE session_id = ?", &sqlitex.ExecOptions{Args: []any{format, string(sid)}}); err != nil {
				t.Fatal(err)
			}
			db.Pool().Put(conn)
			if row.VariableLimit > 0 {
				conn := takeConn(t, db.Pool())
				conn.Limit(sqlite.LimitVariableNumber, row.VariableLimit)
				db.Pool().Put(conn)
			}
			target := sid
			if row.OtherSession {
				target = otherID
			}
			for _, operation := range row.Operations {
				t.Run(string(operation), func(t *testing.T) {
					err := runIndexReadOperation(t, db, target, operation, row.PreviewIDs)
					if row.Refuse {
						var unsupported *store.UnsupportedIndexFormatError
						if !errors.As(err, &unsupported) || unsupported.SessionID != sid {
							t.Fatalf("operation did not propagate typed format refusal for %s: %v", sid, err)
						}
					} else if err != nil {
						t.Fatalf("supported or metadata-only scope was refused: %v", err)
					}
				})
			}
		})
	}
}

func runIndexReadOperation(t *testing.T, db *store.Store, sid schema.SessionID, operation indexReadOperation, previewIDs int) error {
	t.Helper()
	ctx := t.Context()
	switch operation {
	case indexReadEntries:
		_, err := db.ListEntries(ctx, sid)
		return err
	case indexReadRange:
		_, err := db.ListEntriesRange(ctx, sid, 99, 100)
		return err
	case indexReadMaximum:
		_, err := db.MaxEntryIndex(ctx, sid)
		return err
	case indexReadExists:
		_, err := db.SessionEntriesExist(ctx, sid)
		return err
	case indexReadFirstMessage:
		_, err := db.FirstUserMessage(ctx, string(sid))
		return err
	case indexReadBulkPreview:
		ids := make([]string, max(1, previewIDs))
		for i := range ids {
			ids[i] = string(sid)
		}
		_, err := db.FirstUserMessageBulk(ctx, ids)
		return err
	case indexReadEntryAnnotations:
		_, err := db.GetEntryAnnotationsForSession(ctx, string(sid))
		return err
	case indexReadEntryAnnotation:
		_, err := db.GetAnnotationsForEntry(ctx, string(sid), 99)
		return err
	case indexReadAnnotationInputs:
		_, err := db.GetAnnotationRunInputs(ctx, sid)
		return err
	case indexReadEntriesHash:
		_, _, err := db.GetCurrentSessionEntriesHash(ctx, sid)
		return err
	case indexReadAPIDetail:
		_, err := api.NewStoreDataProviderWithFS(db, sessionvisibility.All(), testutil.NewMemFS()).SessionByID(ctx, string(sid))
		return err
	case indexReadExport:
		_, err := export.ExportSession(ctx, db, testutil.NewMemFS(), string(sid))
		return err
	case indexReadSearch:
		_, err := codemap.NewService(db, nil, nil, sessionvisibility.All()).Search(ctx, "searchable", 1)
		return err
	case indexReadMetadata:
		_, err := db.SessionByID(ctx, string(sid))
		return err
	case indexReadSessionAnnotations:
		_, err := db.GetAnnotationsForSession(ctx, string(sid))
		return err
	default:
		t.Fatalf("unknown operation %q", operation)
		return nil
	}
}
