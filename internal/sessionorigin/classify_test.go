package sessionorigin_test

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/classification/cases.yaml
var classificationFixtureBytes []byte

// requiredClassificationCaseNames is the deletion guard. A row removed from the
// fixture takes a behaviour with it, so the loader refuses a fixture that no
// longer carries every named case. The list is names rather than a count,
// because a count churns on every addition and collides between parallel edits.
var requiredClassificationCaseNames = []string{
	"child_transcript_is_agent_driven",
	"parent_link_outranks_a_command_wrapper",
	"structured_identity_from_agent_name",
	"structured_identity_from_team_name_alone",
	"structured_identity_beside_plain_prose",
	"programmatic_entry_from_the_sdk_pair",
	"command_message_wrapper_is_a_person",
	"command_name_wrapper_is_a_person",
	"attributed_command_wrapper_is_still_a_person",
	"self_closing_command_wrapper_is_still_a_person",
	"command_wrapper_wins_over_bootstrap_text_in_its_body",
	"command_wrapper_behind_leading_whitespace_is_still_a_person",
	"teammate_message_opener_is_a_bootstrap",
	"skill_invocation_opener_with_no_wrapper_tag",
	"numbered_skill_invocation_opener",
	"use_skill_invocation_opener",
	"zero_evidence_resolves_unknown",
	"plain_prose_is_never_declared_a_person",
	"unlisted_tag_sharing_a_listed_prefix_decides_nothing",
	"unlisted_tag_sharing_the_bootstrap_prefix_decides_nothing",
	"a_programmatic_entrypoint_alone_decides_nothing",
	"a_programmatic_prompt_source_alone_decides_nothing",
}

type classificationFixture struct {
	Cases []classificationCase `yaml:"cases"`
}

type classificationCase struct {
	Name     string          `yaml:"name"`
	Arm      string          `yaml:"arm"`
	Evidence evidenceFixture `yaml:"evidence"`
	Origin   string          `yaml:"origin"`
	Signal   string          `yaml:"signal"`
}

type evidenceFixture struct {
	HasParent     bool   `yaml:"has_parent"`
	AgentName     string `yaml:"agent_name"`
	TeamName      string `yaml:"team_name"`
	Entrypoint    string `yaml:"entrypoint"`
	PromptSource  string `yaml:"prompt_source"`
	FirstUserText string `yaml:"first_user_text"`
}

func (e evidenceFixture) evidence() sessionorigin.Evidence {
	return sessionorigin.Evidence{
		HasParent:     e.HasParent,
		AgentName:     e.AgentName,
		TeamName:      e.TeamName,
		Entrypoint:    e.Entrypoint,
		PromptSource:  e.PromptSource,
		FirstUserText: e.FirstUserText,
	}
}

// LoadClassificationFixtures decodes the behaviour corpus and refuses any
// fixture that has lost a required case, repeated a name, or declared a value
// outside the production closed sets.
func LoadClassificationFixtures(data []byte) (classificationFixture, error) {
	var fixture classificationFixture
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		return classificationFixture{}, fmt.Errorf("decode classification fixture first document: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return classificationFixture{}, fmt.Errorf("classification fixture must contain exactly one YAML document: %v", err)
	}

	names := make(map[string]struct{}, len(fixture.Cases))
	arms := make(map[string]struct{}, len(sessionorigin.AllSignals))
	for _, tc := range fixture.Cases {
		if tc.Name == "" {
			return classificationFixture{}, errors.New("classification fixture holds a case with no name")
		}
		if _, duplicate := names[tc.Name]; duplicate {
			return classificationFixture{}, fmt.Errorf("classification fixture repeats case name %q", tc.Name)
		}
		names[tc.Name] = struct{}{}
		if _, err := sessionorigin.Parse(tc.Origin); err != nil {
			return classificationFixture{}, fmt.Errorf("classification fixture case %q declares origin %q: %w", tc.Name, tc.Origin, err)
		}
		if !sessionorigin.Signal(tc.Signal).Valid() {
			return classificationFixture{}, fmt.Errorf("classification fixture case %q declares signal %q, which is not a deciding signal", tc.Name, tc.Signal)
		}
		if tc.Arm != tc.Signal {
			return classificationFixture{}, fmt.Errorf("classification fixture case %q sits in arm %q but declares signal %q; the arm names the deciding signal", tc.Name, tc.Arm, tc.Signal)
		}
		arms[tc.Arm] = struct{}{}
	}
	for _, required := range requiredClassificationCaseNames {
		if _, ok := names[required]; !ok {
			return classificationFixture{}, fmt.Errorf("classification fixture is missing required case %q", required)
		}
	}
	// Membership against the production closed set, not a count: every deciding
	// signal the rule can return must be exercised by at least one arm.
	for _, signal := range sessionorigin.AllSignals {
		if _, ok := arms[signal.String()]; !ok {
			return classificationFixture{}, fmt.Errorf("classification fixture has no arm for deciding signal %q", signal)
		}
	}
	return fixture, nil
}

