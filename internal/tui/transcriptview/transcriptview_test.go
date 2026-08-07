package transcriptview_test

import (
	"bytes"
	_ "embed"
	"fmt"
	"image/color"
	"io"
	"regexp"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/exp/golden"
	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/theme"
	"github.com/peasant-labs/peasant/internal/tui/transcriptview"
)

//go:embed testdata/transcripts.yaml
var transcriptData []byte

// coloredRun is one span of the rendered pane that must carry a NAMED palette
// token's color. Naming the token rather than a hex value keeps the expectation
// readable and keeps it from silently agreeing with whatever the code emits.
type coloredRun struct {
	Text  string `yaml:"text"`
	Token string `yaml:"token"`
}

// transcriptCase is one recorded transcript rendered at one width in one mode.
type transcriptCase struct {
	Name        string                 `yaml:"name"`
	Theme       string                 `yaml:"theme"`
	Width       int                    `yaml:"width"`
	Turns       []testutil.TurnFixture `yaml:"turns"`
	WantLabels  []string               `yaml:"wantLabels"`
	WantVisible []string               `yaml:"wantVisible"`
	WantMissing []string               `yaml:"wantMissing"`
	WantColored []coloredRun           `yaml:"wantColored"`
}

// kindCase is one recorded (role, entry type) pair and the rendering kind it
// must collapse to.
type kindCase struct {
	Name      string `yaml:"name"`
	Role      string `yaml:"role"`
	EntryType string `yaml:"entryType"`
	Want      string `yaml:"want"`
}

// transcriptDoc is the whole fixture plus its row-count and closed-set guards.
type transcriptDoc struct {
	ExpectedCaseCount      int              `yaml:"expectedCaseCount"`
	ExpectedTokenNameCount int              `yaml:"expectedTokenNameCount"`
	ExpectedKindCaseCount  int              `yaml:"expectedKindCaseCount"`
	TokenNames             []string         `yaml:"tokenNames"`
	Kinds                  []kindCase       `yaml:"kinds"`
	Cases                  []transcriptCase `yaml:"cases"`
}

func loadTranscriptDoc(t *testing.T) transcriptDoc {
	t.Helper()
	var doc transcriptDoc
	dec := yaml.NewDecoder(bytes.NewReader(transcriptData))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("decode testdata/transcripts.yaml: %v", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		t.Fatalf("transcripts.yaml must hold exactly one document: %v", err)
	}
	if doc.ExpectedCaseCount != len(doc.Cases) || len(doc.Cases) == 0 {
		t.Fatalf("expectedCaseCount=%d but %d cases present", doc.ExpectedCaseCount, len(doc.Cases))
	}
	if doc.ExpectedTokenNameCount != len(doc.TokenNames) || len(doc.TokenNames) == 0 {
		t.Fatalf("expectedTokenNameCount=%d but %d token names listed", doc.ExpectedTokenNameCount, len(doc.TokenNames))
	}
	if doc.ExpectedKindCaseCount != len(doc.Kinds) || len(doc.Kinds) == 0 {
		t.Fatalf("expectedKindCaseCount=%d but %d kind rows present", doc.ExpectedKindCaseCount, len(doc.Kinds))
	}
	// Every kind the renderer can produce must be some row's expectation, or a
	// kind could be dropped from the classifier without a single row going red.
	covered := map[string]bool{}
	kindNames := map[string]bool{}
	for _, k := range doc.Kinds {
		if k.Name == "" || kindNames[k.Name] {
			t.Fatalf("kind row name %q is missing or duplicated", k.Name)
		}
		kindNames[k.Name] = true
		if !transcriptview.Kind(k.Want).IsValid() {
			t.Fatalf("kind row %q expects %q, which is not one of the renderer's kinds", k.Name, k.Want)
		}
		if !ingest.Role(k.Role).IsValid() {
			t.Fatalf("kind row %q declares role %q, which is not one of the contract's roles", k.Name, k.Role)
		}
		if !ingest.EntryType(k.EntryType).IsValid() {
			t.Fatalf("kind row %q declares entry type %q, which is not one of the contract's entry types", k.Name, k.EntryType)
		}
		covered[k.Want] = true
	}
	for _, kind := range transcriptview.AllKinds {
		if !covered[kind.String()] {
			t.Fatalf("no kind row expects %s; that kind could be dropped without any row failing", kind)
		}
	}
	declared := map[string]bool{}
	for _, name := range doc.TokenNames {
		// Resolving here proves the declared set is real, so a typo in the list
		// cannot make a case's token silently unassertable.
		paletteColorFor(t, name, theme.New(theme.ModeDark))
		declared[name] = true
	}
	names := map[string]bool{}
	for _, c := range doc.Cases {
		if c.Name == "" || names[c.Name] {
			t.Fatalf("transcript case name %q is missing or duplicated", c.Name)
		}
		names[c.Name] = true
		if c.Width <= 0 {
			t.Fatalf("transcript case %q declares width %d; a non-positive width renders nothing", c.Name, c.Width)
		}
		if len(c.WantVisible)+len(c.WantMissing)+len(c.WantColored)+len(c.WantLabels) == 0 {
			t.Fatalf("transcript case %q asserts nothing; an empty want list is a guaranteed pass", c.Name)
		}
		for _, run := range c.WantColored {
			if !declared[run.Token] {
				t.Fatalf("transcript case %q names palette token %q, which is not in tokenNames", c.Name, run.Token)
			}
			if run.Text == "" {
				t.Fatalf("transcript case %q declares a colored run with no text; an empty needle always matches", c.Name)
			}
		}
		themeFor(t, c.Theme)
	}
	return doc
}

