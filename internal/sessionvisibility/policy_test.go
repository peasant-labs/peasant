package sessionvisibility_test

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
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
	"exact_branch_exclusion",
	"branch_exclusion_does_not_cross_clone_boundary",
	"project_admitted_exact_session_exclusion",
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
	// Origin scope. Every withholding arm is paired with a control that differs
	// only in origin, so an arm cannot pass because the row would have been
	// absent anyway. The two scopes are exercised alone and together, so a later
	// change cannot let one absorb the other.
	"origin_scope_alone_hides_an_agent_root",
	"origin_scope_alone_admits_a_user_root",
	"origin_scope_alone_admits_an_unknown_root",
	"origin_scope_alone_hides_an_agent_subagent_row",
	"a_subagent_row_a_person_drove_stays_in_discovery",
	"selection_scope_alone_hides_a_user_origin_row",
	"both_scopes_together_hide_an_agent_row_in_a_selected_project",
	"both_scopes_together_admit_a_user_row_in_a_selected_project",
	"an_unusable_origin_withholds_the_discovery_list",
}

type policyFixture struct {
	Cases []policyCase `yaml:"cases"`
}

type policyCase struct {
	Name      string                                   `yaml:"name"`
	Mode      config.SelectionMode                     `yaml:"mode"`
	Harnesses map[string]config.SelectionHarnessConfig `yaml:"harnesses"`
	Candidate candidateFixture                         `yaml:"candidate"`
	// Result is what selection scope alone decides, through Policy.Visible.
	Result        string `yaml:"result"`
	ErrorContains string `yaml:"error_contains"`
	// DiscoveryResult is what the two scopes together decide, through
	// Policy.VisibleForDiscovery. Empty on the arms that predate origin scope,
	// which state selection scope only. An arm that names an origin must state
	// it, so an origin arm can never be silently non-asserting.
	DiscoveryResult        string `yaml:"discovery_result"`
	DiscoveryErrorContains string `yaml:"discovery_error_contains"`
}

type candidateFixture struct {
	Harness         string `yaml:"harness"`
	SessionID       string `yaml:"session_id"`
	ProjectName     string `yaml:"project_name"`
	GitRemote       string `yaml:"git_remote"`
	GitBranch       string `yaml:"git_branch"`
	ClonePath       string `yaml:"clone_path"`
	Origin          string `yaml:"origin"`
	ParentSessionID string `yaml:"parent_session_id"`
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
		if tc.Candidate.ClonePath != "" && (!filepath.IsAbs(tc.Candidate.ClonePath) || filepath.Clean(tc.Candidate.ClonePath) != tc.Candidate.ClonePath) {
			return policyFixture{}, fmt.Errorf("policy fixture case %q has non-exact clone_path %q", tc.Name, tc.Candidate.ClonePath)
		}
		if tc.DiscoveryResult != "" && tc.DiscoveryResult != "visible" && tc.DiscoveryResult != "hidden" && tc.DiscoveryResult != "error" {
			return policyFixture{}, fmt.Errorf("policy fixture case %q has discovery_result %q: it must be exactly visible, hidden, or error when set", tc.Name, tc.DiscoveryResult)
		}
		if (tc.Candidate.Origin != "") != (tc.DiscoveryResult != "") {
			return policyFixture{}, fmt.Errorf("policy fixture case %q must state discovery_result if and only if the candidate names an origin", tc.Name)
		}
		if (tc.DiscoveryResult == "error") != (tc.DiscoveryErrorContains != "") {
			return policyFixture{}, fmt.Errorf("policy fixture case %q must set discovery_error_contains if and only if discovery_result is error", tc.Name)
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
			candidate := sessionvisibility.Candidate{
				SessionID:       ingest.SessionID(tc.Candidate.SessionID),
				Harness:         defaults.Harness(tc.Candidate.Harness),
				GitRemote:       tc.Candidate.GitRemote,
				ProjectName:     tc.Candidate.ProjectName,
				ClonePath:       ingest.ClonePath(tc.Candidate.ClonePath),
				GitBranch:       tc.Candidate.GitBranch,
				Origin:          sessionorigin.Origin(tc.Candidate.Origin),
				ParentSessionID: ingest.SessionID(tc.Candidate.ParentSessionID),
			}
			assertDiscoveryScope(t, policy, candidate, tc)
			visible, err := policy.Visible(candidate)
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

// assertDiscoveryScope checks the conjunction of the two scopes for the arms
// that state one. Every arm still checks selection scope alone through
// Policy.Visible, so a case whose two expectations DIFFER is what proves origin
// scope acts in discovery and nowhere else.
func assertDiscoveryScope(t *testing.T, policy sessionvisibility.Policy, candidate sessionvisibility.Candidate, tc policyCase) {
	t.Helper()
	if tc.DiscoveryResult == "" {
		return
	}
	visible, err := policy.VisibleForDiscovery(candidate)
	if tc.DiscoveryResult == "error" {
		if err == nil || !strings.Contains(err.Error(), tc.DiscoveryErrorContains) {
			t.Fatalf("VisibleForDiscovery error = %v, want containing %q", err, tc.DiscoveryErrorContains)
		}
		if visible {
			t.Fatalf("VisibleForDiscovery = true alongside an error; it must withhold the row")
		}
		return
	}
	if err != nil {
		t.Fatalf("VisibleForDiscovery: %v", err)
	}
	if want := tc.DiscoveryResult == "visible"; visible != want {
		t.Fatalf("VisibleForDiscovery = %v, want %v", visible, want)
	}
}

func TestZeroPolicyDiscoveryFailsClosed(t *testing.T) {
	visible, err := (sessionvisibility.Policy{}).VisibleForDiscovery(sessionvisibility.Candidate{Origin: sessionorigin.User})
	if err == nil || visible {
		t.Fatalf("zero policy VisibleForDiscovery = %v, %v; want false and actionable error", visible, err)
	}
}
