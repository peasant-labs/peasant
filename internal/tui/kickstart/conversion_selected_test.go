package kickstart_test

import (
	"bytes"
	_ "embed"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
)

//go:embed testdata/legacy_selected_conversion.yaml
var legacySelectedConversionData []byte

type legacySelectedConversionDocument struct {
	ExpectedCaseCount int                            `yaml:"expectedCaseCount"`
	ExpectedCaseNames []string                       `yaml:"expectedCaseNames"`
	Cases             []legacySelectedConversionCase `yaml:"cases"`
}

type legacySelectedConversionCase struct {
	Name                      string                 `yaml:"name"`
	ExpectedStoredCount       *int                   `yaml:"expectedStoredCount"`
	ExpectedResolverCallCount *int                   `yaml:"expectedResolverCallCount"`
	Current                   config.SelectionConfig `yaml:"current"`
	Stored                    []legacyAllStoredRow   `yaml:"stored"`
	ResolverFailures          []string               `yaml:"resolverFailures"`
	ExpectedResolverCalls     []string               `yaml:"expectedResolverCalls"`
	Expected                  config.SelectionConfig `yaml:"expected"`
}

func loadLegacySelectedConversionDocument(t *testing.T) legacySelectedConversionDocument {
	t.Helper()
	var document legacySelectedConversionDocument
	if err := decodeStrictFixture(legacySelectedConversionData, &document); err != nil {
		t.Fatalf("decode legacy selected conversion fixture: %v", err)
	}
	if document.ExpectedCaseCount != len(document.Cases) || document.ExpectedCaseCount == 0 {
		t.Fatalf("expectedCaseCount=%d but %d cases are present", document.ExpectedCaseCount, len(document.Cases))
	}
	if len(document.ExpectedCaseNames) != document.ExpectedCaseCount {
		t.Fatalf("expectedCaseNames has %d names, want %d", len(document.ExpectedCaseNames), document.ExpectedCaseCount)
	}

	seen := make(map[string]struct{}, len(document.Cases))
	actualNames := make([]string, 0, len(document.Cases))
	for _, testCase := range document.Cases {
		testutil.RequireFixtureFields(t, "legacy selected conversion", testCase.Name, []testutil.FixtureField{
			{Key: "name", Value: testCase.Name},
			{Key: "expectedStoredCount", Value: fixtureCountValue(testCase.ExpectedStoredCount)},
			{Key: "expectedResolverCallCount", Value: fixtureCountValue(testCase.ExpectedResolverCallCount)},
		})
		if _, duplicate := seen[testCase.Name]; duplicate {
			t.Fatalf("legacy selected conversion fixture repeats case name %q", testCase.Name)
		}
		seen[testCase.Name] = struct{}{}
		actualNames = append(actualNames, testCase.Name)
		if *testCase.ExpectedStoredCount != len(testCase.Stored) {
			t.Fatalf("case %q expectedStoredCount=%d but %d rows are present", testCase.Name, *testCase.ExpectedStoredCount, len(testCase.Stored))
		}
		if *testCase.ExpectedResolverCallCount != len(testCase.ExpectedResolverCalls) {
			t.Fatalf("case %q expectedResolverCallCount=%d but %d calls are present", testCase.Name, *testCase.ExpectedResolverCallCount, len(testCase.ExpectedResolverCalls))
		}
		failures := make(map[string]struct{}, len(testCase.ResolverFailures))
		for _, raw := range testCase.ResolverFailures {
			if raw == "" {
				t.Fatalf("case %q has an empty resolver failure", testCase.Name)
			}
			if _, duplicate := failures[raw]; duplicate {
				t.Fatalf("case %q repeats resolver failure %q", testCase.Name, raw)
			}
			failures[raw] = struct{}{}
		}
	}
	if !reflect.DeepEqual(actualNames, document.ExpectedCaseNames) {
		t.Fatalf("legacy selected conversion names = %v, want exact manifest %v", actualNames, document.ExpectedCaseNames)
	}
	return document
}

type selectedConversionResolver struct {
	calls    []string
	failures map[string]struct{}
}

