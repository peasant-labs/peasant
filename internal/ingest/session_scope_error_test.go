package ingest_test

import (
	_ "embed"
	"fmt"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/session_scope_errors/cases.yaml
var sessionScopeErrorsYAML []byte

//go:embed testdata/session_scope_errors/manifest.yaml
var sessionScopeErrorsManifestYAML []byte

// sessionScopeErrorPane is one run's rendering: the store the run had (which
// the prefix does not change) makes the pane pair the only difference between
// the bare and prefixed expectations.
type sessionScopeErrorPane struct {
	Reason string `yaml:"reason"`
	Fix    string `yaml:"fix"`
	Block  string `yaml:"block"`
}

type sessionScopeErrorSearch struct {
	Harness  string `yaml:"harness"`
	Method   string `yaml:"method"`
	Location string `yaml:"location"`
	Reason   string `yaml:"reason,omitempty"`
	Detail   string `yaml:"detail,omitempty"`
}

type sessionScopeErrorMiss struct {
	ID            string                    `yaml:"id"`
	Reason        string                    `yaml:"reason"`
	StoredHarness string                    `yaml:"storedHarness,omitempty"`
	Searched      []sessionScopeErrorSearch `yaml:"searched,omitempty"`
}

type sessionScopeErrorCase struct {
	Name     string                  `yaml:"name"`
	Command  string                  `yaml:"command"`
	Store    string                  `yaml:"store"`
	Misses   []sessionScopeErrorMiss `yaml:"misses"`
	Bare     sessionScopeErrorPane   `yaml:"bare"`
	Prefixed sessionScopeErrorPane   `yaml:"prefixed"`
}

type sessionScopeErrorsFixture struct {
	Prefix string                  `yaml:"prefix"`
	Cases  []sessionScopeErrorCase `yaml:"cases"`
}

func loadSessionScopeErrorsFixture(t *testing.T) sessionScopeErrorsFixture {
	t.Helper()
	var fixture sessionScopeErrorsFixture
	decoder := yaml.NewDecoder(strings.NewReader(string(sessionScopeErrorsYAML)))
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode session scope error fixture: %v", err)
	}
	if fixture.Prefix == "" {
		t.Fatal("session scope error fixture declares no non-default prefix, so the prefixed pane is untested")
	}
	names := make([]string, 0, len(fixture.Cases))
	for _, testCase := range fixture.Cases {
		names = append(names, testCase.Name)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(sessionScopeErrorsManifestYAML, "session scope errors")
	if err != nil {
		t.Fatalf("decode session scope error manifest: %v", err)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "session scope errors"); err != nil {
		t.Fatalf("validate session scope error manifest: %v", err)
	}
	return fixture
}

// TestSessionScopeErrorGoldens pins the three renderers for every way a
// selected id can be unusable, once per run prefix. The label, the reason, the
// searched clause and the fix each appear exactly once in the block, and every
// suggested command carries the run's prefix.
func TestSessionScopeErrorGoldens(t *testing.T) {
	t.Parallel()
	fixture := loadSessionScopeErrorsFixture(t)
	for _, testCase := range fixture.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			scopeErr, err := buildSessionScopeError(testCase)
			if err != nil {
				t.Fatal(err)
			}
			panes := []struct {
				name   string
				prefix string
				want   sessionScopeErrorPane
			}{
				{name: "bare", prefix: "", want: testCase.Bare},
				{name: "prefixed", prefix: fixture.Prefix, want: testCase.Prefixed},
			}
			for _, pane := range panes {
				t.Run(pane.name, func(t *testing.T) {
					scopeErr.Prefix = pane.prefix
					assertSessionScopeErrorPane(t, scopeErr, pane.prefix, pane.want)
				})
			}
		})
	}
}

