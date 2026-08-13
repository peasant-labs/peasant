package kickstart_test

import (
	"bytes"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
)

//go:embed testdata/legacy_all_conversion.yaml
var legacyAllConversionData []byte

type legacyAllConversionDocument struct {
	ExpectedCaseCount int                       `yaml:"expectedCaseCount"`
	Cases             []legacyAllConversionCase `yaml:"cases"`
}

type legacyAllConversionCase struct {
	Name                         string                                   `yaml:"name"`
	ExpectedStoredCount          *int                                     `yaml:"expectedStoredCount"`
	ExpectedScanCount            *int                                     `yaml:"expectedScanCount"`
	ExpectedResolverCallCount    *int                                     `yaml:"expectedResolverCallCount"`
	ExpectedSelectedProjectCount *int                                     `yaml:"expectedSelectedProjectCount"`
	ExpectedUnmatchedCount       *int                                     `yaml:"expectedUnmatchedCount"`
	AutoIngestNewBranches        *bool                                    `yaml:"autoIngestNewBranches"`
	Stored                       []legacyAllStoredRow                     `yaml:"stored"`
	Scan                         []ftue.SessionListing                    `yaml:"scan"`
	ExpectedResolverCalls        []string                                 `yaml:"expectedResolverCalls"`
	ExpectedInitialHarnesses     map[string]config.SelectionHarnessConfig `yaml:"expectedInitialHarnesses"`
	ExpectedUnmatchedHarnesses   map[string]config.SelectionHarnessConfig `yaml:"expectedUnmatchedHarnesses"`
}

type legacyAllStoredRow struct {
	SessionID     string `yaml:"sessionId"`
	Harness       string `yaml:"harness"`
	GitRemote     string `yaml:"gitRemote"`
	Branch        string `yaml:"branch"`
	GitWorktree   string `yaml:"gitWorktree"`
	CanonicalCwd  string `yaml:"canonicalCwd"`
	Title         string `yaml:"title"`
	IngestedMs    int64  `yaml:"ingestedMs"`
	SchemaVersion int    `yaml:"schemaVersion"`
}

func (r legacyAllStoredRow) productionRow() store.IngestedSessionRow {
	return store.IngestedSessionRow{
		SessionID:     r.SessionID,
		Harness:       r.Harness,
		GitRemote:     r.GitRemote,
		Branch:        r.Branch,
		GitWorktree:   r.GitWorktree,
		CanonicalCwd:  r.CanonicalCwd,
		Title:         r.Title,
		IngestedMs:    r.IngestedMs,
		SchemaVersion: r.SchemaVersion,
	}
}

func loadLegacyAllConversionDocument(t *testing.T) legacyAllConversionDocument {
	t.Helper()
	var document legacyAllConversionDocument
	if err := decodeStrictFixture(legacyAllConversionData, &document); err != nil {
		t.Fatalf("decode legacy all conversion fixture: %v", err)
	}
	if document.ExpectedCaseCount != len(document.Cases) || len(document.Cases) == 0 {
		t.Fatalf("expectedCaseCount=%d but %d cases present", document.ExpectedCaseCount, len(document.Cases))
	}
	seen := map[string]struct{}{}
	for _, testCase := range document.Cases {
		testutil.RequireFixtureFields(t, "legacy all conversion", testCase.Name, []testutil.FixtureField{
			{Key: "name", Value: testCase.Name},
			{Key: "expectedStoredCount", Value: fixtureCountValue(testCase.ExpectedStoredCount)},
			{Key: "expectedScanCount", Value: fixtureCountValue(testCase.ExpectedScanCount)},
			{Key: "expectedResolverCallCount", Value: fixtureCountValue(testCase.ExpectedResolverCallCount)},
			{Key: "expectedSelectedProjectCount", Value: fixtureCountValue(testCase.ExpectedSelectedProjectCount)},
			{Key: "expectedUnmatchedCount", Value: fixtureCountValue(testCase.ExpectedUnmatchedCount)},
			{Key: "autoIngestNewBranches", Value: fixtureBoolValue(testCase.AutoIngestNewBranches)},
		})
		if _, duplicate := seen[testCase.Name]; duplicate {
			t.Fatalf("legacy all conversion fixture repeats case name %q", testCase.Name)
		}
		seen[testCase.Name] = struct{}{}
		if *testCase.ExpectedStoredCount != len(testCase.Stored) {
			t.Fatalf("case %q expectedStoredCount=%d but %d rows present", testCase.Name, *testCase.ExpectedStoredCount, len(testCase.Stored))
		}
		if *testCase.ExpectedScanCount != len(testCase.Scan) || len(testCase.Scan) == 0 {
			t.Fatalf("case %q expectedScanCount=%d but %d rows present", testCase.Name, *testCase.ExpectedScanCount, len(testCase.Scan))
		}
		if *testCase.ExpectedResolverCallCount != len(testCase.ExpectedResolverCalls) {
			t.Fatalf("case %q expectedResolverCallCount=%d but %d calls present", testCase.Name, *testCase.ExpectedResolverCallCount, len(testCase.ExpectedResolverCalls))
		}
		if *testCase.ExpectedSelectedProjectCount != selectedProjectCount(testCase.ExpectedInitialHarnesses) {
			t.Fatalf("case %q expectedSelectedProjectCount=%d but expected harnesses contain %d projects", testCase.Name, *testCase.ExpectedSelectedProjectCount, selectedProjectCount(testCase.ExpectedInitialHarnesses))
		}
		if *testCase.ExpectedUnmatchedCount != explicitSessionCount(testCase.ExpectedUnmatchedHarnesses) {
			t.Fatalf("case %q expectedUnmatchedCount=%d but expected harnesses contain %d sessions", testCase.Name, *testCase.ExpectedUnmatchedCount, explicitSessionCount(testCase.ExpectedUnmatchedHarnesses))
		}
	}
	return document
}

