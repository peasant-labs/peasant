package ingest

import (
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
)

// TestFreshnessBySessionOmitsSessionsWithNoRows proves that the per-session
// freshness aggregate leaves a session with no rows out of the returned map, so
// a caller can tell an empty projection from a real timestamp and fall back to
// the mtime floor. A populated session carries a real, non-zero time.
func TestFreshnessBySessionOmitsSessionsWithNoRows(t *testing.T) {
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

		freshness, err := source.CurrentFreshnessBySession(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if _, present := freshness["ses_absent000000000000000000"]; present {
			t.Fatal("current freshness map contains a session with no rows, want it omitted so the caller falls back to the floor")
		}
		present, ok := freshness["ses_3cd91f52effeXd3QAJ54jOyzv5"]
		if !ok || present.IsZero() {
			t.Fatalf("populated current session freshness = %v ok=%t, want a real timestamp", present, ok)
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

		freshness, err := source.LegacyFreshnessBySession(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if _, present := freshness["ses_absent000000000000000000"]; present {
			t.Fatal("legacy freshness map contains a session with no rows, want it omitted so the caller falls back to the floor")
		}
	})
}