func buildSessionScopeError(testCase sessionScopeErrorCase) (*ingest.SessionScopeError, error) {
	store, err := sessionScopeStoreAttachment(testCase.Store)
	if err != nil {
		return nil, err
	}
	scopeErr := &ingest.SessionScopeError{Command: testCase.Command, Store: store}
	for _, fixtureMiss := range testCase.Misses {
		reason, err := sessionScopeMissReason(fixtureMiss.Reason)
		if err != nil {
			return nil, err
		}
		miss := ingest.ScopeMiss{
			ID:            ingest.SessionID(fixtureMiss.ID),
			Reason:        reason,
			StoredHarness: ingest.Harness(fixtureMiss.StoredHarness),
		}
		for _, fixtureSearch := range fixtureMiss.Searched {
			method, err := sessionScopeSearchMethod(fixtureSearch.Method)
			if err != nil {
				return nil, err
			}
			searched := ingest.SearchedSource{
				Harness:  ingest.Harness(fixtureSearch.Harness),
				Method:   method,
				Location: fixtureSearch.Location,
				Detail:   fixtureSearch.Detail,
			}
			if fixtureSearch.Reason != "" {
				reason, err := sessionScopeUnsupportedReason(fixtureSearch.Reason)
				if err != nil {
					return nil, err
				}
				searched.Reason = reason
			}
			miss.Searched = append(miss.Searched, searched)
		}
		scopeErr.Misses = append(scopeErr.Misses, miss)
	}
	return scopeErr, nil
}

// assertSessionScopeErrorPane asserts the golden text and the structural
// contract it must keep regardless of the words: one label, one reason line,
// one fix, and the prefix only inside suggested commands.
func assertSessionScopeErrorPane(t *testing.T, scopeErr *ingest.SessionScopeError, prefix string, want sessionScopeErrorPane) {
	t.Helper()

	reason := scopeErr.Reason()
	if reason != want.Reason {
		t.Fatalf("Reason() mismatch\n--- got ---\n%s\n--- want ---\n%s", reason, want.Reason)
	}
	if strings.Contains(reason, "\n") {
		t.Fatalf("Reason() is not one line:\n%s", reason)
	}
	if strings.Contains(reason, "peasant:") || strings.Contains(reason, "fix:") {
		t.Fatalf("Reason() carries a label:\n%s", reason)
	}
	if strings.HasSuffix(reason, ".") {
		t.Fatalf("Reason() ends with a period:\n%s", reason)
	}

	fix := scopeErr.FixLine()
	if fix != want.Fix {
		t.Fatalf("FixLine() mismatch\n--- got ---\n%s\n--- want ---\n%s", fix, want.Fix)
	}
	if strings.Contains(fix, "\n") || strings.Contains(fix, "fix:") {
		t.Fatalf("FixLine() carries a label or a second line:\n%s", fix)
	}

	block := scopeErr.Error()
	if block != want.Block {
		t.Fatalf("Error() mismatch\n--- got ---\n%s\n--- want ---\n%s", block, want.Block)
	}
	lines := strings.Split(block, "\n")
	if !strings.HasPrefix(lines[0], "peasant: ") {
		t.Fatalf("Error() first line does not start with the bare label:\n%s", lines[0])
	}
	if count := strings.Count(block, "peasant: "); count != 1 {
		t.Fatalf("Error() prints the label %d times:\n%s", count, block)
	}
	if count := strings.Count(block, "  fix:      "); count != 1 {
		t.Fatalf("Error() prints the fix %d times:\n%s", count, block)
	}
	fixSection, found := blockLabelSection(block, "fix:")
	if !found {
		t.Fatalf("Error() carries no fix section:\n%s", block)
	}
	if fixSection != fix {
		t.Fatalf("Error()'s fix section is not FixLine()\n--- got ---\n%s\n--- want ---\n%s", fixSection, fix)
	}
	for _, label := range []string{"what:", "why:", "where:", "when:", "impact:", "fix:", "searched:", "also:"} {
		if strings.Contains(block[strings.Index(block, "  fix:")+1:], "  "+label) {
			t.Fatalf("Error() prints %q after the fix:\n%s", label, block)
		}
	}

	if prefix == "" {
		if strings.Contains(block, "peasant --") {
			t.Fatalf("the bare run's block suggests a command with an unexpected prefix:\n%s", block)
		}
		return
	}
	if strings.Contains(lines[0], prefix) {
		t.Fatalf("the message label carries the run prefix:\n%s", lines[0])
	}
	for _, text := range []string{block, fix, reason} {
		if suggestsUnprefixedCommand(text, prefix) {
			t.Fatalf("a suggested command omits the run prefix:\n%s", text)
		}
	}
	if strings.Contains(fix, "`") && !strings.Contains(fix, prefix) {
		t.Fatalf("FixLine() suggests a command without the run prefix %q:\n%s", prefix, fix)
	}
}

