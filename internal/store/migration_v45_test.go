package store_test

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"io"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/migrations/v45_opencode_seq_cursor.yaml
var openCodeSeqCursorFixture []byte

type openCodeSeqCursorFixtureFile struct {
	DeclaredRecords    int                          `yaml:"declared_records"`
	DeclaredRejections int                          `yaml:"declared_rejections"`
	Records            []openCodeSeqCursorRecord    `yaml:"records"`
	Rejections         []openCodeSeqCursorRejection `yaml:"rejections"`
}

type openCodeSeqCursorRecord struct {
	Name      string `yaml:"name"`
	SessionID string `yaml:"session_id"`
	LastSeq   int64  `yaml:"last_seq"`
}

type openCodeSeqCursorRejection struct {
	Name string `yaml:"name"`
	SQL  string `yaml:"sql"`
}

func loadOpenCodeSeqCursorFixture(t *testing.T) openCodeSeqCursorFixtureFile {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(openCodeSeqCursorFixture))
	decoder.KnownFields(true)
	var fixture openCodeSeqCursorFixtureFile
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode OpenCode seq cursor fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("OpenCode seq cursor fixture must contain exactly one YAML document: %v", err)
	}
	if fixture.DeclaredRecords != len(fixture.Records) {
		t.Fatalf("OpenCode seq cursor record guard failed: declared=%d actual=%d", fixture.DeclaredRecords, len(fixture.Records))
	}
	if fixture.DeclaredRejections != len(fixture.Rejections) {
		t.Fatalf("OpenCode seq cursor rejection guard failed: declared=%d actual=%d", fixture.DeclaredRejections, len(fixture.Rejections))
	}
	return fixture
}

// TestMigrationV45OpenCodeSeqCursorRoundTrip verifies that the change cursor
// stores and returns each session's last ingested sequence, that an upsert
// replaces an earlier value, and that the closed constraints refuse a wrong row.
func TestMigrationV45OpenCodeSeqCursorRoundTrip(t *testing.T) {
	fixture := loadOpenCodeSeqCursorFixture(t)
	st := openTestStore(t)
	ctx := context.Background()

	ids := make([]ingest.SessionID, 0, len(fixture.Records))
	for _, record := range fixture.Records {
		if err := st.UpsertOpenCodeSeqCursor(ctx, ingest.SessionID(record.SessionID), record.LastSeq); err != nil {
			t.Fatalf("upsert cursor %q: %v", record.Name, err)
		}
		ids = append(ids, ingest.SessionID(record.SessionID))
	}
	cursors, err := st.BulkLookupOpenCodeSeqCursors(ctx, ids)
	if err != nil {
		t.Fatalf("bulk lookup cursors: %v", err)
	}
	for _, record := range fixture.Records {
		got, ok := cursors[ingest.SessionID(record.SessionID)]
		if !ok || got != record.LastSeq {
			t.Fatalf("cursor %q = %d present=%t, want %d", record.Name, got, ok, record.LastSeq)
		}
	}

	// An upsert replaces the earlier value rather than appending a row.
	replaced := fixture.Records[1]
	if err := st.UpsertOpenCodeSeqCursor(ctx, ingest.SessionID(replaced.SessionID), replaced.LastSeq+64); err != nil {
		t.Fatalf("replace cursor: %v", err)
	}
	cursors, err = st.BulkLookupOpenCodeSeqCursors(ctx, []ingest.SessionID{ingest.SessionID(replaced.SessionID)})
	if err != nil {
		t.Fatalf("re-read replaced cursor: %v", err)
	}
	if cursors[ingest.SessionID(replaced.SessionID)] != replaced.LastSeq+64 {
		t.Fatalf("replaced cursor = %d, want %d", cursors[ingest.SessionID(replaced.SessionID)], replaced.LastSeq+64)
	}

	// A session with no stored cursor is omitted, so its first sighting is new.
	missing, err := st.BulkLookupOpenCodeSeqCursors(ctx, []ingest.SessionID{ingest.SessionID("ses_never_stored_000000000")})
	if err != nil {
		t.Fatalf("lookup missing cursor: %v", err)
	}
	if _, present := missing[ingest.SessionID("ses_never_stored_000000000")]; present {
		t.Fatal("a session with no stored cursor must be omitted")
	}

	// The closed constraints refuse a wrong row.
	pool := st.PoolForTest()
	conn := takeConn(t, pool)
	defer pool.Put(conn)
	for _, rejection := range fixture.Rejections {
		if err := sqlitex.ExecuteTransient(conn, rejection.SQL, nil); err == nil {
			t.Fatalf("rejection %q: expected the constraint to refuse the row, but it was accepted", rejection.Name)
		}
	}
}
