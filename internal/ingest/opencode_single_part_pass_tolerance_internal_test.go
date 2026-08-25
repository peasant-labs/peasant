package ingest

import (
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// insertLegacyPartRow writes one part row into a synthetic legacy database with
// an explicit time_created value, so a test can store a REAL value in the
// INTEGER column and reproduce a source-level decode drop.
func insertLegacyPartRow(t *testing.T, path, id, messageID, sessionID string, timeCreated any, data string) {
	t.Helper()
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatalf("open synthetic legacy database read-write: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := sqlitex.Execute(conn, `INSERT INTO part(id, message_id, session_id, time_created, time_updated, data) VALUES(?1, ?2, ?3, ?4, ?4, ?5)`, &sqlitex.ExecOptions{Args: []any{id, messageID, sessionID, timeCreated, data}}); err != nil {
		t.Fatalf("insert synthetic legacy part %q: %v", id, err)
	}
}

func openLegacyToleranceSource(t *testing.T, path string) OpenCodeSQLiteSource {
	t.Helper()
	sourcePath, err := NewOpenCodeSQLiteSourcePath(path)
	if err != nil {
		t.Fatalf("validate synthetic source path: %v", err)
	}
	source, err := OpenOpenCodeSQLiteSource(t.Context(), sourcePath, DefaultOpenCodeSQLiteSourceOptions())
	if err != nil {
		t.Fatalf("open synthetic source: %v", err)
	}
	t.Cleanup(func() { _ = source.Close(t.Context()) })
	return source
}

// TestOpenCodeLegacyPresentMessageBadPartFailsSession proves that a part row the
// source cannot decode, whose message is present in the same session, fails the
// whole session with the decode error instead of being tolerated as an orphan
// with blank identifiers.
func TestOpenCodeLegacyPresentMessageBadPartFailsSession(t *testing.T) {
	const session = "ses_3cd91f52effeXd3QAJ54jOyzO1"
	const presentHost = "msg_orphan_host"
	materialized := testfixture.MaterializeByName(t, "legacy-orphan-tolerance")
	// A REAL time_created keeps the column non-integer, so the source drops the
	// row. Its message is present, so the drop must fail the session.
	insertLegacyPartRow(t, materialized.Path, "part_present_bad", presentHost, session, 1002.5, `{"id":"part_present_bad","messageID":"msg_orphan_host","type":"text","text":"BAD"}`)

	source := openLegacyToleranceSource(t, materialized.Path)
	sessionID, err := NewOpenCodeLegacySessionID(session)
	if err != nil {
		t.Fatalf("build legacy session id: %v", err)
	}
	pageSize, err := NewOpenCodeLegacyPageSize(64)
	if err != nil {
		t.Fatalf("build page size: %v", err)
	}
	_, _, readErr := readOpenCodeLegacyProjectionWithDiagnostics(t.Context(), source, sessionID, pageSize)
	if readErr == nil {
		t.Fatal("projection read succeeded, want a session failure naming the present message with a decoded-part error")
	}
	if !strings.Contains(readErr.Error(), presentHost) {
		t.Fatalf("session failure = %q, want it to name the present message %q", readErr.Error(), presentHost)
	}
}

// TestOpenCodeLegacySessionPartPassAdvancesPastAllDroppedPage proves that a
// bounded part page whose rows the source all drops still advances the cursor,
// so a true orphan on a later page is not lost when an earlier page holds only
// dropped rows.
func TestOpenCodeLegacySessionPartPassAdvancesPastAllDroppedPage(t *testing.T) {
	const session = "ses_3cd91f52effeXd3QAJ54jOyzO1"
	materialized := testfixture.MaterializeByName(t, "legacy-orphan-tolerance")
	// Two absent-message rows the source drops (REAL time_created), then a
	// usable absent-message orphan on a later page. Identifier order places the
	// dropped rows before the good orphan.
	insertLegacyPartRow(t, materialized.Path, "part_zdrop_1", "msg_absent", session, 1101.5, `{}`)
	insertLegacyPartRow(t, materialized.Path, "part_zdrop_2", "msg_absent", session, 1102.5, `{}`)
	insertLegacyPartRow(t, materialized.Path, "part_zgood_3", "msg_absent", session, 1103, `{"id":"part_zgood_3","messageID":"msg_absent","type":"text","text":"GOOD_ORPHAN"}`)

	source := openLegacyToleranceSource(t, materialized.Path)
	sessionID, err := NewOpenCodeLegacySessionID(session)
	if err != nil {
		t.Fatalf("build legacy session id: %v", err)
	}
	// A page size of one forces the dropped rows onto their own pages, so the
	// good orphan is only reached when a dropped-only page still advances.
	pageSize, err := NewOpenCodeLegacyPageSize(1)
	if err != nil {
		t.Fatalf("build page size: %v", err)
	}
	projection, _, readErr := readOpenCodeLegacyProjectionWithDiagnostics(t.Context(), source, sessionID, pageSize)
	if readErr != nil {
		t.Fatalf("projection read failed on tolerated absent-message rows: %v", readErr)
	}
	foundGoodOrphan := false
	for _, message := range projection.Messages {
		for _, part := range message.Parts {
			if part.ID == "part_zgood_3" {
				foundGoodOrphan = true
			}
		}
	}
	if !foundGoodOrphan {
		t.Fatal("the usable orphan on a later page was lost: a dropped-only page ended pagination instead of advancing its cursor")
	}
}