func fixtureCountValue(value *int) string {
	if value == nil {
		return ""
	}
	return strconv.Itoa(*value)
}

func fixtureBoolValue(value *bool) string {
	if value == nil {
		return ""
	}
	return strconv.FormatBool(*value)
}

func selectedProjectCount(harnesses map[string]config.SelectionHarnessConfig) int {
	count := 0
	for _, harness := range harnesses {
		count += len(harness.Projects)
	}
	return count
}

func explicitSessionCount(harnesses map[string]config.SelectionHarnessConfig) int {
	count := 0
	for _, harness := range harnesses {
		count += len(harness.Sessions)
	}
	return count
}

type recordingPreResolvedResolver struct {
	calls []string
}

func (r *recordingPreResolvedResolver) Resolve(raw string) (ingest.ClonePath, error) {
	r.calls = append(r.calls, raw)
	if raw == "" || !filepath.IsAbs(raw) || filepath.Clean(raw) != raw {
		return "", fmt.Errorf("fixture path %q is not a clean absolute path", raw)
	}
	return ingest.ClonePath(raw), nil
}

var _ ingest.PathIdentityResolver = (*recordingPreResolvedResolver)(nil)

func TestConvertLegacyAll_StoredEvidenceCorpus(t *testing.T) {
	t.Parallel()
	document := loadLegacyAllConversionDocument(t)
	for _, testCase := range document.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			stored := make([]store.IngestedSessionRow, len(testCase.Stored))
			for index, row := range testCase.Stored {
				stored[index] = row.productionRow()
			}
			storedBefore := append(make([]store.IngestedSessionRow, 0, len(stored)), stored...)
			scanBefore := cloneSessionListings(testCase.Scan)
			resolver := &recordingPreResolvedResolver{}

			conversion, err := kickstart.ConvertLegacyAll(testCase.Scan, stored, resolver, *testCase.AutoIngestNewBranches)
			if err != nil {
				t.Fatalf("convert legacy all: %v", err)
			}

			wantInitial := config.SelectionConfig{
				Mode:                  config.SelectionModeSelected,
				AutoIngestNewBranches: *testCase.AutoIngestNewBranches,
				Harnesses:             testCase.ExpectedInitialHarnesses,
			}
			if !reflect.DeepEqual(conversion.Initial, wantInitial) {
				t.Fatalf("initial selection mismatch\n got: %#v\nwant: %#v", conversion.Initial, wantInitial)
			}
			if !reflect.DeepEqual(conversion.Unmatched.Harnesses, testCase.ExpectedUnmatchedHarnesses) {
				t.Fatalf("unmatched baseline mismatch\n got: %#v\nwant: %#v", conversion.Unmatched.Harnesses, testCase.ExpectedUnmatchedHarnesses)
			}
			if got := selectedProjectCount(conversion.Initial.Harnesses); got != *testCase.ExpectedSelectedProjectCount {
				t.Fatalf("selected projects = %d, want %d", got, *testCase.ExpectedSelectedProjectCount)
			}
			if got := explicitSessionCount(conversion.Unmatched.Harnesses); got != *testCase.ExpectedUnmatchedCount {
				t.Fatalf("unmatched sessions = %d, want %d", got, *testCase.ExpectedUnmatchedCount)
			}
			if !reflect.DeepEqual(resolver.calls, testCase.ExpectedResolverCalls) {
				t.Fatalf("resolver calls = %v, want %v", resolver.calls, testCase.ExpectedResolverCalls)
			}
			if !reflect.DeepEqual(stored, storedBefore) {
				t.Fatalf("conversion mutated stored rows\n got: %#v\nwant: %#v", stored, storedBefore)
			}
			if !reflect.DeepEqual(testCase.Scan, scanBefore) {
				t.Fatalf("conversion mutated scanner rows\n got: %#v\nwant: %#v", testCase.Scan, scanBefore)
			}
		})
	}
}

