package kickstart_test

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
	"github.com/peasant-labs/peasant/internal/tui/settings"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

//go:embed testdata/selection_ux.yaml
var selectionUXData []byte

const expectedSelectionUXCaseCount = 6

// selectionUXCase is one first-run selection-step scenario: which sessions the
// local store already holds, the keys pressed, and what the rendered step and
// its footer must and must not show afterward.
type selectionUXCase struct {
	Name              string   `yaml:"name"`
	Ingested          []string `yaml:"ingested"`
	Keys              []string `yaml:"keys"`
	WantContains      []string `yaml:"wantContains"`
	WantMissing       []string `yaml:"wantMissing"`
	FooterContains    []string `yaml:"footerContains"`
	FooterMissing     []string `yaml:"footerMissing"`
	WantNoUnchecked   bool     `yaml:"wantNoUnchecked"`
	WantSomeUnchecked bool     `yaml:"wantSomeUnchecked"`
}

type selectionUXDoc struct {
	ExpectedCaseCount int                   `yaml:"expectedCaseCount"`
	Listings          []ftue.SessionListing `yaml:"listings"`
	Cases             []selectionUXCase     `yaml:"cases"`
}

func decodeSelectionUX(data []byte) (selectionUXDoc, error) {
	var doc selectionUXDoc
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		return doc, fmt.Errorf("decode testdata/selection_ux.yaml: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		return doc, fmt.Errorf("selection_ux.yaml must hold exactly one document: %w", err)
	}
	if doc.ExpectedCaseCount != expectedSelectionUXCaseCount || len(doc.Cases) != expectedSelectionUXCaseCount {
		return doc, fmt.Errorf("selection ux cases: declared=%d actual=%d required=%d",
			doc.ExpectedCaseCount, len(doc.Cases), expectedSelectionUXCaseCount)
	}
	if len(doc.Listings) == 0 {
		return doc, fmt.Errorf("selection ux fixture needs at least one listing")
	}
	sessionIDs := map[string]bool{}
	for _, listing := range doc.Listings {
		if listing.SessionID == "" || sessionIDs[listing.SessionID] {
			return doc, fmt.Errorf("selection ux listing %q is empty or duplicated", listing.SessionID)
		}
		sessionIDs[listing.SessionID] = true
	}
	names := map[string]bool{}
	for _, c := range doc.Cases {
		if c.Name == "" || names[c.Name] {
			return doc, fmt.Errorf("selection ux case %q is empty or duplicated", c.Name)
		}
		names[c.Name] = true
		assertions := len(c.WantContains) + len(c.WantMissing) + len(c.FooterContains) + len(c.FooterMissing)
		if c.WantNoUnchecked {
			assertions++
		}
		if c.WantSomeUnchecked {
			assertions++
		}
		if assertions == 0 {
			return doc, fmt.Errorf("selection ux case %q asserts nothing", c.Name)
		}
		for _, id := range c.Ingested {
			if !sessionIDs[id] {
				return doc, fmt.Errorf("selection ux case %q ingests unknown session %q", c.Name, id)
			}
		}
	}
	return doc, nil
}

