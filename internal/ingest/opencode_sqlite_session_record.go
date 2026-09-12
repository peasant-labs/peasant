package ingest

import (
	"fmt"

	"zombiezen.com/go/sqlite"
)

// decodeOpenCodeSessionRecord is shared by paged discovery and targeted
// materialization attribution. Presence survives a dropped optional row so the
// discovery authority does not mistake undecodable metadata for deletion.
func decodeOpenCodeSessionRecord(stmt *sqlite.Stmt, support openCodeSessionColumnSupport) (OpenCodeSessionRecord, *OpenCodeSessionLinkID, error) {
	if stmt.ColumnType(0) != sqlite.TypeText {
		return OpenCodeSessionRecord{}, nil, fmt.Errorf("a session row was dropped because id has SQLite type %s instead of text", stmt.ColumnType(0))
	}
	rawID := stmt.ColumnText(0)
	sessionID, err := NewOpenCodeSessionLinkID(rawID)
	if err != nil {
		return OpenCodeSessionRecord{}, nil, fmt.Errorf("a session row was dropped because id %q is not a valid identifier: %v", rawID, err)
	}
	record := OpenCodeSessionRecord{SessionID: sessionID}
	if support.hasParent {
		if stmt.ColumnType(1) == sqlite.TypeText && stmt.ColumnText(1) != "" {
			parentID, err := NewOpenCodeSessionLinkID(stmt.ColumnText(1))
			if err != nil {
				return OpenCodeSessionRecord{}, &sessionID, fmt.Errorf("session row %q was dropped because parent_id is not a valid identifier: %v", rawID, err)
			}
			record.ParentID = parentID
		}
	}
	if support.hasClock {
		column := 1
		if support.hasParent {
			column = 2
		}
		switch stmt.ColumnType(column) {
		case sqlite.TypeNull:
		case sqlite.TypeInteger:
			record.TimeUpdated = stmt.ColumnInt64(column)
		default:
			return OpenCodeSessionRecord{}, &sessionID, fmt.Errorf("session row %q was dropped because time_updated has SQLite type %s instead of integer", rawID, stmt.ColumnType(column))
		}
	}
	if support.attribution() {
		if stmt.ColumnType(3) == sqlite.TypeText {
			record.Directory = stmt.ColumnText(3)
		}
		if stmt.ColumnType(4) == sqlite.TypeText {
			record.Title = stmt.ColumnText(4)
		}
		if stmt.ColumnType(5) == sqlite.TypeInteger {
			record.TimeCreated = stmt.ColumnInt64(5)
		}
	}
	if support.extendedAttribution() {
		if stmt.ColumnType(6) == sqlite.TypeText {
			record.Agent = stmt.ColumnText(6)
		}
		if stmt.ColumnType(7) == sqlite.TypeInteger {
			record.TokensInput = stmt.ColumnInt64(7)
		}
		if stmt.ColumnType(8) == sqlite.TypeInteger {
			record.TokensOutput = stmt.ColumnInt64(8)
		}
		if stmt.ColumnType(9) == sqlite.TypeInteger {
			record.TokensReasoning = stmt.ColumnInt64(9)
		}
		if stmt.ColumnType(10) == sqlite.TypeInteger {
			record.TokensCacheRead = stmt.ColumnInt64(10)
		}
		if stmt.ColumnType(11) == sqlite.TypeInteger {
			record.TokensCacheWrite = stmt.ColumnInt64(11)
		}
		switch stmt.ColumnType(12) {
		case sqlite.TypeFloat, sqlite.TypeInteger:
			record.Cost = stmt.ColumnFloat(12)
		}
		if stmt.ColumnType(13) == sqlite.TypeText {
			record.Version = stmt.ColumnText(13)
		}
		if stmt.ColumnType(14) == sqlite.TypeText {
			record.Slug = stmt.ColumnText(14)
		}
		if stmt.ColumnType(15) == sqlite.TypeText {
			record.Revert = stmt.ColumnText(15)
		}
	}
	return record, &sessionID, nil
}
