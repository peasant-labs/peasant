package kickstart_test

import (
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

// recordingReader is a deterministic kit.BodySource for the preview smoke: it
// yields a body that renders "body:<id>" and records every session id it was
// asked for, mutex-guarded so the race detector is satisfied.
//
// It is deliberately NOT the real transcript preview. What these tests pin is
// the PANE's behaviour - spinner, stale-result guard, focus - which needs a
// body whose text is predictable, not a rendered transcript.
type recordingReader struct {
	mu    sync.Mutex
	calls []string
}

// Body implements kit.BodySource.
func (r *recordingReader) Body(id string) (kit.PreviewBody, error) {
	r.mu.Lock()
	r.calls = append(r.calls, id)
	r.mu.Unlock()
	return literalBody("body:" + id), nil
}

var _ kit.BodySource = (*recordingReader)(nil)

// literalBody is a preview whose text does not depend on the width it is drawn
// at, so a pane assertion reads exactly what the source produced.
type literalBody string

// Render implements kit.PreviewBody.
func (b literalBody) Render(int) string { return string(b) }

// collectMsgs runs a command, unwrapping one level of tea.BatchMsg, and returns
// every concrete message it produced (dropping nils and the batch envelope).
func collectMsgs(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	var out []tea.Msg
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			out = append(out, collectMsgs(c)...)
		}
		return out
	}
	if msg != nil {
		out = append(out, msg)
	}
	return out
}

func sampleSessions() []ftue.SessionListing {
	return []ftue.SessionListing{
		{Harness: "claude-code", SessionID: "alpha", Title: "alpha session"},
		{Harness: "claude-code", SessionID: "bravo", Title: "bravo session"},
		{Harness: "claude-code", SessionID: "charlie", Title: "charlie session"},
	}
}

// TestSessionPreview_SpinnerThenContent proves the mounted preview shows the
// spinner while a session body is loading and the loaded content once the async
// read completes.
func TestSessionPreview_SpinnerThenContent(t *testing.T) {
	t.Parallel()
	th := theme.New(theme.ModeDark)
	split, initCmd := kickstart.NewSessionPreview(th, sampleSessions(), &recordingReader{})
	split.SetSize(40, 8)

	if !split.Loading() {
		t.Fatal("preview should be loading immediately after mount")
	}
	if got := split.View(); !strings.Contains(got, "loading preview") {
		t.Fatalf("preview pane should show the spinner label while loading; view:\n%s", got)
	}
	for _, msg := range collectMsgs(initCmd) {
		split, _ = split.Update(msg)
	}
	if split.Loading() {
		t.Fatal("preview should not be loading after the result is applied")
	}
	if got := split.View(); !strings.Contains(got, "body:alpha") {
		t.Fatalf("preview pane should show the first session body; view:\n%s", got)
	}
}

// TestSessionPreview_StaleResultDropped proves the stale guard: a late result for
// a since-de-highlighted session never overwrites the current preview.
func TestSessionPreview_StaleResultDropped(t *testing.T) {
	t.Parallel()
	th := theme.New(theme.ModeDark)
	split, initCmd := kickstart.NewSessionPreview(th, sampleSessions(), &recordingReader{})
	split.SetSize(40, 8)

	// Hold (do not apply) the first (alpha) load; it is now in flight.
	staleMsgs := collectMsgs(initCmd)

	// Move the highlight to bravo, which starts a NEW load (superseding alpha).
	var downCmd tea.Cmd
	split, downCmd = split.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if id, _ := split.HighlightedID(); id != "bravo" {
		t.Fatalf("highlight = %q, want bravo", id)
	}
	for _, msg := range collectMsgs(downCmd) {
		split, _ = split.Update(msg)
	}
	if got := split.View(); !strings.Contains(got, "body:bravo") {
		t.Fatalf("preview should show bravo after the highlight moved; view:\n%s", got)
	}

	// Now deliver the STALE alpha result: it must be dropped, not shown.
	for _, msg := range staleMsgs {
		split, _ = split.Update(msg)
	}
	if got := split.View(); strings.Contains(got, "body:alpha") {
		t.Fatalf("stale alpha result overwrote the current bravo preview; view:\n%s", got)
	}
	if got := split.View(); !strings.Contains(got, "body:bravo") {
		t.Fatalf("current preview should still be bravo; view:\n%s", got)
	}
}
