package ingest

import (
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
)

// TestSelectedFreshnessReturnsZeroForEmptyProjection proves that a session
// with no rows reports the zero time, not the Unix epoch, so a caller can tell
// an empty projection from a real 1970 timestamp and fall back to the mtime
// floor.
func TestSelectedFreshnessReturnsZeroForEmptyProjection(t *testing.T) {
	t.Run("current", func(t *testing.T) {
		materialized := testfixture.MaterializeByName(t, "current-session-message")
		path, err := NewOpenCodeSQLiteSourcePath(materialized.Path)
		if err != nil {
			t.Fatal(err)
		}
		source, err := OpenOpenCodeSQLiteSource(t.Context(), path, DefaultOpenCodeSQLiteSourceOptions())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = source.Close(t.Context()) }()

		emptyID, err := NewOpenCodeCurrentSessionID("ses_absent000000000000000000")
		if err != nil {
			t.Fatal(err)
		}
		empty, err := source.CurrentSessionFreshness(t.Context(), emptyID)
		if err != nil {
			t.Fatal(err)
		}
		if !empty.IsZero() {
			t.Fatalf("empty current projection freshness=%v, want the zero time", empty)
		}

		presentID, err := NewOpenCodeCurrentSessionID("ses_3cd91f52effeXd3QAJ54jOyzv5")
		if err != nil {
			t.Fatal(err)
		}
		present, err := source.CurrentSessionFreshness(t.Context(), presentID)
		if err != nil {
			t.Fatal(err)
		}
		if present.IsZero() {
			t.Fatal("populated current projection freshness is the zero time, want a real timestamp")
		}
	})

	t.Run("legacy", func(t *testing.T) {
		materialized := testfixture.MaterializeByName(t, "legacy-message-part")
		path, err := NewOpenCodeSQLiteSourcePath(materialized.Path)
		if err != nil {
			t.Fatal(err)
		}
		source, err := OpenOpenCodeSQLiteSource(t.Context(), path, DefaultOpenCodeSQLiteSourceOptions())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = source.Close(t.Context()) }()

		emptyID, err := NewOpenCodeLegacySessionID("ses_absent000000000000000000")
		if err != nil {
			t.Fatal(err)
		}
		empty, err := source.LegacySessionFreshness(t.Context(), emptyID)
		if err != nil {
			t.Fatal(err)
		}
		if !empty.IsZero() {
			t.Fatalf("empty legacy projection freshness=%v, want the zero time", empty)
		}
	})
}
