package ingest

import (
	"errors"
	"testing"
)

// TestOpenCodeControlRowIgnoredClassifiesOnlyTheIgnoredSentinel pins the
// distinction the row profiler relies on: an entryless marker this build
// interprets is not a refusal, while a skipped newer control row is an omission
// and stays one.
func TestOpenCodeControlRowIgnoredClassifiesOnlyTheIgnoredSentinel(t *testing.T) {
	if !OpenCodeControlRowIgnored(errOpenCodeIgnoredControlRow) {
		t.Fatal("the ignored-control sentinel was not classified as ignored")
	}
	if OpenCodeControlRowIgnored(errOpenCodeSkipControlRow) {
		t.Fatal("a skipped newer control row was classified as ignored; its omission must stay visible")
	}
	if OpenCodeControlRowIgnored(nil) {
		t.Fatal("nil was classified as an ignored control row")
	}
	if OpenCodeControlRowIgnored(errors.New("other")) {
		t.Fatal("an unrelated error was classified as an ignored control row")
	}
}
