package kickstart_test

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/exp/golden"
	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/tui/kickstart"
)

//go:embed testdata/oauth_prompt_render.yaml
var oauthPromptRenderData []byte

const expectedOAuthPromptRenderCaseCount = 4

// oauthPromptPhase names which of the two connect/login prompts a case renders.
type oauthPromptPhase string

const (
	oauthPromptPhaseOAuth      oauthPromptPhase = "oauth"
	oauthPromptPhaseVisibility oauthPromptPhase = "visibility"
)

func (p oauthPromptPhase) valid() bool {
	return p == oauthPromptPhaseOAuth || p == oauthPromptPhaseVisibility
}

// oauthPromptRenderCase is one captured prompt screen plus the structural facts
// its rendered content must (or must not) carry at that terminal width. Real
// wrap behavior - not just a byte-identical golden - is what proves the prompt
// is width-aware rather than a hard-wrapped block: wantSingleLine pins a fact
// that must still fit on one physical line, wantWrappedFact pins a fact that
// must NOT survive as one contiguous line at that width.
type oauthPromptRenderCase struct {
	Name            string           `yaml:"name"`
	Phase           oauthPromptPhase `yaml:"phase"`
	Width           int              `yaml:"width"`
	Height          int              `yaml:"height"`
	WantHeading     string           `yaml:"wantHeading"`
	WantBulletCount int              `yaml:"wantBulletCount"`
	WantSingleLine  string           `yaml:"wantSingleLine"`
	WantWrappedFact string           `yaml:"wantWrappedFact"`
}

type oauthPromptRenderDoc struct {
	ExpectedCaseCount int                     `yaml:"expectedCaseCount"`
	Cases             []oauthPromptRenderCase `yaml:"cases"`
}

func decodeOAuthPromptRenderDoc(data []byte) (oauthPromptRenderDoc, error) {
	var doc oauthPromptRenderDoc
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		return doc, fmt.Errorf("decode testdata/oauth_prompt_render.yaml: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		return doc, fmt.Errorf("oauth_prompt_render.yaml must hold exactly one document: %w", err)
	}
	if doc.ExpectedCaseCount != expectedOAuthPromptRenderCaseCount || len(doc.Cases) != expectedOAuthPromptRenderCaseCount {
		return doc, fmt.Errorf("oauth prompt render cases: declared=%d actual=%d required=%d",
			doc.ExpectedCaseCount, len(doc.Cases), expectedOAuthPromptRenderCaseCount)
	}
	names := map[string]bool{}
	for _, c := range doc.Cases {
		if c.Name == "" || names[c.Name] || !c.Phase.valid() || c.Width <= 0 || c.Height <= 0 ||
			c.WantHeading == "" || c.WantBulletCount <= 0 {
			return doc, fmt.Errorf("oauth prompt render case %q is empty, duplicated, or missing required fields", c.Name)
		}
		if c.WantSingleLine == "" && c.WantWrappedFact == "" {
			return doc, fmt.Errorf("oauth prompt render case %q asserts neither a fitted nor a wrapped line", c.Name)
		}
		names[c.Name] = true
	}
	return doc, nil
}

