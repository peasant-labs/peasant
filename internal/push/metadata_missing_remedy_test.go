package push_test

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/githooks"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/metadata_missing_remedy.yaml
var metadataMissingRemedyFixtureData []byte

const metadataMissingRemedyFixturePath = "internal/push/testdata/metadata_missing_remedy.yaml"

// The session and binding the corpus is driven with. The binding is bound so the
// recommended command is provably rendered in the same config/data context the
// push ran in, rather than as a bare 'peasant ingest'.
const (
	remedySessionID  = "44444444-4444-4444-8444-444444444444"
	remedyBoundData  = "/tmp/bound data"
	remedyOutputBase = "/sync"
)

type metadataMissingRemedyDocument struct {
	ExpectedCaseCount int                         `yaml:"expectedCaseCount"`
	Cases             []metadataMissingRemedyCase `yaml:"cases"`
}

type metadataMissingRemedyCase struct {
	Name           string   `yaml:"name"`
	HostSlug       string   `yaml:"hostSlug"`
	ExpectRemedy   bool     `yaml:"expectRemedy"`
	MustContain    []string `yaml:"mustContain"`
	MustNotContain []string `yaml:"mustNotContain"`
}

// loadMetadataMissingRemedyFixture decodes and fully validates the corpus.
func loadMetadataMissingRemedyFixture(data []byte) (metadataMissingRemedyDocument, error) {
	var document metadataMissingRemedyDocument
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return document, metadataMissingRemedyRuleError(
			"typed YAML fields must match the document schema", "loader=first-document decode",
			fmt.Sprintf("fix=remove unknown fields and match the typed schema: %v", err))
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found another YAML document")
		}
		return document, metadataMissingRemedyRuleError(
			"exactly one YAML document is allowed; cases below a second one prove nothing",
			"loader=end-of-document check",
			fmt.Sprintf("fix=remove the second document so the next decode returns EOF: %v", err))
	}
	if len(document.Cases) == 0 || document.ExpectedCaseCount != len(document.Cases) {
		return document, metadataMissingRemedyRuleError(
			fmt.Sprintf("declared and actual case counts must match and be non-zero, got expectedCaseCount=%d cases=%d",
				document.ExpectedCaseCount, len(document.Cases)),
			"loader=case-count validation",
			"fix=set expectedCaseCount to the number of cases present")
	}
	seen := map[string]bool{}
	sawGatedOut := false
	for index, testCase := range document.Cases {
		if strings.TrimSpace(testCase.Name) == "" || seen[testCase.Name] {
			return document, metadataMissingRemedyRuleError(
				fmt.Sprintf("case name %q is missing or duplicated", testCase.Name),
				fmt.Sprintf("loader=case index %d", index),
				"fix=give every case a unique, behaviour-naming name")
		}
		seen[testCase.Name] = true
		if strings.TrimSpace(testCase.HostSlug) == "" {
			return document, metadataMissingRemedyRuleError(
				fmt.Sprintf("case %q has no hostSlug", testCase.Name),
				fmt.Sprintf("loader=case index %d", index),
				"fix=state the recorded slug; it is the only thing the gate reads")
		}
		if len(testCase.MustContain) == 0 {
			return document, metadataMissingRemedyRuleError(
				fmt.Sprintf("case %q asserts nothing", testCase.Name),
				fmt.Sprintf("loader=case index %d", index),
				"fix=state what this failure has to tell the user")
		}
		if !testCase.ExpectRemedy {
			sawGatedOut = true
		}
	}
	if !sawGatedOut {
		return document, metadataMissingRemedyRuleError(
			"no case expects the repair to be withheld",
			"loader=gate coverage",
			"fix=add a case with an intact slug; without one, a remedy that fires on every cause of a missing metadata file passes")
	}
	return document, nil
}

func metadataMissingRemedyRuleError(what, where, fix string) error {
	return fmt.Errorf(
		"metadata-missing remedy fixture rule failed: %s; a malformed corpus invalidates the only evidence that the repair is "+
			"recommended when it helps and withheld when it would be wrong; where=%s %s; when=test fixture loading; "+
			"impact=a user could be told to force a re-ingest that cannot fix their problem; %s",
		what, metadataMissingRemedyFixturePath, where, fix)
}

