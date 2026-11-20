package store

import (
	"context"
	"fmt"
	"os"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// RefreshSessionEvidence is the stored evidence available before migrations.
// A nil stamp is unproven, including when its column has not been added yet.
type RefreshSessionEvidence struct {
	SessionID        string
	Harness          string
	MetadataVersion  int
	AdapterVersion   *int
	IndexerVersion   int
	IndexedInputHash *string
}

// ReadRefreshEvidence reads session stamps without upgrading or writing.
// It uses the version being read, not the schema of the running build.
func ReadRefreshEvidence(ctx context.Context, path string) ([]RefreshSessionEvidence, int, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if !isStoreFilePath(path) {
		return nil, CurrentSchemaVersion(), nil
	}
	info, err := os.Stat(path)
	if os.IsNotExist(err) || (err == nil && info.Size() < 100) {
		return nil, CurrentSchemaVersion(), nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("store: inspect %s for refresh evidence: %w; nothing was changed", path, err)
	}
	conn, err := openReadOnlyNoSidecars(path)
	if err != nil {
		return nil, 0, fmt.Errorf("store: open %s for refresh evidence: %w; nothing was changed; check read permissions", path, err)
	}
	defer conn.Close()
	v, err := readPragmaInt(conn, "PRAGMA user_version")
	if err != nil {
		return nil, 0, fmt.Errorf("store: read schema version in %s: %w", path, err)
	}
	version := int(v)
	if version > CurrentSchemaVersion() {
		return nil, 0, newerSchemaError(path, version)
	}
	if version == 0 {
		return nil, version, nil
	}
	// The named migration entries are the source of these availability
	// boundaries: v10 adds index tracking, v55 the adapter stamp, and v56
	// the index input proof.
	index, adapter, proof := "0", "NULL", "NULL"
	if version >= indexTrackingVersion {
		index = "index_version"
	}
	if version >= adapterStampVersion {
		adapter = "adapter_version"
	}
	if version >= indexInputProofVersion {
		proof = "indexed_input_hash"
	}
	query := "SELECT session_id, model_harness, schema_version, " + adapter + ", " + index + ", " + proof + " FROM sessions ORDER BY session_id"
	var rows []RefreshSessionEvidence
	err = sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		row := RefreshSessionEvidence{SessionID: stmt.ColumnText(0), Harness: stmt.ColumnText(1), MetadataVersion: stmt.ColumnInt(2), IndexerVersion: stmt.ColumnInt(4)}
		if stmt.ColumnType(3) != sqlite.TypeNull {
			value := stmt.ColumnInt(3)
			row.AdapterVersion = &value
		}
		if stmt.ColumnType(5) != sqlite.TypeNull {
			value := stmt.ColumnText(5)
			row.IndexedInputHash = &value
		}
		rows = append(rows, row)
		return nil
	}})
	if err != nil {
		return nil, 0, fmt.Errorf("store: read refresh evidence from %s at schema %d: %w; nothing was changed; check the store and retry", path, version, err)
	}
	return rows, version, nil
}