// themeFor resolves a fixture's theme name, failing closed on an unknown one.
func themeFor(t *testing.T, name string) theme.Theme {
	t.Helper()
	mode := theme.Mode(name)
	if !mode.IsValid() {
		t.Fatalf("transcript fixture names theme %q, which is neither %q nor %q", name, theme.ModeDark, theme.ModeLight)
	}
	return theme.New(mode)
}

// paletteColorFor resolves a fixture's palette token name against the SAME
// theme.Palette the renderer draws from, failing closed on an unknown name so a
// typo can never turn into an assertion that quietly matches nothing.
func paletteColorFor(t *testing.T, name string, th theme.Theme) color.Color {
	t.Helper()
	p := th.Palette
	var pair theme.ColorPair
	switch name {
	case "ink":
		pair = p.Ink
	case "ink-3":
		pair = p.Ink3
	case "teal":
		pair = p.Teal
	case "mauve":
		pair = p.Mauve
	case "olive":
		pair = p.Olive
	case "danger":
		pair = p.Danger
	default:
		t.Fatalf("transcript fixture names palette token %q, which this test cannot resolve; add it here or fix the fixture", name)
	}
	return th.Color(pair)
}

// coloredRunPattern matches text carried by a style run that sets the given
// foreground color, tolerating the other attributes (bold on a label, italic on
// a comment) that travel with it.
func coloredRunPattern(t *testing.T, run coloredRun, th theme.Theme) *regexp.Regexp {
	t.Helper()
	r, g, b, _ := paletteColorFor(t, run.Token, th).RGBA()
	fg := fmt.Sprintf("38;2;%d;%d;%d", uint8(r>>8), uint8(g>>8), uint8(b>>8))
	return regexp.MustCompile(`\x1b\[[0-9;]*` + regexp.QuoteMeta(fg) + `[0-9;]*m` + regexp.QuoteMeta(run.Text))
}

var ansiPattern = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func visible(s string) string { return ansiPattern.ReplaceAllString(s, "") }

// flatten joins wrapped rows so a phrase split across a wrap point is still
// findable, and collapses the gutter rails out of the way.
func flatten(s string) string {
	out := visible(s)
	out = strings.ReplaceAll(out, "│ ", "")
	out = strings.ReplaceAll(out, "│", "")
	return strings.Join(strings.Fields(out), " ")
}

// render drives the REAL renderer over one fixture case.
func render(t *testing.T, c transcriptCase) string {
	t.Helper()
	turns := testutil.Turns(t, c.Name, c.Turns)
	return transcriptview.New(themeFor(t, c.Theme)).Document(turns).Render(c.Width)
}

