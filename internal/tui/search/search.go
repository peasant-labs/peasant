// Package search provides shared text-filtering primitives for TUI components.
package search

import "strings"

// FilterState manages incremental text filtering state.
type FilterState struct {
	Active bool   // true when in search/typing mode
	Text   string // current filter text (case-insensitive matching)
}

// Enter activates search mode.
func (f *FilterState) Enter() { f.Active = true }

// Exit deactivates search mode without clearing the filter text.
func (f *FilterState) Exit() { f.Active = false }

// Clear deactivates search mode AND clears the filter text.
func (f *FilterState) Clear() {
	f.Active = false
	f.Text = ""
}

// Append adds a character to the filter text.
func (f *FilterState) Append(ch rune) { f.Text += string(ch) }

// Backspace removes the last character. No-op when text is empty.
func (f *FilterState) Backspace() {
	if len(f.Text) > 0 {
		f.Text = f.Text[:len(f.Text)-1]
	}
}

// HasFilter reports whether a non-empty filter is set.
func (f *FilterState) HasFilter() bool { return f.Text != "" }

// Matches reports whether text contains the filter substring (case-insensitive).
// An empty filter matches everything.
func (f *FilterState) Matches(text string) bool {
	if f.Text == "" {
		return true
	}
	return strings.Contains(strings.ToLower(text), strings.ToLower(f.Text))
}

// NormalizedFilter returns the filter text lowercased for use in bulk matching.
func (f *FilterState) NormalizedFilter() string {
	return strings.ToLower(f.Text)
}

// MatchesAny reports whether the filter matches any of the given fields (case-insensitive).
// An empty filter matches everything.
func MatchesAny(filter string, fields ...string) bool {
	if filter == "" {
		return true
	}
	f := strings.ToLower(filter)
	for _, field := range fields {
		if strings.Contains(strings.ToLower(field), f) {
			return true
		}
	}
	return false
}