func (r *selectedConversionResolver) Resolve(raw string) (ingest.ClonePath, error) {
	r.calls = append(r.calls, raw)
	if _, fail := r.failures[raw]; fail {
		return "", fmt.Errorf("fixture rejects path %q", raw)
	}
	if raw == "" || !filepath.IsAbs(raw) || filepath.Clean(raw) != raw {
		return "", fmt.Errorf("fixture path %q is not a clean absolute path", raw)
	}
	return ingest.ClonePath(raw), nil
}

var _ ingest.PathIdentityResolver = (*selectedConversionResolver)(nil)

func TestConvertLegacySelected_StoredCloneCorpus(t *testing.T) {
	t.Parallel()
	document := loadLegacySelectedConversionDocument(t)
	for _, testCase := range document.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			stored := make([]store.IngestedSessionRow, len(testCase.Stored))
			for index, row := range testCase.Stored {
				stored[index] = row.productionRow()
			}
			storedBefore := append([]store.IngestedSessionRow(nil), stored...)
			currentBefore := cloneSelectedFixtureSelection(testCase.Current)
			failures := make(map[string]struct{}, len(testCase.ResolverFailures))
			for _, raw := range testCase.ResolverFailures {
				failures[raw] = struct{}{}
			}
			resolver := &selectedConversionResolver{calls: []string{}, failures: failures}

			converted, err := kickstart.ConvertLegacySelected(testCase.Current, stored, resolver)
			if err != nil {
				t.Fatalf("convert legacy selected: %v", err)
			}
			if !reflect.DeepEqual(converted, testCase.Expected) {
				t.Fatalf("converted selection mismatch\n got: %#v\nwant: %#v", converted, testCase.Expected)
			}
			if !reflect.DeepEqual(testCase.Current, currentBefore) {
				t.Fatalf("conversion mutated current selection\n got: %#v\nwant: %#v", testCase.Current, currentBefore)
			}
			if !reflect.DeepEqual(stored, storedBefore) {
				t.Fatalf("conversion mutated stored rows\n got: %#v\nwant: %#v", stored, storedBefore)
			}
			if !reflect.DeepEqual(resolver.calls, testCase.ExpectedResolverCalls) {
				t.Fatalf("resolver calls = %v, want %v", resolver.calls, testCase.ExpectedResolverCalls)
			}
			assertNoPathlessSelectedProjects(t, converted)
			assertSelectedPathsUniquePerHarness(t, converted)

			rerun, err := kickstart.ConvertLegacySelected(converted, stored, nil)
			if err != nil {
				t.Fatalf("rerun exact selected conversion: %v", err)
			}
			if !reflect.DeepEqual(rerun, converted) {
				t.Fatalf("exact selected rerun drifted\n got: %#v\nwant: %#v", rerun, converted)
			}
		})
	}
}

func TestConvertLegacySelected_RequiresResolverForPathlessRules(t *testing.T) {
	t.Parallel()
	current := config.SelectionConfig{
		Mode: config.SelectionModeSelected,
		Harnesses: map[string]config.SelectionHarnessConfig{
			"claude-code": {Projects: []config.ProjectSelection{{GitRemote: "github.com/acme/tool"}}},
		},
	}
	_, err := kickstart.ConvertLegacySelected(current, nil, nil)
	if err == nil {
		t.Fatal("pathless selected conversion accepted a nil identity resolver")
	}
	message := strings.ToLower(err.Error())
	if !strings.Contains(message, "what:") || !strings.Contains(message, "why:") ||
		!strings.Contains(message, "where:") || !strings.Contains(message, "when:") ||
		!strings.Contains(message, "meaning:") || !strings.Contains(message, "fix:") {
		t.Fatalf("nil-resolver error is not actionable: %v", err)
	}
}

