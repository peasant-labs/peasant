package ingest

import (
	_ "embed"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/content_capture_failure_code.yaml
var contentCaptureFailureCodeCorpus []byte

type contentCaptureFailureCodeFixtures struct {
	RequiredMembers []string `yaml:"required_members"`
	Members         []struct {
		Value     string `yaml:"value"`
		Permanent bool   `yaml:"permanent"`
	} `yaml:"members"`
	Refused []struct {
		Name  string `yaml:"name"`
		Value string `yaml:"value"`
	} `yaml:"refused"`
}

// TestContentCaptureFailureCodeClosedSet holds the set of reasons a stored
// capture is not complete, and which of them this build can never clear.
//
// The set is a contract because the selector acts on it: a code read as "no
// failure recorded" when it is really a refusal puts the session back into
// pending work on every harvest, which is the condition this corpus exists to
// prevent. Membership is asserted exactly, in both directions.
func TestContentCaptureFailureCodeClosedSet(t *testing.T) {
	t.Parallel()
	var fixtures contentCaptureFailureCodeFixtures
	if err := yaml.Unmarshal(contentCaptureFailureCodeCorpus, &fixtures); err != nil {
		t.Fatal(err)
	}
	declared := make(map[string]bool, len(fixtures.Members))
	for _, member := range fixtures.Members {
		if declared[member.Value] {
			t.Fatalf("duplicate member %q", member.Value)
		}
		declared[member.Value] = true
		code, err := NewContentCaptureFailureCode(member.Value)
		if err != nil {
			t.Fatalf("declared member %q is not accepted: %v", member.Value, err)
		}
		if string(code) != member.Value {
			t.Fatalf("member %q round-tripped as %q", member.Value, code)
		}
		if got := permanentCaptureRefusal(code); got != member.Permanent {
			t.Fatalf("member %q is permanent=%v, want %v: a permanent refusal is a steady state and a temporary one owes another attempt", member.Value, got, member.Permanent)
		}
	}
	if len(fixtures.RequiredMembers) == 0 {
		t.Fatal("the corpus must name the members it requires")
	}
	for _, name := range fixtures.RequiredMembers {
		if !declared[name] {
			t.Fatalf("required member %q is missing from the corpus", name)
		}
	}
	if len(declared) != len(fixtures.RequiredMembers) {
		t.Fatalf("the corpus declares %d members and requires %d; add the new code to the manifest", len(declared), len(fixtures.RequiredMembers))
	}
	if len(fixtures.Refused) == 0 {
		t.Fatal("the corpus must state values that are refused; without one, the closed set is only half asserted")
	}
	for _, refused := range fixtures.Refused {
		t.Run(refused.Name, func(t *testing.T) {
			t.Parallel()
			code, err := NewContentCaptureFailureCode(refused.Value)
			if err == nil {
				t.Fatalf("%q was accepted as %q; an unnameable code must be refused, never read as the absent one", refused.Value, code)
			}
			if code != ContentCaptureNoFailure {
				t.Fatalf("a refused value returned %q rather than the zero code", code)
			}
		})
	}
}