func TestClassify(t *testing.T) {
	fixture, err := LoadClassificationFixtures(classificationFixtureBytes)
	if err != nil {
		t.Fatalf("load classification fixture: %v", err)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			origin, signal := sessionorigin.Classify(tc.Evidence.evidence())
			if origin.String() != tc.Origin {
				t.Errorf("Classify origin = %q, want %q", origin, tc.Origin)
			}
			if signal.String() != tc.Signal {
				t.Errorf("Classify signal = %q, want %q", signal, tc.Signal)
			}
		})
	}
}

// TestClassifyNeverHidesACommandInvocation states the rule the whole ordering
// exists for. A command wrapper is something a person typed, so a case in that
// arm resolving anything but user is a defect rather than a tolerance.
func TestClassifyNeverHidesACommandInvocation(t *testing.T) {
	fixture, err := LoadClassificationFixtures(classificationFixtureBytes)
	if err != nil {
		t.Fatalf("load classification fixture: %v", err)
	}
	exercised := false
	for _, tc := range fixture.Cases {
		if tc.Arm != sessionorigin.SignalCommandInvocation.String() {
			continue
		}
		exercised = true
		origin, signal := sessionorigin.Classify(tc.Evidence.evidence())
		if origin != sessionorigin.User || signal != sessionorigin.SignalCommandInvocation {
			t.Errorf("case %q: a command-wrapped session classified %q/%q, want user/command-invocation", tc.Name, origin, signal)
		}
	}
	if !exercised {
		t.Fatal("no command-invocation case was exercised, so this guard proved nothing")
	}
}

// TestClassifyIsPure checks that the rule reads nothing but its argument: the
// same evidence classified twice gives the same answer, and classifying one case
// cannot change the answer to another.
func TestClassifyIsPure(t *testing.T) {
	fixture, err := LoadClassificationFixtures(classificationFixtureBytes)
	if err != nil {
		t.Fatalf("load classification fixture: %v", err)
	}
	first := make([]sessionorigin.Origin, len(fixture.Cases))
	for i, tc := range fixture.Cases {
		first[i], _ = sessionorigin.Classify(tc.Evidence.evidence())
	}
	for i := len(fixture.Cases) - 1; i >= 0; i-- {
		again, _ := sessionorigin.Classify(fixture.Cases[i].Evidence.evidence())
		if again != first[i] {
			t.Errorf("case %q classified %q then %q on the same evidence", fixture.Cases[i].Name, first[i], again)
		}
	}
}

func TestLoadClassificationFixturesRejectsADeletedCase(t *testing.T) {
	fixture, err := LoadClassificationFixtures(classificationFixtureBytes)
	if err != nil {
		t.Fatalf("load classification fixture: %v", err)
	}
	var trimmed classificationFixture
	trimmed.Cases = append(trimmed.Cases, fixture.Cases[1:]...)
	encoded, err := yaml.Marshal(trimmed)
	if err != nil {
		t.Fatalf("marshal trimmed fixture: %v", err)
	}
	if _, err := LoadClassificationFixtures(encoded); err == nil {
		t.Fatal("loader accepted a fixture with a required case removed")
	} else if !strings.Contains(err.Error(), "missing required case") {
		t.Fatalf("loader refused for the wrong reason: %v", err)
	}
}

func TestLoadClassificationFixturesRejectsAnUnknownField(t *testing.T) {
	if _, err := LoadClassificationFixtures([]byte("cases:\n  - name: x\n    unexpected: y\n")); err == nil {
		t.Fatal("loader accepted a fixture carrying a field the case struct does not declare")
	}
}