// suggestsUnprefixedCommand reports whether text suggests a `peasant ...`
// command that does not carry the run's prefix. Every prefixed command is
// replaced first, so the prefix itself is not read as a bare command.
func suggestsUnprefixedCommand(text, prefix string) bool {
	if prefix == "" {
		return false
	}
	stripped := strings.ReplaceAll(text, "`"+prefix, "`<prefix>")
	return strings.Contains(stripped, "`peasant ")
}

// blockLabelSection returns one labelled section's text joined back into a
// single line. The block wraps the long fields, and joining with a space
// restores exactly the string the renderer was given.
func blockLabelSection(block, label string) (string, bool) {
	prefix := "  " + label + strings.Repeat(" ", 10-len(label))
	index := strings.Index(block, prefix)
	if index < 0 {
		return "", false
	}
	lines := strings.Split(block[index+len(prefix):], "\n")
	parts := []string{lines[0]}
	for _, line := range lines[1:] {
		if !strings.HasPrefix(line, strings.Repeat(" ", 12)) {
			break
		}
		parts = append(parts, strings.TrimPrefix(line, strings.Repeat(" ", 12)))
	}
	return strings.Join(parts, " "), true
}

func sessionScopeMissReason(raw string) (ingest.MissReason, error) {
	switch raw {
	case "not_found":
		return ingest.MissNotFound, nil
	case "harness_filtered":
		return ingest.MissHarnessFiltered, nil
	case "since_filtered":
		return ingest.MissSinceFiltered, nil
	default:
		return ingest.MissReason(0), fmt.Errorf("fixture miss reason %q is not a declared value", raw)
	}
}

func sessionScopeSearchMethod(raw string) (ingest.SearchMethod, error) {
	switch raw {
	case "stored_path":
		return ingest.SearchStoredPath, nil
	case "name_pattern":
		return ingest.SearchNamePattern, nil
	case "sqlite_row":
		return ingest.SearchSQLiteRow, nil
	case "unsupported":
		return ingest.SearchUnsupported, nil
	case "harness_disabled":
		return ingest.SearchHarnessDisabled, nil
	case "failed":
		return ingest.SearchFailed, nil
	default:
		return ingest.SearchMethod(0), fmt.Errorf("fixture search method %q is not a declared value", raw)
	}
}

func sessionScopeUnsupportedReason(raw string) (ingest.UnsupportedReason, error) {
	switch raw {
	case "file_header":
		return ingest.IDOnlyInFileHeader, nil
	case "file_body":
		return ingest.IDOnlyInFileBody, nil
	default:
		return ingest.UnsupportedReason(0), fmt.Errorf("fixture unsupported reason %q is not a declared value", raw)
	}
}

func sessionScopeStoreAttachment(raw string) (ingest.StoreAttachment, error) {
	switch raw {
	case "attached":
		return ingest.StoreAttached, nil
	case "not_attached":
		return ingest.StoreNotAttached, nil
	default:
		return ingest.StoreAttachment(0), fmt.Errorf("fixture store attachment %q is not a declared value", raw)
	}
}
