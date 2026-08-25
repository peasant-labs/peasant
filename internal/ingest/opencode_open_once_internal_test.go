package ingest

import (
	"strings"
	"testing"
)

// TestWithOpenCodeSQLiteSourceNilOpenerReturnsActionableError proves that the
// shared open helper fails with an actionable error, not a panic, when the
// adapter has no injected opener, and that it never runs the caller function.
func TestWithOpenCodeSQLiteSourceNilOpenerReturnsActionableError(t *testing.T) {
	adapter := &OpenCodeAdapter{}
	called := false
	err := adapter.withOpenCodeSQLiteSource(t.Context(), "/synthetic/opencode.db", func(OpenCodeSQLiteSource) error {
		called = true
		return nil
	})
	if err == nil {
		t.Fatal("nil opener open helper returned no error; it must fail closed")
	}
	if called {
		t.Fatal("nil opener open helper ran the caller function; it must not open or read")
	}
	if !strings.Contains(err.Error(), "source opener is nil") {
		t.Fatalf("nil opener error = %q, want the actionable nil-opener guard message", err.Error())
	}
}
