package store_test

import (
	"bytes"
	_ "embed"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/index_formats.yaml
var indexFormatWriterYAML []byte

type indexFormatPayload string

const (
	indexFormatPayloadV1     indexFormatPayload = ""
	indexFormatPayloadFake   indexFormatPayload = "fake-v1"
	indexFormatPayloadAbsent indexFormatPayload = "absent"
	indexFormatPayloadNil    indexFormatPayload = "nil-v1"
)

type indexFormatWriterCase struct {
	Name            string             `yaml:"name"`
	StoredProducer  int                `yaml:"storedProducer"`
	StoredFormat    int                `yaml:"storedFormat"`
	MissingFormat   bool               `yaml:"missingFormat"`
	Producer        int                `yaml:"producer"`
	DeclaredFormat  int                `yaml:"declaredFormat"`
	Direct          bool               `yaml:"direct"`
	Empty           bool               `yaml:"empty"`
	FailFormatStamp bool               `yaml:"failFormatStamp"`
	Payload         indexFormatPayload `yaml:"payload"`
	WantError       string             `yaml:"wantError"`
}

func loadIndexFormatWriterFixtures(t *testing.T) []indexFormatWriterCase {
	t.Helper()
	var document struct {
		RequiredNames []string                `yaml:"requiredNames"`
		Cases         []indexFormatWriterCase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(indexFormatWriterYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("index writer fixtures require one document: %v", err)
	}
	names := make(map[string]bool)
	for _, row := range document.Cases {
		if row.Name == "" || names[row.Name] {
			t.Fatalf("invalid or duplicate index writer case %q", row.Name)
		}
		if row.Payload != indexFormatPayloadV1 && row.Payload != indexFormatPayloadFake && row.Payload != indexFormatPayloadAbsent && row.Payload != indexFormatPayloadNil {
			t.Fatalf("unknown payload in case %q: %q", row.Name, row.Payload)
		}
		names[row.Name] = true
	}
	if err := testutil.RequireFixtureNames("index formats", "writer", document.RequiredNames, names); err != nil {
		t.Fatal(err)
	}
	return document.Cases
}

type fakeV1Result struct{}

func (fakeV1Result) IndexVersion() int { return 1 }

var _ indexformat.Result = fakeV1Result{}

type indexStateSnapshot struct {
	Producer int
	Format   *int
	At       *int64
	Hash     *string
	Entries  []schema.SessionEntry
}

func readIndexSnapshot(t *testing.T, db *store.Store, sid schema.SessionID) indexStateSnapshot {
	t.Helper()
	var snapshot indexStateSnapshot
	conn := takeConn(t, db.Pool())
	if err := sqlitex.ExecuteTransient(conn, `SELECT index_version, index_format_version, indexed_at, session_entries_hash FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sid)}, ResultFunc: func(stmt *sqlite.Stmt) error {
			snapshot.Producer = stmt.ColumnInt(0)
			if stmt.ColumnType(1) != sqlite.TypeNull {
				snapshot.Format = intPtr(stmt.ColumnInt(1))
			}
			if stmt.ColumnType(2) != sqlite.TypeNull {
				snapshot.At = int64Ptr(stmt.ColumnInt64(2))
			}
			if stmt.ColumnType(3) != sqlite.TypeNull {
				snapshot.Hash = strPtr(stmt.ColumnText(3))
			}
			return nil
		},
	}); err != nil {
		db.Pool().Put(conn)
		t.Fatal(err)
	}
	db.Pool().Put(conn)
	// The public entry reader verifies the projection stored by the real writer.
	var err error
	snapshot.Entries, err = db.ListEntries(t.Context(), sid)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestIndexFormatWriterPreservesActualProvenance(t *testing.T) {
	for _, row := range loadIndexFormatWriterFixtures(t) {
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			db := openTestStore(t)
			sid := schema.SessionID(testutil.TestSessionUUID)
			seedSession(t, db, string(sid))
			if err := db.IndexSessionEntries(t.Context(), sid, batchTestEntries(sid, "last-good", 1)); err != nil {
				t.Fatal(err)
			}
			storedFormat := row.StoredFormat
			if storedFormat == 0 {
				storedFormat = 1
			}
			conn := takeConn(t, db.Pool())
			var formatArg any = storedFormat
			if row.MissingFormat {
				formatArg = nil
			}
			if err := sqlitex.ExecuteTransient(conn, `UPDATE sessions SET index_version = ?, index_format_version = ?, indexed_at = ? WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{row.StoredProducer, formatArg, int64(1700000000100), string(sid)}}); err != nil {
				t.Fatal(err)
			}
			if row.FailFormatStamp {
				if err := sqlitex.ExecuteScript(conn, `CREATE TRIGGER reject_format_stamp BEFORE UPDATE OF index_format_version ON sessions BEGIN SELECT RAISE(ABORT, 'synthetic format stamp failure'); END;`, nil); err != nil {
					t.Fatal(err)
				}
			}
			db.Pool().Put(conn)
			before := readIndexSnapshot(t, db, sid)
			entries := batchTestEntries(sid, "replacement", 2)
			if row.Empty {
				entries = nil
			}
			var result indexformat.Result = indexformat.V1{Entries: entries}
			switch row.Payload {
			case indexFormatPayloadFake:
				result = fakeV1Result{}
			case indexFormatPayloadAbsent:
				result = nil
			case indexFormatPayloadNil:
				result = (*indexformat.V1)(nil)
			}
			var err error
			if row.Direct {
				err = db.IndexSessionEntries(t.Context(), sid, entries)
			} else {
				writes := db.IndexSessionEntryBatch(t.Context(), []ingest.SessionEntryWrite{{SessionID: sid, Result: result, IndexVersion: row.DeclaredFormat, IndexerVersion: row.Producer, IndexedAtMs: 1700000000200}})
				err = writes[0].Err
				if writes[0].Written != (row.WantError == "") {
					t.Fatalf("write completion=%v, wantError=%q", writes[0].Written, row.WantError)
				}
			}
			after := readIndexSnapshot(t, db, sid)
			if row.WantError != "" {
				if err == nil || !strings.Contains(err.Error(), row.WantError) {
					t.Fatalf("error=%v, want %q", err, row.WantError)
				}
				if !reflect.DeepEqual(before, after) {
					t.Fatalf("refused write changed last-good data: before=%+v after=%+v", before, after)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if after.Format == nil || *after.Format != 1 || after.Hash == nil || len(*after.Hash) != 64 || len(after.Entries) != len(entries) {
				t.Fatalf("successful write lacks actual V1 data/evidence: %+v", after)
			}
			if row.Direct {
				if after.Producer != before.Producer || !reflect.DeepEqual(after.At, before.At) {
					t.Fatalf("entry-only write invented a parser run: before=%+v after=%+v", before, after)
				}
			} else if after.Producer != row.Producer || after.At == nil || *after.At != 1700000000200 {
				t.Fatalf("successful parse lacks actual producer/time: %+v", after)
			}
		})
	}
}