func TestLegacyAllConversionFixtureRejectsUnknownStoredIdentityKey(t *testing.T) {
	malformed := bytes.Replace(legacyAllConversionData, []byte("gitWorktree:"), []byte("gitWorktreeTypo:"), 1)
	if bytes.Equal(malformed, legacyAllConversionData) {
		t.Fatal("legacy all conversion fixture has no gitWorktree key to mutate")
	}
	var document legacyAllConversionDocument
	if err := decodeStrictFixture(malformed, &document); err == nil {
		t.Fatal("legacy all conversion fixture decoder accepted an unknown stored identity key")
	}
}

type recordingPhysicalResolver struct {
	delegate ingest.PathIdentityResolver
	calls    []string
}

func (r *recordingPhysicalResolver) Resolve(raw string) (ingest.ClonePath, error) {
	r.calls = append(r.calls, raw)
	return r.delegate.Resolve(raw)
}

var _ ingest.PathIdentityResolver = (*recordingPhysicalResolver)(nil)

func TestConvertLegacyAll_UnresolvableWorktreeDoesNotUseCanonicalFallback(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	canonical := filepath.Join(root, "available", "tool")
	if err := os.MkdirAll(canonical, 0o755); err != nil {
		t.Fatalf("create available scanner project: %v", err)
	}
	missingWorktree := filepath.Join(root, "missing", "tool")
	resolver := &recordingPhysicalResolver{delegate: ingest.NewPhysicalPathResolver()}
	stored := []store.IngestedSessionRow{{
		SessionID:    "stored-session",
		Harness:      "claude-code",
		GitRemote:    "git@github.com:acme/tool.git",
		GitWorktree:  missingWorktree,
		CanonicalCwd: canonical,
	}}
	scan := []ftue.SessionListing{{
		SessionID:   "stored-session",
		Harness:     "claude-code",
		ProjectName: "tool",
		GitRemote:   "git@github.com:acme/tool.git",
		WorkingDir:  canonical,
	}}

	conversion, err := kickstart.ConvertLegacyAll(scan, stored, resolver, true)
	if err != nil {
		t.Fatalf("convert legacy all: %v", err)
	}
	if len(conversion.Initial.Harnesses) != 0 {
		t.Fatalf("unresolvable stored worktree selected the canonical fallback project: %#v", conversion.Initial.Harnesses)
	}
	if !conversion.Initial.AutoIngestNewBranches {
		t.Fatal("conversion changed enabled auto-ingest-new-branches policy")
	}
	wantUnmatched := map[string]config.SelectionHarnessConfig{
		"claude-code": {Sessions: []string{"stored-session"}},
	}
	if !reflect.DeepEqual(conversion.Unmatched.Harnesses, wantUnmatched) {
		t.Fatalf("unmatched baseline = %#v, want %#v", conversion.Unmatched.Harnesses, wantUnmatched)
	}
	wantCalls := []string{missingWorktree, canonical}
	if !reflect.DeepEqual(resolver.calls, wantCalls) {
		t.Fatalf("resolver calls = %v, want only chosen worktree then scanner path %v", resolver.calls, wantCalls)
	}
}

func TestConvertLegacyAll_RequiresIdentityResolver(t *testing.T) {
	t.Parallel()
	_, err := kickstart.ConvertLegacyAll(nil, nil, nil, false)
	if err == nil {
		t.Fatal("conversion accepted a nil identity resolver")
	}
	message := strings.ToLower(err.Error())
	if !strings.Contains(message, "what:") || !strings.Contains(message, "why:") ||
		!strings.Contains(message, "where:") || !strings.Contains(message, "when:") ||
		!strings.Contains(message, "meaning:") || !strings.Contains(message, "fix:") {
		t.Fatalf("nil-resolver error is not actionable: %v", err)
	}
}

func cloneSessionListings(source []ftue.SessionListing) []ftue.SessionListing {
	clone := append([]ftue.SessionListing(nil), source...)
	for index := range clone {
		clone[index].SubagentIDs = append([]string(nil), source[index].SubagentIDs...)
	}
	return clone
}
