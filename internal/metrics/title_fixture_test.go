package metrics_test

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"testing"
	"unicode/utf8"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/metrics"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/title.yaml
var titleFixtureYAML []byte

type titleAssertion string

const (
	assertTitleExact      titleAssertion = "exact"
	assertTitleAbsent     titleAssertion = "absent"
	assertTitleUnicodeCap titleAssertion = "unicode_cap"
	assertTitleIdempotent titleAssertion = "idempotent"
)

// titleFixtureCaseCount is the exact number of cases the canonical title
// fixture must hold. It fails a silent fixture truncation or an accidental
// duplicate load, which would otherwise let whole cases stop running unnoticed.
const titleFixtureCaseCount = 15

type titleFixture struct {
	Cases []titleCase `yaml:"cases"`
}
type titleCase struct {
	Name         string         `yaml:"name"`
	Assertion    titleAssertion `yaml:"assertion"`
	Harness      schema.Harness `yaml:"harness"`
	Worktree     string         `yaml:"worktree"`
	CanonicalCWD string         `yaml:"canonical_cwd"`
	Entries      []titleEntry   `yaml:"entries"`
	Expected     string         `yaml:"expected"`
	MaxRunes     int            `yaml:"max_runes"`
	ContextError string         `yaml:"context_error"`
}
type titleEntry struct {
	Role    ingest.Role `yaml:"role"`
	Depth   int         `yaml:"depth"`
	Preview string      `yaml:"preview"`
}

type titleMetricsStore struct {
	*testutil.StubMetricsStore
	worktree, canonicalCWD string
}

func (s *titleMetricsStore) GetTitleContext(context.Context, ingest.SessionID) (schema.Harness, string, error) {
	path := s.worktree
	if path == "" {
		path = s.canonicalCWD
	}
	return s.TitleHarness, path, s.TitleContextErr
}

func loadTitleFixture(t *testing.T) titleFixture {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(titleFixtureYAML))
	decoder.KnownFields(true)
	var fixture titleFixture
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode title fixture: %v", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatalf("title fixture must contain exactly one YAML document: %v", err)
	}
	if len(fixture.Cases) != titleFixtureCaseCount {
		t.Fatalf("title fixture holds %d cases, want exactly %d; update titleFixtureCaseCount when a case is deliberately added or removed", len(fixture.Cases), titleFixtureCaseCount)
	}
	requiredNames := map[string]bool{"shared_project_path_parity": false, "canonical_cwd_fallback": false, "nested_user_is_not_title": false, "malformed_wrapper_omits_title": false, "no_root_user_omits_title": false, "context_lookup_error_omits_title": false, "unicode_code_point_cap": false, "generated_title_is_idempotent": false, "command_with_args_titles_the_argument_prose": false, "bare_command_then_prose_titles_the_prose": false, "local_command_output_then_prose_titles_the_prose": false, "codex_environment_context_then_prose_titles_the_prose": false, "system_reminder_then_prose_titles_the_prose": false, "malformed_first_candidate_then_prose_titles_the_prose": false, "only_injected_turns_omits_title": false}
	requiredArms := map[titleAssertion]bool{assertTitleExact: false, assertTitleAbsent: false, assertTitleUnicodeCap: false, assertTitleIdempotent: false}
	seen := make(map[string]struct{}, len(fixture.Cases))
	for _, tc := range fixture.Cases {
		if tc.Name == "" || len(tc.Entries) == 0 || tc.Harness == "" || (tc.Worktree == "" && tc.CanonicalCWD == "") {
			t.Fatalf("title fixture has incomplete case: %#v", tc)
		}
		if _, exists := seen[tc.Name]; exists {
			t.Fatalf("duplicate title fixture name %q", tc.Name)
		}
		if _, known := requiredArms[tc.Assertion]; !known {
			t.Fatalf("title fixture %q has unknown assertion %q", tc.Name, tc.Assertion)
		}
		if tc.Assertion == assertTitleUnicodeCap && tc.MaxRunes != 80 {
			t.Fatalf("unicode cap case must pin 80 runes")
		}
		seen[tc.Name] = struct{}{}
		requiredArms[tc.Assertion] = true
		if _, required := requiredNames[tc.Name]; required {
			requiredNames[tc.Name] = true
		}
	}
	for name, present := range requiredNames {
		if !present {
			t.Fatalf("required title fixture case %q is missing", name)
		}
	}
	for arm, present := range requiredArms {
		if !present {
			t.Fatalf("required title assertion arm %q is missing", arm)
		}
	}
	return fixture
}

