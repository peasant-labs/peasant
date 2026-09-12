package store_test

import (
	"bytes"
	_ "embed"
	"errors"
	"io"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/index_annotation_scope.yaml
var indexAnnotationScopeYAML []byte

type indexAnnotationSelection string

const (
	indexAnnotationHealthySession     indexAnnotationSelection = "healthy-session"
	indexAnnotationUnsupportedSession indexAnnotationSelection = "unsupported-session"
	indexAnnotationHealthyLabel       indexAnnotationSelection = "healthy-label"
	indexAnnotationUnsupportedLabel   indexAnnotationSelection = "unsupported-label"
	indexAnnotationNone               indexAnnotationSelection = "none"
)

type indexAnnotationScopeCase struct {
	Name         string                   `yaml:"name"`
	Select       indexAnnotationSelection `yaml:"select"`
	MetadataOnly bool                     `yaml:"metadataOnly"`
	Refuse       bool                     `yaml:"refuse"`
	WantTotal    int                      `yaml:"wantTotal"`
}

func loadIndexAnnotationScopeFixtures(t *testing.T) []indexAnnotationScopeCase {
	t.Helper()
	var document struct {
		RequiredNames []string                   `yaml:"requiredNames"`
		Cases         []indexAnnotationScopeCase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(indexAnnotationScopeYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("annotation scope fixtures require one document: %v", err)
	}
	names := make(map[string]bool)
	for _, row := range document.Cases {
		if row.Name == "" || names[row.Name] {
			t.Fatalf("invalid annotation scope fixture %+v", row)
		}
		switch row.Select {
		case indexAnnotationHealthySession, indexAnnotationUnsupportedSession, indexAnnotationHealthyLabel, indexAnnotationUnsupportedLabel, indexAnnotationNone:
		default:
			t.Fatalf("unknown selection %q", row.Select)
		}
		names[row.Name] = true
	}
	if err := testutil.RequireFixtureNames("index annotation scope", "case", document.RequiredNames, names); err != nil {
		t.Fatal(err)
	}
	return document.Cases
}

func TestIndexFormatAnnotationPushValidatesOnlySelectedCoordinates(t *testing.T) {
	for _, row := range loadIndexAnnotationScopeFixtures(t) {
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			db := openTestStore(t)
			ctx := t.Context()
			healthy, unsupported := schema.SessionID(testutil.TestSessionUUID), schema.SessionID(testutil.TestSessionUUID2)
			seedSession(t, db, string(healthy))
			seedSession(t, db, string(unsupported))
			if err := db.IndexSessionEntries(ctx, healthy, batchTestEntries(healthy, "healthy", 1)); err != nil {
				t.Fatal(err)
			}
			if err := db.IndexSessionEntries(ctx, unsupported, batchTestEntries(unsupported, "newer", 1)); err != nil {
				t.Fatal(err)
			}
			annotator, err := db.GetAnnotatorIDByName(ctx, "frustration-classifier")
			if err != nil {
				t.Fatal(err)
			}
			entryType, err := db.GetAnnotationTypeID(ctx, "quality.frustration_signal")
			if err != nil {
				t.Fatal(err)
			}
			createEntry := func(sid schema.SessionID) string {
				id, err := db.CreateEntryAnnotation(ctx, ingest.EntryAnnotationParams{SessionID: string(sid), EntryIndex: 0, EndIndex: 1, AnnotatorID: annotator, AnnotationTypeID: entryType, Value: "detected"})
				if err != nil {
					t.Fatal(err)
				}
				return id
			}
			healthyAnnotation := createEntry(healthy)
			var unsupportedAnnotation string
			if row.MetadataOnly {
				typeID, err := db.GetAnnotationTypeID(ctx, "quality.session_outcome")
				if err != nil {
					t.Fatal(err)
				}
				id := string(unsupported)
				unsupportedAnnotation, err = db.CreateAnnotation(ctx, store.CreateAnnotationParams{SessionID: &id, AnnotatorID: annotator, AnnotationTypeID: typeID, Value: "resolved"})
				if err != nil {
					t.Fatal(err)
				}
			} else {
				unsupportedAnnotation = createEntry(unsupported)
			}
			conn := takeConn(t, db.Pool())
			if err := sqlitex.ExecuteTransient(conn, "UPDATE sessions SET index_format_version = 99 WHERE session_id = ?", &sqlitex.ExecOptions{Args: []any{string(unsupported)}}); err != nil {
				t.Fatal(err)
			}
			db.Pool().Put(conn)
			selection := push.AnnotationSelection{}
			switch row.Select {
			case indexAnnotationHealthySession:
				selection.SessionIDs = map[string]bool{string(healthy): true}
			case indexAnnotationUnsupportedSession:
				selection.SessionIDs = map[string]bool{string(unsupported): true}
			case indexAnnotationHealthyLabel:
				selection.IDs = map[string]bool{healthyAnnotation: true}
			case indexAnnotationUnsupportedLabel:
				selection.IDs = map[string]bool{unsupportedAnnotation: true}
			case indexAnnotationNone:
				selection.SessionIDs = map[string]bool{}
			}
			result, err := push.PushAnnotationsSelected(ctx, nil, db, selection, true, 1)
			if row.Refuse {
				var formatError *store.UnsupportedIndexFormatError
				if !errors.As(err, &formatError) || result != nil || formatError.SessionID != unsupported {
					t.Fatalf("selected unsupported coordinates reached emission: result=%+v error=%v", result, err)
				}
			} else if err != nil || result.Total != row.WantTotal || len(result.Unpublishable) != 0 {
				t.Fatalf("safe selected annotation scope was changed: result=%+v error=%v", result, err)
			}
		})
	}
}