func loadSelectionUXDoc(t *testing.T) selectionUXDoc {
	t.Helper()
	doc, err := decodeSelectionUX(selectionUXData)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// buildFirstRunSelection drives the real kickstart Program the command mounts
// against a config path that does NOT exist yet - a genuine first run, so the
// only sessions marked already-imported are the ones in ingested and nothing is
// tracked from a prior save.
func buildFirstRunSelection(t *testing.T, doc selectionUXDoc, c selectionUXCase) kickstart.Program {
	t.Helper()
	th := theme.New(theme.ModeDark)
	source := kickstart.NewScannerTreeSource(doc.Listings, withFixturePathResolver(), kickstart.WithIngestedSessionIDs(c.Ingested))
	preview := kickstart.NewListingPreview(th, doc.Listings, turnsFromPrompts(nil), kickstart.WithListingPreviewContextSource(source))
	path := filepath.Join(t.TempDir(), "config.yaml")
	draft, err := settings.NewDraft(path, config.BaseConfig())
	if err != nil {
		t.Fatalf("open first-run draft: %v", err)
	}
	p := kickstart.NewProgram(kickstart.ProgramDeps{Theme: th, Draft: draft, Source: source, Preview: preview})
	p.SetSize(120, 28)
	p = declineOAuth(t, p)
	for _, key := range c.Keys {
		p = pressAndDrain(p, selectionUXRune(t, key))
	}
	return p
}

func selectionUXRune(t *testing.T, key string) rune {
	t.Helper()
	switch key {
	case "space":
		return ' '
	default:
		runes := []rune(key)
		if len(runes) != 1 {
			t.Fatalf("selection ux fixture names unsupported key %q", key)
		}
		return runes[0]
	}
}

// footerLine is the rendered hint bar: the last content line inside the frame,
// immediately above the bottom border.
func footerLine(view string) string {
	lines := strings.Split(strings.TrimRight(view, "\n"), "\n")
	if len(lines) < 2 {
		return ""
	}
	return lines[len(lines)-2]
}

func TestSelectionStep_FirstRunUX(t *testing.T) {
	doc := loadSelectionUXDoc(t)
	for _, c := range doc.Cases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			view := stripRender(buildFirstRunSelection(t, doc, c).View())
			for _, want := range c.WantContains {
				if !strings.Contains(view, want) {
					t.Errorf("selection step must show %q:\n%s", want, view)
				}
			}
			for _, missing := range c.WantMissing {
				if strings.Contains(view, missing) {
					t.Errorf("selection step must not show %q:\n%s", missing, view)
				}
			}
			footer := footerLine(view)
			for _, want := range c.FooterContains {
				if !strings.Contains(footer, want) {
					t.Errorf("footer must show %q, got %q", want, footer)
				}
			}
			for _, missing := range c.FooterMissing {
				if strings.Contains(footer, missing) {
					t.Errorf("footer must not show %q, got %q", missing, footer)
				}
			}
			if c.WantNoUnchecked && strings.Contains(view, "[ ]") {
				t.Errorf("expected no empty checkboxes after keys %v:\n%s", c.Keys, view)
			}
			if c.WantSomeUnchecked && !strings.Contains(view, "[ ]") {
				t.Errorf("expected at least one empty checkbox after keys %v:\n%s", c.Keys, view)
			}
		})
	}
}

func TestSelectionUXFixtureRejectsUnknownFields(t *testing.T) {
	mutated := append(append([]byte(nil), selectionUXData...), []byte("\nunknownField: true\n")...)
	if _, err := decodeSelectionUX(mutated); err == nil {
		t.Fatal("selection ux fixture accepted an unknown field")
	}
}

func TestSelectionUXFixtureRejectsTrailingDocuments(t *testing.T) {
	mutated := append(append([]byte(nil), selectionUXData...), []byte("\n---\n{}\n")...)
	if _, err := decodeSelectionUX(mutated); err == nil {
		t.Fatal("selection ux fixture accepted a trailing document")
	}
}

func TestSelectionUXFixturePinsCaseCount(t *testing.T) {
	declared := []byte(fmt.Sprintf("expectedCaseCount: %d", expectedSelectionUXCaseCount))
	changed := []byte(fmt.Sprintf("expectedCaseCount: %d", expectedSelectionUXCaseCount+1))
	mutated := bytes.Replace(selectionUXData, declared, changed, 1)
	if bytes.Equal(mutated, selectionUXData) {
		t.Fatal("selection ux count mutation did not alter the fixture")
	}
	if _, err := decodeSelectionUX(mutated); err == nil {
		t.Fatal("selection ux fixture accepted a mismatched case-count guard")
	}
}
