package projectlabel_test

import (
	_ "embed"
	"testing"

	"github.com/peasant-labs/peasant/internal/projectlabel"
	"github.com/peasant-labs/schema/testcase"
	"github.com/peasant-labs/schema/testcase/assert"
)

//go:embed testdata/label_cases.yaml
var labelCasesYAML []byte

type labelInput struct {
	Remote   string `yaml:"remote"`
	Fallback string `yaml:"fallback"`
}

type labelExpected struct {
	Label string `yaml:"label"`
}

// expectedCaseCount is a floor (assert.RequireMin), not an exact count, so
// the corpus can grow without this guard needing an update; it still catches
// the corpus being silently gutted or never loaded.
const expectedCaseCount = 12

func loadLabelCorpus(t *testing.T) testcase.Corpus[labelInput, labelExpected] {
	t.Helper()
	corpus, err := testcase.LoadCorpus[labelInput, labelExpected](labelCasesYAML)
	if err != nil {
		t.Fatalf("load projectlabel fixture: %v", err)
	}
	assert.RequireMin(t, corpus, expectedCaseCount)
	assert.RequireValid(t, corpus)
	return corpus
}

// TestLabel_FixtureCorpus drives projectlabel.Label over every fixture case:
// github/gitlab bare canonical_remote forms, an unrecognized host (keeps its
// full hostname rather than failing closed), defensive HTTPS/SSH remote
// forms peasant does not normally store but should not choke on, and the
// empty/malformed-remote fallback path. A missing remote must fall back to the
// caller's path/hash display value, never fail or return an empty label.
func TestLabel_FixtureCorpus(t *testing.T) {
	corpus := loadLabelCorpus(t)
	for _, fixtureCase := range corpus.Cases {
		t.Run(fixtureCase.Name, func(t *testing.T) {
			got := projectlabel.Label(fixtureCase.Input.Remote, fixtureCase.Input.Fallback)
			if got != fixtureCase.Expected.Label {
				t.Errorf("Label(%q, %q) = %q, want %q", fixtureCase.Input.Remote, fixtureCase.Input.Fallback, got, fixtureCase.Expected.Label)
			}
		})
	}
}

// TestFromRemote_OkFlag directly exercises the ok return value that Label
// hides: FromRemote must report ok=false (not just an empty string) whenever
// the fixture corpus's fallback path is expected to win, so Label's fallback
// behavior is provably driven by FromRemote's failure signal rather than an
// incidental empty-string coincidence.
func TestFromRemote_OkFlag(t *testing.T) {
	corpus := loadLabelCorpus(t)
	for _, fixtureCase := range corpus.Cases {
		t.Run(fixtureCase.Name, func(t *testing.T) {
			label, ok := projectlabel.FromRemote(fixtureCase.Input.Remote)
			fellBack := fixtureCase.Expected.Label == fixtureCase.Input.Fallback && fixtureCase.Input.Remote != fixtureCase.Expected.Label
			if fellBack && ok {
				t.Errorf("FromRemote(%q) = (%q, true), want ok=false so Label falls back to %q", fixtureCase.Input.Remote, label, fixtureCase.Input.Fallback)
			}
			if !fellBack && !ok {
				t.Errorf("FromRemote(%q) = (%q, false), want ok=true producing %q", fixtureCase.Input.Remote, label, fixtureCase.Expected.Label)
			}
		})
	}
}

// TestFromRemote_HostPrefixTableIsNotExhaustive proves an unrecognized host
// is not silently dropped: it keeps its full hostname rather than the label
// collapsing to a bare "owner/repo" (which would look identical to a
// known-host label with the host stripped, masking a real behavior gap).
func TestFromRemote_HostPrefixTableIsNotExhaustive(t *testing.T) {
	label, ok := projectlabel.FromRemote("git.example.com/acme/widgets")
	if !ok {
		t.Fatalf("FromRemote unrecognized host: ok = false, want true")
	}
	if label != "git.example.com:acme/widgets" {
		t.Fatalf("FromRemote unrecognized host = %q, want the full hostname preserved as the prefix", label)
	}
}