// TestRender_PerCase drives the real renderer over each fixture case: the role
// tags it must show, the text it must (and must not) carry, and the palette
// token each named span must be styled with.
func TestRender_PerCase(t *testing.T) {
	t.Parallel()
	doc := loadTranscriptDoc(t)

	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			got := render(t, c)
			flat := flatten(got)

			for _, label := range c.WantLabels {
				if !strings.Contains(flat, label) {
					t.Errorf("rendered transcript must tag a turn %q; got:\n%s", label, visible(got))
				}
			}
			for _, want := range c.WantVisible {
				if !strings.Contains(flat, strings.Join(strings.Fields(want), " ")) {
					t.Errorf("rendered transcript must contain %q; got:\n%s", want, visible(got))
				}
			}
			for _, missing := range c.WantMissing {
				if strings.Contains(flat, strings.Join(strings.Fields(missing), " ")) {
					t.Errorf("rendered transcript must not contain %q; got:\n%s", missing, visible(got))
				}
			}
			th := themeFor(t, c.Theme)
			for _, run := range c.WantColored {
				pattern := coloredRunPattern(t, run, th)
				if !pattern.MatchString(got) {
					t.Errorf("output must carry %q styled with the %s token (pattern %s); got:\n%q",
						run.Text, run.Token, pattern, got)
				}
			}
		})
	}
}

// TestRender_Golden snapshots each case's full rendered transcript, escapes and
// all. It is the regression net the named assertions cannot be: a change in the
// gutter, the block spacing, the label styling, or glamour's own layout shows
// up here even when every named expectation still passes.
func TestRender_Golden(t *testing.T) {
	doc := loadTranscriptDoc(t)
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			golden.RequireEqual(t, []byte(render(t, c)))
		})
	}
}

// TestRender_FitsTheRequestedWidth proves the contract every pane depends on:
// no rendered line is wider than the width asked for, gutters and labels
// included. A pane clips by cells, and a line that overflows would be cut
// mid-escape-sequence.
func TestRender_FitsTheRequestedWidth(t *testing.T) {
	t.Parallel()
	doc := loadTranscriptDoc(t)
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			for _, width := range []int{c.Width, narrowWidth, wideWidth} {
				turns := testutil.Turns(t, c.Name, c.Turns)
				got := transcriptview.New(themeFor(t, c.Theme)).Document(turns).Render(width)
				for i, line := range strings.Split(got, "\n") {
					if w := lipgloss.Width(line); w > width {
						t.Errorf("at width %d, line %d is %d cells wide: %q", width, i, w, visible(line))
					}
				}
			}
		})
	}
}

// narrowWidth and wideWidth bracket the fixture widths, so the width contract is
// checked somewhere the gutter barely fits and somewhere it is comfortable.
const (
	narrowWidth = 12
	wideWidth   = 100
)

// TestRender_SameInputSameBytes proves the property the per-turn cache rests on
// and that a golden snapshot needs: one transcript rendered twice at one width
// is byte-identical, whether or not a cache served the second call.
func TestRender_SameInputSameBytes(t *testing.T) {
	t.Parallel()
	doc := loadTranscriptDoc(t)
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			turns := testutil.Turns(t, c.Name, c.Turns)
			r := transcriptview.New(themeFor(t, c.Theme))
			first := r.Document(turns).Render(c.Width)
			second := r.Document(turns).Render(c.Width)
			if first != second {
				t.Errorf("the same transcript rendered twice differs:\nfirst:\n%q\nsecond:\n%q", first, second)
			}
			// A fresh renderer must agree with a warm one, or a cached pane and
			// a cold pane would show different things.
			cold := transcriptview.New(themeFor(t, c.Theme)).Document(turns).Render(c.Width)
			if cold != first {
				t.Errorf("a cold renderer disagrees with a warm one:\ncold:\n%q\nwarm:\n%q", cold, first)
			}
		})
	}
}

