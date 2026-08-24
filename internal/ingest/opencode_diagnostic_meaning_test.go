package ingest_test

import (
	"bytes"
	_ "embed"
	"errors"
	"io"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/opencode_diagnostic_meaning.yaml
var openCodeDiagnosticMeaningYAML []byte

// openCodeDiagnosticMeaningCase pins the rendered Meaning and Remediation a
// discovery diagnostic must carry for one outcome, so a non-skip event cannot
// silently reuse the skip template.
type openCodeDiagnosticMeaningCase struct {
	Name                        string `yaml:"name"`
	ExpectedMeaningContains     string `yaml:"expected_meaning_contains"`
	ForbiddenMeaningContains    string `yaml:"forbidden_meaning_contains"`
	ExpectedRemediationContains string `yaml:"expected_remediation_contains"`
}

type openCodeDiagnosticMeaningFixture struct {
	RequiredCases []string                        `yaml:"required_cases"`
	Cases         []openCodeDiagnosticMeaningCase `yaml:"cases"`
}

func loadOpenCodeDiagnosticMeaningFixture(t testing.TB) openCodeDiagnosticMeaningFixture {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(openCodeDiagnosticMeaningYAML))
	decoder.KnownFields(true)
	var fixture openCodeDiagnosticMeaningFixture
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode OpenCode diagnostic meaning fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("decode OpenCode diagnostic meaning fixture: expected exactly one YAML document: %v", err)
	}
	presentDiag := make(map[string]struct{}, len(fixture.Cases))
	for _, testCase := range fixture.Cases {
		presentDiag[testCase.Name] = struct{}{}
	}
	if len(fixture.RequiredCases) == 0 {
		t.Fatal("OpenCode diagnostic meaning fixture declares no required cases")
	}
	for _, name := range fixture.RequiredCases {
		if _, ok := presentDiag[name]; !ok {
			t.Fatalf("OpenCode diagnostic meaning fixture is missing required case %q", name)
		}
	}
	for _, testCase := range fixture.Cases {
		if strings.TrimSpace(testCase.Name) == "" || strings.TrimSpace(testCase.ExpectedMeaningContains) == "" || strings.TrimSpace(testCase.ForbiddenMeaningContains) == "" || strings.TrimSpace(testCase.ExpectedRemediationContains) == "" {
			t.Fatalf("OpenCode diagnostic meaning fixture case is incomplete: %+v", testCase)
		}
	}
	return fixture
}

// TestOpenCodeDanglingParentDiagnosticMeaningMatchesOutcome proves that the
// dangling-parent diagnostic reports what actually happened: the child was
// ingested as a root because its parent was not discovered. Its Meaning must not
// claim the session was skipped, and its Remediation must point at discovering
// the parent rather than retrying a write.
func TestOpenCodeDanglingParentDiagnosticMeaningMatchesOutcome(t *testing.T) {
	fixture := loadOpenCodeDiagnosticMeaningFixture(t)
	testCase := fixture.Cases[0]
	const absentParent = "ses_3cd91f52effeXd3QAJ54jOyzP9"
	_, evidence, path := discoverDanglingChild(t, absentParent)
	diagnostic := discoveryFailedDiagnostic(evidence, path)
	if diagnostic == nil {
		t.Fatalf("dangling parent recorded no diagnostic: evidence=%+v", evidence)
	}
	if !strings.Contains(diagnostic.Meaning, testCase.ExpectedMeaningContains) {
		t.Fatalf("diagnostic meaning = %q, want it to contain %q", diagnostic.Meaning, testCase.ExpectedMeaningContains)
	}
	if strings.Contains(diagnostic.Meaning, testCase.ForbiddenMeaningContains) {
		t.Fatalf("diagnostic meaning = %q, want it not to contain %q because the sessions were ingested as roots, not skipped", diagnostic.Meaning, testCase.ForbiddenMeaningContains)
	}
	if !strings.Contains(diagnostic.Remediation, testCase.ExpectedRemediationContains) {
		t.Fatalf("diagnostic remediation = %q, want it to contain %q", diagnostic.Remediation, testCase.ExpectedRemediationContains)
	}
}