// --- loader guards ----------------------------------------------------------

func TestLoadMetadataMissingRemedyFixture_RejectsACorpusWithNoWithheldCase(t *testing.T) {
	t.Parallel()
	_, err := loadMetadataMissingRemedyFixture([]byte(`expectedCaseCount: 1
cases:
  - name: only-the-broken-slug
    hostSlug: "<HIGH_ENTROPY>"
    expectRemedy: true
    mustContain: ["redaction placeholder"]
`))
	if err == nil || !strings.Contains(err.Error(), "no case expects the repair to be withheld") {
		t.Fatalf("error = %v, want rejection of a corpus that cannot detect a blanket remedy", err)
	}
}

// --- the corpus -------------------------------------------------------------

// TestPipeline_MetadataMissing_RecommendsAReIngestOnlyForARedactedSlug drives the
// real pipeline against a store row whose metadata file is not on disk.
//
// This is the state an install left permanently unable to publish: the clamp that
// stops new imports from corrupting a slug does not heal the rows already
// written, because the host-slug insert ignores conflicts and re-ingest skips an
// unchanged session. So the failure itself has to name the repair - and it has to
// name it ONLY for the cause it actually repairs.
func TestPipeline_MetadataMissing_RecommendsAReIngestOnlyForARedactedSlug(t *testing.T) {
	t.Parallel()
	document, err := loadMetadataMissingRemedyFixture(metadataMissingRemedyFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range document.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			failure := runMetadataMissingPush(t, testCase.HostSlug)
			message := failure.Error()
			if got := push.ClassifyPushError(failure); got != push.CategoryMetadataMissing {
				t.Errorf("category = %q, want %q: the remedy must not change how the failure is grouped", got, push.CategoryMetadataMissing)
			}
			for _, want := range testCase.MustContain {
				if !strings.Contains(message, want) {
					t.Errorf("the failure must state %q; got:\n%s", want, message)
				}
			}
			for _, forbidden := range testCase.MustNotContain {
				if strings.Contains(message, forbidden) {
					t.Errorf("the failure must not state %q; got:\n%s", forbidden, message)
				}
			}
			scoped := "peasant --data-dir '" + remedyBoundData + "' ingest --force --session '" + remedySessionID + "'"
			if !testCase.ExpectRemedy {
				// The whole point of the gate: a deleted output tree or a moved
				// output path is not repaired by a forced re-ingest.
				if strings.Contains(message, "ingest --force") {
					t.Errorf("a forced re-ingest must not be recommended for a cause it cannot repair; got:\n%s", message)
				}
				return
			}
			for _, want := range []string{
				scoped,
				remedyOutputBase,
				remedySessionID,
				"clears every already-published marker",
			} {
				if !strings.Contains(message, want) {
					t.Errorf("the repair must state %q so it is runnable from the state that printed it; got:\n%s", want, message)
				}
			}
			if strings.Contains(message, "peasant ingest --force --session") {
				t.Errorf("the repair must carry the bound paths this push ran with, not a bare command; got:\n%s", message)
			}
		})
	}
}

// runMetadataMissingPush pushes one session whose metadata file was never
// written, and returns the per-session failure. The filesystem is left empty on
// purpose: that is what the recorded-slug defect looks like from push's side.
func runMetadataMissingPush(t *testing.T, hostSlug string) error {
	t.Helper()
	store := &testutil.StubPushStore{
		Sessions: []ingest.PushSessionRow{
			makeSession(remedySessionID, hostSlug, string(defaults.HarnessClaudeCode), nil),
		},
	}
	var stderr bytes.Buffer
	pipeline := newTestPipeline(store, &testutil.StubPublisher{}, testutil.NewMemFS(), baseTestConfig(),
		push.PipelineConfig{CommandBinding: githooks.Binding{DataDir: remedyBoundData}}, &stderr)

	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(result.Sessions) != 1 || result.Sessions[0].Status != push.PushStatusError {
		t.Fatalf("expected exactly one failed session, got %+v", result.Sessions)
	}
	if result.Sessions[0].Error == nil {
		t.Fatal("a failed session must carry the error that explains it")
	}
	return result.Sessions[0].Error
}
