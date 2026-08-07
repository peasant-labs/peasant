package kit_test

import (
	"bytes"
	_ "embed"
	"errors"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/tui/kit"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/previewsplit_error_tokens.yaml
var previewErrorTokensData []byte

// previewErrorTokens is the fixture of substrings an actionable in-pane
// ContentSource-error message must contain (what/why/where/fix).
type previewErrorTokens struct {
	ExpectedTokenCount int      `yaml:"expectedTokenCount"`
	Tokens             []string `yaml:"tokens"`
}

func loadPreviewErrorTokens(t *testing.T) previewErrorTokens {
	t.Helper()
	var doc previewErrorTokens
	dec := yaml.NewDecoder(bytes.NewReader(previewErrorTokensData))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("decode previewsplit_error_tokens.yaml: %v", err)
	}
	if doc.ExpectedTokenCount != len(doc.Tokens) || len(doc.Tokens) == 0 {
		t.Fatalf("expectedTokenCount=%d but %d tokens listed", doc.ExpectedTokenCount, len(doc.Tokens))
	}
	return doc
}

// erroringSource is a kit.ContentSource that always fails, so the actionable
// error path can be exercised deterministically.
type erroringSource struct{ err error }

func (s erroringSource) Content(_ string, _ int) (string, error) { return "", s.err }

// TestPreviewSplit_ActionableErrorInPane proves a ContentSource error renders
// an actionable in-pane message answering what/why/where/fix, driven by the
// fixture token list.
func TestPreviewSplit_ActionableErrorInPane(t *testing.T) {
	tokens := loadPreviewErrorTokens(t)
	src := erroringSource{err: errors.New("disk on fire")}
	items := []kit.ListItem{kit.StringItem("alpha"), kit.StringItem("bravo")}
	ps := kit.NewPreviewSplit(darkTheme(), kit.NewListLeftPane(kit.NewList(darkTheme(), items)), src)
	ps.SetSize(60, 8)
	ps.Focus()

	for _, msg := range collectMsgs(ps.Load()) {
		ps, _ = ps.Update(msg)
	}
	if ps.Loading() {
		t.Fatal("split should not be loading after an error result")
	}

	// Strip ansi so token assertions match on visible text, not escapes.
	view := stripANSI(ps.View())
	for _, tok := range tokens.Tokens {
		if !strings.Contains(view, tok) {
			t.Errorf("error pane missing actionable token %q; view:\n%s", tok, view)
		}
	}
}

// TestPreviewSplit_FocusToggle proves focus moves between the list pane and
// the preview viewport via the keymap next/prev-field actions, and that while
// the preview pane is focused, navigation scrolls the viewport rather than
// moving the list highlight.
func TestPreviewSplit_FocusToggle(t *testing.T) {
	// A long content body of DISTINCT lines so scrolling visibly changes the
	// rendered viewport window.
	var sb strings.Builder
	for i := 0; i < 40; i++ {
		sb.WriteString("line-")
		sb.WriteByte(byte('a' + i%26))
		sb.WriteByte('\n')
	}
	src := fixedContentSource{content: sb.String()}
	items := []kit.ListItem{kit.StringItem("alpha"), kit.StringItem("bravo"), kit.StringItem("charlie")}
	ps := kit.NewPreviewSplit(darkTheme(), kit.NewListLeftPane(kit.NewList(darkTheme(), items)), src)
	ps.SetSize(40, 6)
	ps.Focus()
	for _, msg := range collectMsgs(ps.Load()) {
		ps, _ = ps.Update(msg)
	}

	if ps.ActivePane() != kit.PaneLeft {
		t.Fatalf("initial active pane = %s, want left", ps.ActivePane())
	}

	// Tab moves focus to the preview viewport.
	ps, _ = ps.Update(keyPress(t, "tab"))
	if ps.ActivePane() != kit.PaneRight {
		t.Fatalf("active pane after tab = %s, want right", ps.ActivePane())
	}

	// With the preview focused, Down scrolls the viewport and must NOT move
	// the list highlight.
	beforeID, _ := ps.HighlightedID()
	before := ps.View()
	ps, _ = ps.Update(keyPress(t, "down"))
	ps, _ = ps.Update(keyPress(t, "down"))
	afterID, _ := ps.HighlightedID()
	after := ps.View()
	if beforeID != afterID {
		t.Fatalf("list highlight moved (%q -> %q) while preview pane focused", beforeID, afterID)
	}
	if before == after {
		t.Fatal("preview viewport did not scroll on Down while focused")
	}

	// Shift+tab returns focus to the list.
	ps, _ = ps.Update(keyPress(t, "shift+tab"))
	if ps.ActivePane() != kit.PaneLeft {
		t.Fatalf("active pane after shift+tab = %s, want left", ps.ActivePane())
	}

	// Back on the list, Down moves the highlight again.
	ps, _ = ps.Update(keyPress(t, "down"))
	if id, _ := ps.HighlightedID(); id != "bravo" {
		t.Fatalf("highlight after down on list = %q, want bravo", id)
	}
}

// stripANSI removes SGR escape sequences so a test can assert on visible text.
func stripANSI(s string) string {
	var b strings.Builder
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		if runes[i] == 0x1b {
			// Skip until the terminating 'm' of the SGR sequence.
			for i < len(runes) && runes[i] != 'm' {
				i++
			}
			continue
		}
		b.WriteRune(runes[i])
	}
	return b.String()
}
