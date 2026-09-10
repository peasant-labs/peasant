package ingest

import (
	_ "embed"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/strict_refusal_steady_state.yaml
var strictRefusalSteadyStateCorpus []byte

type strictRefusalSettledFixtures struct {
	Required []string `yaml:"settled_required_names"`
	Cases    []struct {
		Name          string `yaml:"name"`
		FailureCode   string `yaml:"failure_code"`
		StoredVersion int    `yaml:"stored_indexer_version"`
		TargetVersion int    `yaml:"target_indexer_version"`
		InputProof    bool   `yaml:"input_proof"`
		Settled       bool   `yaml:"settled"`
	} `yaml:"settled_cases"`
}

// TestStrictRefusalSettledPredicate holds each term of the rule that gives a
// strict refusal a steady state.
//
// The harvest corpus proves the selector and the recovery sweep ask this
// question; this proves the answer. A whole harvest cannot cheaply arrange
// every stored state a real installation holds - a legacy preview-only capture
// written with an input proof is the one that matters most, because without
// the failure-code term it would look settled and the user would be left with
// a preview forever.
func TestStrictRefusalSettledPredicate(t *testing.T) {
	t.Parallel()
	var fixtures strictRefusalSettledFixtures
	if err := yaml.Unmarshal(strictRefusalSteadyStateCorpus, &fixtures); err != nil {
		t.Fatal(err)
	}
	present := make(map[string]bool, len(fixtures.Cases))
	for _, fixture := range fixtures.Cases {
		if present[fixture.Name] {
			t.Fatalf("duplicate fixture %s", fixture.Name)
		}
		present[fixture.Name] = true
	}
	requireFixtureNames(t, "strict refusal settled predicate", fixtures.Required, present)
	for _, fixture := range fixtures.Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			t.Parallel()
			code, err := NewContentCaptureFailureCode(fixture.FailureCode)
			if err != nil {
				t.Fatal(err)
			}
			state := &SessionIndexState{
				ContentStatus:      ContentCaptureIncomplete,
				ContentFailureCode: code,
				IndexerVersion:     fixture.StoredVersion,
			}
			if fixture.InputProof {
				proof := "an input this parser already consumed"
				state.IndexedInputHash = &proof
			}
			got := strictRefusalIsSettled(state, HarvesterVersions{IndexerVersion: fixture.TargetVersion})
			if got != fixture.Settled {
				t.Fatalf("a capture refused=%q stored at producer %d, target %d, input proof=%v is settled=%v, want %v",
					fixture.FailureCode, fixture.StoredVersion, fixture.TargetVersion, fixture.InputProof, got, fixture.Settled)
			}
		})
	}
}