func loadOAuthPromptRenderDoc(t *testing.T) oauthPromptRenderDoc {
	t.Helper()
	doc, err := decodeOAuthPromptRenderDoc(oauthPromptRenderData)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// buildOAuthPromptCase drives a fresh Program to the case's phase at its size.
// A non-nil Login is required to reach PhaseVisibility at all (updateFlow only
// offers it when p.deps.Login != nil); the login itself is never invoked here.
func buildOAuthPromptCase(t *testing.T, c oauthPromptRenderCase) kickstart.Program {
	t.Helper()
	p, _ := newTestProgram(t, kickstart.ProgramDeps{
		Login: func(context.Context, func(string)) (string, error) { return "fixture-user", nil },
	})
	p.SetSize(c.Width, c.Height)
	switch c.Phase {
	case oauthPromptPhaseOAuth:
		return p
	case oauthPromptPhaseVisibility:
		p = declineOAuth(t, p)
		return enterRetainedVisibility(t, p)
	default:
		t.Fatalf("unknown oauth prompt phase %q", c.Phase)
		return p
	}
}

// bulletLines returns the trimmed lines of a stripped view that render as a
// bullet ("- " prefix), the shape the connect/visibility prompts must use
// instead of an unstructured prose block.
func bulletLines(view string) []string {
	var out []string
	for _, ln := range strings.Split(view, "\n") {
		trimmed := strings.TrimSpace(ln)
		if strings.HasPrefix(trimmed, "- ") {
			out = append(out, trimmed)
		}
	}
	return out
}

// TestOAuthPromptRender_Structure proves the connect-now and visibility-login
// prompts render as a styled heading plus a bulleted fact list that adapts to
// the terminal width: a fact that fits the wrap cap stays one physical line, and
// a fact that does not fit a narrow width is actually broken across lines
// rather than clipped or left as one long unwrapped line. It also golden-pins
// the full rendered screen so a human reviewer can see the structured result.
func TestOAuthPromptRender_Structure(t *testing.T) {
	doc := loadOAuthPromptRenderDoc(t)
	for _, c := range doc.Cases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			p := buildOAuthPromptCase(t, c)
			view := p.View()
			stripped := stripRender(view)

			if !strings.Contains(stripped, c.WantHeading) {
				t.Errorf("%s: view missing heading %q:\n%s", c.Name, c.WantHeading, stripped)
			}
			if got := len(bulletLines(stripped)); got != c.WantBulletCount {
				t.Errorf("%s: got %d bullet lines, want %d:\n%s", c.Name, got, c.WantBulletCount, stripped)
			}
			if c.WantSingleLine != "" {
				found := false
				for _, ln := range bulletLines(stripped) {
					if ln == c.WantSingleLine {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("%s: fact %q did not survive as one physical line:\n%s", c.Name, c.WantSingleLine, stripped)
				}
			}
			if c.WantWrappedFact != "" && strings.Contains(stripped, c.WantWrappedFact) {
				t.Errorf("%s: fact %q was not wrapped at width %d (still one unwrapped line):\n%s",
					c.Name, c.WantWrappedFact, c.Width, stripped)
			}
			for i, ln := range strings.Split(view, "\n") {
				if got := lipgloss.Width(ln); got > c.Width {
					t.Errorf("%s: line %d is %d cells, over width %d: %q", c.Name, i, got, c.Width, stripRender(ln))
				}
			}

			golden.RequireEqual(t, []byte(view))
		})
	}
}

func TestOAuthPromptRenderFixtureRejectsUnknownFields(t *testing.T) {
	mutated := append(append([]byte(nil), oauthPromptRenderData...), []byte("\nunknownField: true\n")...)
	if _, err := decodeOAuthPromptRenderDoc(mutated); err == nil {
		t.Fatal("oauth prompt render fixture accepted an unknown field")
	}
}

func TestOAuthPromptRenderFixtureRejectsTrailingDocuments(t *testing.T) {
	mutated := append(append([]byte(nil), oauthPromptRenderData...), []byte("\n---\n{}\n")...)
	if _, err := decodeOAuthPromptRenderDoc(mutated); err == nil {
		t.Fatal("oauth prompt render fixture accepted a trailing document")
	}
}

func TestOAuthPromptRenderFixturePinsCaseCount(t *testing.T) {
	declared := []byte(fmt.Sprintf("expectedCaseCount: %d", expectedOAuthPromptRenderCaseCount))
	changed := []byte(fmt.Sprintf("expectedCaseCount: %d", expectedOAuthPromptRenderCaseCount+1))
	mutated := bytes.Replace(oauthPromptRenderData, declared, changed, 1)
	if bytes.Equal(mutated, oauthPromptRenderData) {
		t.Fatal("oauth prompt render case-count mutation did not alter the fixture")
	}
	if _, err := decodeOAuthPromptRenderDoc(mutated); err == nil {
		t.Fatal("oauth prompt render fixture accepted a changed case-count declaration")
	}
}

// TestOAuthPromptFacts_NoHardWrapBlock proves the connect and visibility
// prompt copy is a list of short, independent sentences - not a hard-wrapped
// prose block with literal newlines baked into the source string, which is the
// exact defect peasant#138 reports.
func TestOAuthPromptFacts_NoHardWrapBlock(t *testing.T) {
	for _, facts := range [][]string{
		kickstart.VillageContextBulletsForTest(),
		kickstart.VisibilityContextBulletsForTest(),
	} {
		if len(facts) == 0 {
			t.Fatal("prompt facts list is empty")
		}
		for _, fact := range facts {
			if strings.Contains(fact, "\n") {
				t.Errorf("prompt fact %q carries an embedded hard-wrap newline", fact)
			}
		}
	}
}
