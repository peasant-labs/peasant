package search

import "testing"

// ---------------------------------------------------------------------------
// FilterState transitions
// ---------------------------------------------------------------------------

func TestFilterState_ZeroValue(t *testing.T) {
	var f FilterState
	if f.Active {
		t.Error("zero-value Active should be false")
	}
	if f.Text != "" {
		t.Error("zero-value Text should be empty")
	}
	if f.HasFilter() {
		t.Error("zero-value HasFilter should be false")
	}
}

func TestFilterState_Enter(t *testing.T) {
	var f FilterState
	f.Enter()
	if !f.Active {
		t.Error("Enter should set Active to true")
	}
}

func TestFilterState_Exit(t *testing.T) {
	f := FilterState{Active: true, Text: "hello"}
	f.Exit()
	if f.Active {
		t.Error("Exit should set Active to false")
	}
	if f.Text != "hello" {
		t.Error("Exit should preserve Text")
	}
}

func TestFilterState_Clear(t *testing.T) {
	f := FilterState{Active: true, Text: "hello"}
	f.Clear()
	if f.Active {
		t.Error("Clear should set Active to false")
	}
	if f.Text != "" {
		t.Error("Clear should reset Text to empty")
	}
}

func TestFilterState_Append(t *testing.T) {
	var f FilterState
	f.Append('a')
	f.Append('b')
	if f.Text != "ab" {
		t.Errorf("Text = %q, want %q", f.Text, "ab")
	}
}

func TestFilterState_Backspace(t *testing.T) {
	f := FilterState{Text: "abc"}
	f.Backspace()
	if f.Text != "ab" {
		t.Errorf("after Backspace, Text = %q, want %q", f.Text, "ab")
	}
}

func TestFilterState_BackspaceEmpty(t *testing.T) {
	var f FilterState
	f.Backspace() // should not panic
	if f.Text != "" {
		t.Error("Backspace on empty should remain empty")
	}
}

func TestFilterState_HasFilter(t *testing.T) {
	f := FilterState{Text: "x"}
	if !f.HasFilter() {
		t.Error("HasFilter should be true when Text is non-empty")
	}
	f.Text = ""
	if f.HasFilter() {
		t.Error("HasFilter should be false when Text is empty")
	}
}

// ---------------------------------------------------------------------------
// FilterState.Matches
// ---------------------------------------------------------------------------

func TestFilterState_Matches_EmptyFilter(t *testing.T) {
	var f FilterState
	if !f.Matches("anything") {
		t.Error("empty filter should match everything")
	}
	if !f.Matches("") {
		t.Error("empty filter should match empty string")
	}
}

func TestFilterState_Matches_CaseInsensitive(t *testing.T) {
	f := FilterState{Text: "Hello"}
	if !f.Matches("say hello world") {
		t.Error("should match case-insensitively")
	}
	if !f.Matches("HELLO THERE") {
		t.Error("should match uppercase target")
	}
	if f.Matches("goodbye") {
		t.Error("should not match unrelated text")
	}
}

func TestFilterState_Matches_Substring(t *testing.T) {
	f := FilterState{Text: "proj"}
	if !f.Matches("my-project-name") {
		t.Error("should match substring")
	}
	if f.Matches("unrelated") {
		t.Error("should not match when substring absent")
	}
}

// ---------------------------------------------------------------------------
// FilterState.NormalizedFilter
// ---------------------------------------------------------------------------

func TestFilterState_NormalizedFilter(t *testing.T) {
	f := FilterState{Text: "Hello"}
	if got := f.NormalizedFilter(); got != "hello" {
		t.Errorf("NormalizedFilter = %q, want %q", got, "hello")
	}
}

// ---------------------------------------------------------------------------
// MatchesAny
// ---------------------------------------------------------------------------

func TestMatchesAny_EmptyFilter(t *testing.T) {
	if !MatchesAny("", "a", "b", "c") {
		t.Error("empty filter should match any fields")
	}
	if !MatchesAny("") {
		t.Error("empty filter with no fields should still return true")
	}
}

func TestMatchesAny_SingleMatch(t *testing.T) {
	if !MatchesAny("alpha", "alpha-session", "beta-project", "gamma") {
		t.Error("should match when first field contains filter")
	}
	if !MatchesAny("beta", "alpha", "beta-project", "gamma") {
		t.Error("should match when second field contains filter")
	}
}

func TestMatchesAny_NoMatch(t *testing.T) {
	if MatchesAny("delta", "alpha", "beta", "gamma") {
		t.Error("should not match when no field contains filter")
	}
}

func TestMatchesAny_CaseInsensitive(t *testing.T) {
	if !MatchesAny("ALPHA", "alpha-session") {
		t.Error("should match case-insensitively")
	}
	if !MatchesAny("alpha", "ALPHA-SESSION") {
		t.Error("should match case-insensitively (reverse)")
	}
}

func TestMatchesAny_NoFields(t *testing.T) {
	if MatchesAny("something") {
		t.Error("non-empty filter with no fields should return false")
	}
}

// ---------------------------------------------------------------------------
// Round-trip: FilterState lifecycle
// ---------------------------------------------------------------------------

func TestFilterState_TypicalLifecycle(t *testing.T) {
	var f FilterState

	// User activates search.
	f.Enter()
	if !f.Active {
		t.Fatal("should be active after Enter")
	}

	// User types "abc".
	f.Append('a')
	f.Append('b')
	f.Append('c')
	if f.Text != "abc" {
		t.Fatalf("Text = %q, want %q", f.Text, "abc")
	}

	// User presses backspace.
	f.Backspace()
	if f.Text != "ab" {
		t.Fatalf("Text = %q after backspace, want %q", f.Text, "ab")
	}

	// User presses Enter to keep filter.
	f.Exit()
	if f.Active {
		t.Error("should be inactive after Exit")
	}
	if f.Text != "ab" {
		t.Error("Exit should preserve filter text")
	}
	if !f.HasFilter() {
		t.Error("HasFilter should be true")
	}

	// User presses Escape to clear.
	f.Clear()
	if f.Active {
		t.Error("should be inactive after Clear")
	}
	if f.HasFilter() {
		t.Error("HasFilter should be false after Clear")
	}
}