func runTitleCase(tc titleCase) error {
	sid, err := ingest.NewSessionID(testutil.TestSessionUUID)
	if err != nil {
		return err
	}
	base := testutil.NewStubMetricsStore()
	base.TitleHarness = tc.Harness
	if tc.ContextError != "" {
		base.TitleContextErr = fmt.Errorf("%s", tc.ContextError)
	}
	store := &titleMetricsStore{StubMetricsStore: base, worktree: tc.Worktree, canonicalCWD: tc.CanonicalCWD}
	entries := make([]schema.SessionEntry, len(tc.Entries))
	for i := range tc.Entries {
		preview := tc.Entries[i].Preview
		entries[i] = schema.SessionEntry{SessionID: sid, EntryIndex: i, Role: tc.Entries[i].Role, Depth: tc.Entries[i].Depth, EntryType: ingest.EntryTypeText, ContentPreview: &preview}
	}
	base.IndexedEntries[sid] = entries
	engine := metrics.NewEngine(store)
	if count, computeErr := engine.ComputeMetrics(context.Background(), []ingest.SessionID{sid}); computeErr != nil || count != 1 {
		return fmt.Errorf("compute title metrics: count=%d error=%v", count, computeErr)
	}
	got := base.SavedMetrics[sid].TitleGenerated
	switch tc.Assertion {
	case assertTitleAbsent:
		if got != nil {
			return fmt.Errorf("title = %q, want absent", *got)
		}
	case assertTitleExact, assertTitleIdempotent:
		if got == nil || *got != tc.Expected {
			return fmt.Errorf("title = %v, want %q", formatTitle(got), tc.Expected)
		}
		if tc.Assertion == assertTitleIdempotent {
			tc.Entries = []titleEntry{{Role: ingest.RoleUser, Depth: 0, Preview: *got}}
			tc.Assertion = assertTitleExact
			if err := runTitleCase(tc); err != nil {
				return fmt.Errorf("idempotent second generation: %w", err)
			}
		}
	case assertTitleUnicodeCap:
		if got == nil || !utf8.ValidString(*got) || utf8.RuneCountInString(*got) > tc.MaxRunes || len([]rune(tc.Entries[0].Preview)) <= tc.MaxRunes {
			return fmt.Errorf("unicode cap output=%v runes=%d input_runes=%d", formatTitle(got), runeCount(got), utf8.RuneCountInString(tc.Entries[0].Preview))
		}
	default:
		return fmt.Errorf("unsupported assertion %q", tc.Assertion)
	}
	return nil
}

func TestEngine_CanonicalTitleFixture(t *testing.T) {
	fixture := loadTitleFixture(t)
	for _, tc := range fixture.Cases {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			if err := runTitleCase(tc); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestEngine_TitleFixtureMutationIsDetected(t *testing.T) {
	fixture := loadTitleFixture(t)
	var mutated titleCase
	for _, tc := range fixture.Cases {
		if tc.Name == "nested_user_is_not_title" {
			mutated = tc
			break
		}
	}
	mutated.Entries[0].Depth = 0
	if err := runTitleCase(mutated); err == nil {
		t.Fatal("depth-selector mutation passed; fixture does not prove the production selector")
	}
}

func formatTitle(value *string) string {
	if value == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%q", *value)
}
func runeCount(value *string) int {
	if value == nil {
		return 0
	}
	return utf8.RuneCountInString(*value)
}