// TestRender_WidthChangesTheLayout proves the width is not baked in at load
// time: the SAME document rendered at two widths lays out differently. This is
// the regression guard for the stale-width bug this design exists to prevent -
// a cache keyed only by turn identity would return the first width's text
// forever.
func TestRender_WidthChangesTheLayout(t *testing.T) {
	t.Parallel()
	doc := loadTranscriptDoc(t)
	c := doc.Cases[0]
	turns := testutil.Turns(t, c.Name, c.Turns)
	r := transcriptview.New(themeFor(t, c.Theme))
	document := r.Document(turns)

	narrow := document.Render(narrowWidth)
	wide := document.Render(wideWidth)
	if narrow == wide {
		t.Fatalf("the transcript rendered identically at %d and %d cells; the width is being ignored", narrowWidth, wideWidth)
	}
	if strings.Count(narrow, "\n") <= strings.Count(wide, "\n") {
		t.Errorf("the narrow render (%d rows) is not taller than the wide one (%d rows)",
			strings.Count(narrow, "\n")+1, strings.Count(wide, "\n")+1)
	}
	// Re-rendering at the first width must return the FIRST width's layout, not
	// whatever was rendered last.
	if again := document.Render(narrowWidth); again != narrow {
		t.Errorf("re-rendering at %d cells returned a different layout than the first time", narrowWidth)
	}
}

// TestRender_EmptyAndDegradedInputs proves the pane never blanks or panics on
// the shapes a real store produces: no turns at all, a turn whose body is only
// whitespace, and a turn whose source overlay never arrived so its body is the
// truncated stored preview.
func TestRender_EmptyAndDegradedInputs(t *testing.T) {
	t.Parallel()
	r := transcriptview.New(theme.New(theme.ModeDark))

	if out := r.Document(nil).Render(wideWidth); out != "" {
		t.Errorf("a transcript with no turns must render nothing; got %q", out)
	}

	blank := []ingest.Turn{{Index: 0, Role: ingest.RoleUser, EntryType: ingest.EntryTypeText, Content: "   \n\n  "}}
	out := r.Document(blank).Render(wideWidth)
	if !strings.Contains(flatten(out), transcriptview.KindUser.Label()) {
		t.Errorf("a turn with an empty body must still say a turn happened; got %q", visible(out))
	}

	// The store bounds what it holds, so a session whose full content could not
	// be recovered arrives with bodies cut mid-word. That must render as the
	// text it is, not as a failure.
	truncated := []ingest.Turn{{
		Index: 0, Role: ingest.RoleAssistant, EntryType: ingest.EntryTypeText,
		Content: "the pipeline writes to a temp directory and then renam",
	}}
	got := flatten(r.Document(truncated).Render(wideWidth))
	if !strings.Contains(got, "then renam") {
		t.Errorf("a truncated stored body must still render; got %q", got)
	}
}

// TestRender_BoundsAreVisible proves each cost bound announces itself rather
// than silently ending the transcript early: a preview that just stops reads as
// a session that stopped.
func TestRender_BoundsAreVisible(t *testing.T) {
	t.Parallel()
	r := transcriptview.New(theme.New(theme.ModeDark))

	const extraTurns = 5
	many := make([]ingest.Turn, 0, transcriptview.MaxRenderedTurns+extraTurns)
	for i := range transcriptview.MaxRenderedTurns + extraTurns {
		many = append(many, ingest.Turn{
			Index: i, Role: ingest.RoleUser, EntryType: ingest.EntryTypeText,
			Content: fmt.Sprintf("turn number %d", i),
		})
	}
	document := r.Document(many)
	if document.TurnCount() != len(many) {
		t.Errorf("TurnCount = %d, want %d - the count must report the whole transcript, not the drawn part", document.TurnCount(), len(many))
	}
	out := flatten(document.Render(wideWidth))
	if !strings.Contains(out, fmt.Sprintf("(%d more turns not shown here)", extraTurns)) {
		t.Errorf("a transcript past the render bound must say how many turns it left out; got:\n%s", out)
	}
	if !strings.Contains(out, fmt.Sprintf("turn number %d", transcriptview.MaxRenderedTurns-1)) {
		t.Errorf("the last drawn turn is missing; got:\n%s", out)
	}
	if strings.Contains(out, fmt.Sprintf("turn number %d", transcriptview.MaxRenderedTurns)) {
		t.Errorf("a turn past the render bound was drawn anyway; got:\n%s", out)
	}

	oversized := []ingest.Turn{{
		Index: 0, Role: ingest.RoleUser, EntryType: ingest.EntryTypeText,
		Content: strings.Repeat("a", transcriptview.MaxProseBytes) + "TAILMARKER",
	}}
	body := flatten(r.Document(oversized).Render(wideWidth))
	if strings.Contains(body, "TAILMARKER") {
		t.Error("a turn past the prose bound was laid out in full")
	}
	if !strings.Contains(body, "message continues") {
		t.Errorf("a bounded turn body must say that it continues; got:\n%s", body)
	}
}

