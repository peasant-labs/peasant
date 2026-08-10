package sessionvisibility_test

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/policy.yaml
var policyFixtureBytes []byte

var requiredPolicyBehaviorNames = []string{
	"all_mode",
	"selected_empty",
	"unrestricted_named_harness",
	"explicit_session_does_not_widen_siblings",
	"explicit_session_positive",
	"git_remote_identity_fallback",
	"project_name_identity_fallback",
	"absent_branch_is_admitted_for_selected_project",
	"mixed_harness_does_not_cross_select",
	"selected_branch",
	"excluded_branch",
	"conflicting_branch_rules",
	// Real-data-shaped regression coverage (SSH/SCP/ssh:// config
	// remotes vs the normalized bare stored form; a short config name vs a
	// path-shaped stored "project name"), plus a negative control and an
	// explicit-session-still-works check alongside real-form project rules.
	"git_remote_scp_config_matches_normalized_stored_form",
	"git_remote_https_config_matches_normalized_stored_form",
	"git_remote_ssh_url_config_matches_normalized_stored_form",
	"project_name_config_matches_path_shaped_stored_name",
	"unselected_project_with_real_forms_stays_hidden",
	"explicit_session_still_works_alongside_real_form_project_rules",
}

type policyFixture struct {
	Cases []policyCase `yaml:"cases"`
}

type policyCase struct {
	Name          string                                   `yaml:"name"`
	Mode          config.SelectionMode                     `yaml:"mode"`
	Harnesses     map[string]config.SelectionHarnessConfig `yaml:"harnesses"`
	Candidate     candidateFixture                         `yaml:"candidate"`
	Result        string                                   `yaml:"result"`
	ErrorContains string                                   `yaml:"error_contains"`
}

type candidateFixture struct {
	Harness     string `yaml:"harness"`
	SessionID   string `yaml:"session_id"`
	ProjectName string `yaml:"project_name"`
	GitRemote   string `yaml:"git_remote"`
	GitBranch   string `yaml:"git_branch"`
}

func loadPolicyFixture(data []byte) (policyFixture, error) {
	var fixture policyFixture
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		return policyFixture{}, fmt.Errorf("decode policy fixture first document: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return policyFixture{}, fmt.Errorf("policy fixture must contain exactly one YAML document: %v", err)
	}
	if len(fixture.Cases) != len(requiredPolicyBehaviorNames) {
		return policyFixture{}, fmt.Errorf("policy fixture has %d cases, want exactly %d", len(fixture.Cases), len(requiredPolicyBehaviorNames))
	}
	names := make(map[string]struct{}, len(fixture.Cases))
	for _, tc := range fixture.Cases {
		if tc.Name == "" || (tc.Result != "visible" && tc.Result != "hidden" && tc.Result != "error") {
			return policyFixture{}, fmt.Errorf("policy fixture case %q is incomplete: result must be exactly visible, hidden, or error", tc.Name)
		}
		if _, duplicate := names[tc.Name]; duplicate {
			return policyFixture{}, fmt.Errorf("policy fixture repeats case name %q", tc.Name)
		}
		names[tc.Name] = struct{}{}
		if (tc.Result == "error") != (tc.ErrorContains != "") {
			return policyFixture{}, fmt.Errorf("policy fixture case %q must set error_contains if and only if result is error", tc.Name)
		}
	}
	for _, required := range requiredPolicyBehaviorNames {
		if _, ok := names[required]; !ok {
			return policyFixture{}, fmt.Errorf("policy fixture is missing required behavior family %q", required)
		}
	}
	return fixture, nil
}

func TestPolicyFixture(t *testing.T) {
	fixture, err := loadPolicyFixture(policyFixtureBytes)
	if err != nil {
		t.Fatalf("load policy fixture: %v", err)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			policy, err := sessionvisibility.New(config.SelectionConfig{Mode: tc.Mode, Harnesses: tc.Harnesses})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			visible, err := policy.Visible(sessionvisibility.Candidate{
				SessionID:   ingest.SessionID(tc.Candidate.SessionID),
				Harness:     defaults.Harness(tc.Candidate.Harness),
				GitRemote:   tc.Candidate.GitRemote,
				ProjectName: tc.Candidate.ProjectName,
				GitBranch:   tc.Candidate.GitBranch,
			})
			if tc.Result == "error" {
				if err == nil || !strings.Contains(err.Error(), tc.ErrorContains) {
					t.Fatalf("Visible error = %v, want containing %q", err, tc.ErrorContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("Visible: %v", err)
			}
			wantVisible := tc.Result == "visible"
			if visible != wantVisible {
				t.Fatalf("Visible = %v, want %v", visible, wantVisible)
			}
		})
	}
}

func TestPolicyFixtureLoaderRejectsStructuralDrift(t *testing.T) {
	unknownField := bytes.Replace(policyFixtureBytes, []byte("result: visible"), []byte("result: visible\n    unexpected: true"), 1)
	if _, err := loadPolicyFixture(unknownField); err == nil || !strings.Contains(err.Error(), "field unexpected not found") {
		t.Fatalf("unknown-field mutation error = %v, want strict known-field rejection", err)
	}
	duplicateName := bytes.Replace(policyFixtureBytes, []byte("name: selected_empty"), []byte("name: all_mode"), 1)
	if _, err := loadPolicyFixture(duplicateName); err == nil || !strings.Contains(err.Error(), "repeats case name") {
		t.Fatalf("duplicate-name mutation error = %v, want duplicate rejection", err)
	}
	trailingDocument := append(append([]byte{}, policyFixtureBytes...), []byte("\n---\nextra: true\n")...)
	if _, err := loadPolicyFixture(trailingDocument); err == nil || !strings.Contains(err.Error(), "exactly one YAML document") {
		t.Fatalf("trailing-document mutation error = %v, want single-document rejection", err)
	}
	for _, required := range requiredPolicyBehaviorNames {
		t.Run("renamed_"+required, func(t *testing.T) {
			mutated := bytes.Replace(
				policyFixtureBytes,
				[]byte("name: "+required),
				[]byte("name: removed_"+required),
				1,
			)
			if _, err := loadPolicyFixture(mutated); err == nil || !strings.Contains(err.Error(), "missing required behavior family") {
				t.Fatalf("required-family mutation error = %v, want missing-family rejection", err)
			}
		})
	}
}

func TestZeroPolicyFailsClosed(t *testing.T) {
	visible, err := (sessionvisibility.Policy{}).Visible(sessionvisibility.Candidate{})
	if err == nil || visible {
		t.Fatalf("zero policy Visible = %v, %v; want false and actionable error", visible, err)
	}
}

func TestZeroPolicyProjectionInputsFailClosed(t *testing.T) {
	mode, matcher, err := (sessionvisibility.Policy{}).ProjectionInputs()
	if err == nil || mode != "" || matcher != nil {
		t.Fatalf("zero policy ProjectionInputs = %q, %v, %v; want empty mode, nil matcher, and actionable error", mode, matcher, err)
	}
}
