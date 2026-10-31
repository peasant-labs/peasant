package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// TestListUserPromptsReadsStoredRepresentations reads a harmonized session, and
// a mirror session whose capture claims chunks it does not hold. The second
// must refuse: serving its preview would present bounded text as whole.
func TestListUserPromptsReadsStoredRepresentations(t *testing.T) {
	s := openSnapshotStore(t)
	seedShimSuite(t, s)
	prompts, err := s.ListUserPrompts(context.Background(), shimSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(prompts) != 1 || prompts[0].Text != "hello" || prompts[0].EntryIndex != 0 {
		t.Errorf("prompts = %+v", prompts)
	}
	if _, err := s.ListUserPrompts(context.Background(), mirrorSessionID); err == nil || !strings.Contains(err.Error(), "inconsistent") {
		t.Fatalf("err = %v, want the integrity refusal", err)
	}
}

// TestListUserPromptsValidatesIndexFormat proves the read refuses a session
// whose stored format this build cannot serve, as ListEntriesRange does.
func TestListUserPromptsValidatesIndexFormat(t *testing.T) {
	s := openSnapshotStore(t)
	seedShimSuite(t, s)
	conn, err := s.PoolForTest().Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	err = sqlitex.ExecuteTransient(conn, `UPDATE sessions SET index_format_version = 999 WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{shimSessionID}})
	s.PoolForTest().Put(conn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListUserPrompts(context.Background(), shimSessionID); err == nil || !strings.Contains(err.Error(), "index") {
		t.Fatalf("err = %v, want an index format refusal", err)
	}
}