// TestRender_SanitizesRecordedControlCharacters proves a recorded transcript
// cannot repaint the screen or break the pane's row measurements, on every
// path: prose read as markdown, and tool output read as plain text.
func TestRender_SanitizesRecordedControlCharacters(t *testing.T) {
	t.Parallel()
	r := transcriptview.New(theme.New(theme.ModeDark))
	const hostile = "before \x1b[2J\x1b]0;retitled\x07 after\ttabbed\rreturned"
	turns := []ingest.Turn{
		{Index: 0, Role: ingest.RoleUser, EntryType: ingest.EntryTypeText, Content: hostile},
		{Index: 1, Role: ingest.RoleAssistant, EntryType: ingest.EntryTypeToolUse,
			ToolCalls: []ingest.ToolCall{{Name: "Bash", Arguments: hostile, Result: hostile}}},
	}

	got := r.Document(turns).Render(wideWidth)
	for _, bad := range []string{"\x1b[2J", "\x1b]0;", "retitled", "\x07", "\r", "\t"} {
		if strings.Contains(got, bad) {
			t.Errorf("rendered transcript still carries %q; got:\n%q", bad, got)
		}
	}
	// The words around the stripped sequences survive: sanitizing removes what a
	// terminal would ACT on, not the conversation.
	for _, want := range []string{"before", "after", "tabbed", "returned"} {
		if !strings.Contains(flatten(got), want) {
			t.Errorf("rendered transcript dropped %q along with the control characters; got:\n%q", want, visible(got))
		}
	}
}

// TestKindOf_ClassifiesEveryRecordedShape pins the collapse from (role, entry
// type) to a rendering kind, including the ones a naive role-only read would
// flatten: a thinking block and a tool call are both recorded with the
// assistant's role.
func TestKindOf_ClassifiesEveryRecordedShape(t *testing.T) {
	t.Parallel()
	doc := loadTranscriptDoc(t)
	for _, k := range doc.Kinds {
		t.Run(k.Name, func(t *testing.T) {
			t.Parallel()
			turn := ingest.Turn{Role: ingest.Role(k.Role), EntryType: ingest.EntryType(k.EntryType)}
			if got := transcriptview.KindOf(turn); got.String() != k.Want {
				t.Errorf("KindOf(role %s, entry type %s) = %s, want %s", k.Role, k.EntryType, got, k.Want)
			}
		})
	}
}

// TestKind_LabelsAreDistinctLowercaseChrome proves every kind is labelled, that
// no two kinds share a tag, and that the chrome stays lowercase - the tags are
// the only thing telling a reader whose words they are looking at.
func TestKind_LabelsAreDistinctLowercaseChrome(t *testing.T) {
	t.Parallel()
	seen := map[string]transcriptview.Kind{}
	for _, kind := range transcriptview.AllKinds {
		if !kind.IsValid() {
			t.Errorf("%s is in AllKinds but does not validate", kind)
		}
		label := kind.Label()
		if label == "" {
			t.Errorf("kind %s has no label", kind)
		}
		if label != strings.ToLower(label) {
			t.Errorf("kind %s is labelled %q; ui chrome is lowercase", kind, label)
		}
		if other, dup := seen[label]; dup {
			t.Errorf("kinds %s and %s share the label %q", other, kind, label)
		}
		seen[label] = kind
	}
	if transcriptview.Kind("not-a-kind").IsValid() {
		t.Error("an unknown kind validated")
	}
}
