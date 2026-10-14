package store

import (
	"fmt"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// migrationGap is durable accounting, not a generation metadata diagnostic.
// It records a source shape the schema migration could not shred, without
// retaining the value itself. Only deleting the owning session removes it.
type migrationGap struct {
	id, sourceOrdinal, observedLength                         int64
	generationID, sourceTable, sourceColumn, observedJSONType string
	message, remediation                                      string
}

func readMigrationGapsOnConn(conn *sqlite.Conn, sid schema.SessionID) ([]migrationGap, error) {
	var gaps []migrationGap
	err := sqlitex.ExecuteTransient(conn, `SELECT id, generation_id, source_table, source_column, source_ordinal, observed_json_type, observed_length, message, remediation FROM session_migration_gaps WHERE session_id=? ORDER BY id`, &sqlitex.ExecOptions{
		Args: []any{string(sid)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			gaps = append(gaps, migrationGap{
				id: stmt.ColumnInt64(0), generationID: stmt.ColumnText(1),
				sourceTable: stmt.ColumnText(2), sourceColumn: stmt.ColumnText(3),
				sourceOrdinal: stmt.ColumnInt64(4), observedJSONType: stmt.ColumnText(5),
				observedLength: stmt.ColumnInt64(6), message: stmt.ColumnText(7), remediation: stmt.ColumnText(8),
			})
			return nil
		},
	})
	return gaps, err
}

// verifyMigrateCarriedGaps compares the accounting as well as the children:
// zero children cannot conceal a non-array source whose gap was lost. All
// session accounting survives, including that for retired generations.
func verifyMigrateCarriedGaps(conn *sqlite.Conn, oracle *migrateOracle) *migrateShadowMismatch {
	stored, err := readMigrationGapsOnConn(conn, oracle.sessionID)
	if err != nil {
		return &migrateShadowMismatch{Dimension: "carried-gaps", Reason: fmt.Sprintf("read migration gap accounting: %v", err)}
	}
	if len(stored) != len(oracle.gaps) {
		return &migrateShadowMismatch{Dimension: "carried-gaps", Reason: fmt.Sprintf("session holds %d accounted gaps, want %d; missing children require durable accounting and re-harvest from the retained transcript", len(stored), len(oracle.gaps))}
	}
	for i, want := range oracle.gaps {
		if stored[i] != want {
			return &migrateShadowMismatch{Dimension: "carried-gaps", Reason: fmt.Sprintf("accounted gap %d for %s.%s changed; preserve accounting and re-harvest from the retained transcript", want.id, want.sourceTable, want.sourceColumn)}
		}
	}
	return nil
}
