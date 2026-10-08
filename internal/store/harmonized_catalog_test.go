package store_test

import (
	"testing"

	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// GenerationNativeMetadata.Data is text by contract: the
// session_section_native_metadata.data column is STRICT TEXT, and the driver
// binds []byte as BLOB (refused). This pin fails the build the moment the
// field drifts back to bytes.
var _ string = store.GenerationNativeMetadata{}.Data

// TestHarmonizedNativeMetadataDataBindsStrictText proves the contract field
// binds to the column it will fill: the probe mirrors the
// session_section_native_metadata.data STRICT TEXT shape, the row's Data
// binds through a string parameter, and the stored text reads back
// byte-identical. A BLOB bind against the same column is refused, which is
// why the field is text.
func TestHarmonizedNativeMetadataDataBindsStrictText(t *testing.T) {
	t.Parallel()
	conn, err := sqlite.OpenConn(":memory:", sqlite.OpenReadWrite|sqlite.OpenCreate)
	if err != nil {
		t.Fatalf("open conn: %v", err)
	}
	defer conn.Close()

	if err := sqlitex.ExecuteTransient(conn, `CREATE TABLE native_metadata_probe (data TEXT) STRICT`, nil); err != nil {
		t.Fatalf("create probe: %v", err)
	}

	row := store.GenerationNativeMetadata{Data: `{"kind":"codex-thread","turn":3}`}
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO native_metadata_probe (data) VALUES (?)`, &sqlitex.ExecOptions{
		Args: []any{row.Data},
	}); err != nil {
		t.Fatalf("bind Data string to STRICT TEXT: %v", err)
	}

	var got string
	if err := sqlitex.ExecuteTransient(conn, `SELECT data FROM native_metadata_probe`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error { got = stmt.ColumnText(0); return nil },
	}); err != nil {
		t.Fatalf("read probe: %v", err)
	}
	if got != row.Data {
		t.Errorf("round trip: got %q, want %q", got, row.Data)
	}

	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO native_metadata_probe (data) VALUES (?)`, &sqlitex.ExecOptions{
		Args: []any{[]byte(row.Data)},
	}); err == nil {
		t.Error("expected STRICT TEXT to refuse a BLOB bind, but the insert succeeded")
	}
}