func TestConvertLegacySelected_ExactCopyDoesNotAliasInput(t *testing.T) {
	t.Parallel()
	current := config.SelectionConfig{
		Mode: config.SelectionModeSelected,
		Harnesses: map[string]config.SelectionHarnessConfig{
			"claude-code": {
				Projects: []config.ProjectSelection{{
					ClonePaths: []string{"/fixtures/exact"},
					Branches:   []string{"main"},
				}},
				Sessions: []string{"10000000-0000-4000-8000-000000000001"},
				Exclusions: config.SelectionExclusions{
					Branches: []config.BranchExclusion{{
						ClonePath: "/fixtures/exact",
						Branches:  []string{"private"},
					}},
					Sessions: []string{"10000000-0000-4000-8000-000000000009"},
				},
			},
		},
	}
	before := cloneSelectedFixtureSelection(current)
	converted, err := kickstart.ConvertLegacySelected(current, nil, nil)
	if err != nil {
		t.Fatalf("copy exact selected config: %v", err)
	}
	configured := converted.Harnesses["claude-code"]
	configured.Projects[0].ClonePaths[0] = "/fixtures/changed"
	configured.Projects[0].Branches[0] = "changed"
	configured.Sessions[0] = "changed"
	configured.Exclusions.Branches[0].Branches[0] = "changed"
	configured.Exclusions.Sessions[0] = "changed"
	converted.Harnesses["claude-code"] = configured
	if !reflect.DeepEqual(current, before) {
		t.Fatalf("mutating converted exact copy changed input\n got: %#v\nwant: %#v", current, before)
	}
}

func TestLegacySelectedConversionFixtureRejectsUnknownExclusionKey(t *testing.T) {
	malformed := bytes.Replace(legacySelectedConversionData, []byte("exclusions:"), []byte("exclusionsTypo:"), 1)
	if bytes.Equal(malformed, legacySelectedConversionData) {
		t.Fatal("legacy selected conversion fixture has no exclusions key to mutate")
	}
	var document legacySelectedConversionDocument
	if err := decodeStrictFixture(malformed, &document); err == nil {
		t.Fatal("legacy selected conversion fixture decoder accepted an unknown exclusion key")
	}
}

func cloneSelectedFixtureSelection(source config.SelectionConfig) config.SelectionConfig {
	clone := source
	clone.Harnesses = cloneSelectedFixtureHarnesses(source.Harnesses)
	clone.DeprecatedProviders = cloneSelectedFixtureHarnesses(source.DeprecatedProviders)
	return clone
}

func cloneSelectedFixtureHarnesses(source map[string]config.SelectionHarnessConfig) map[string]config.SelectionHarnessConfig {
	if source == nil {
		return nil
	}
	clone := make(map[string]config.SelectionHarnessConfig, len(source))
	for harness, configured := range source {
		copy := configured
		if configured.Projects != nil {
			copy.Projects = make([]config.ProjectSelection, len(configured.Projects))
			for index, project := range configured.Projects {
				copy.Projects[index] = project
				copy.Projects[index].ClonePaths = appendPreservingNil(project.ClonePaths)
				copy.Projects[index].Branches = appendPreservingNil(project.Branches)
			}
		}
		copy.Sessions = appendPreservingNil(configured.Sessions)
		if configured.Exclusions.Branches != nil {
			copy.Exclusions.Branches = make([]config.BranchExclusion, len(configured.Exclusions.Branches))
			for index, exclusion := range configured.Exclusions.Branches {
				copy.Exclusions.Branches[index] = exclusion
				copy.Exclusions.Branches[index].Branches = appendPreservingNil(exclusion.Branches)
			}
		}
		copy.Exclusions.Sessions = appendPreservingNil(configured.Exclusions.Sessions)
		clone[harness] = copy
	}
	return clone
}

func appendPreservingNil(values []string) []string {
	if values == nil {
		return nil
	}
	return append([]string{}, values...)
}

func assertNoPathlessSelectedProjects(t *testing.T, selection config.SelectionConfig) {
	t.Helper()
	if selection.Mode != config.SelectionModeSelected {
		return
	}
	for harness, configured := range selection.Harnesses {
		for index, project := range configured.Projects {
			if len(project.ClonePaths) == 0 {
				t.Errorf("harness %q project %d remains pathless after selected migration", harness, index)
			}
		}
	}
}

func assertSelectedPathsUniquePerHarness(t *testing.T, selection config.SelectionConfig) {
	t.Helper()
	for harness, configured := range selection.Harnesses {
		seen := map[string]int{}
		for projectIndex, project := range configured.Projects {
			for _, clonePath := range project.ClonePaths {
				if first, duplicate := seen[clonePath]; duplicate {
					t.Errorf("harness %q clone path %q occurs in projects %d and %d", harness, clonePath, first, projectIndex)
				}
				seen[clonePath] = projectIndex
			}
		}
	}
}
