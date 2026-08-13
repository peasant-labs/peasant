package ftue

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"gopkg.in/yaml.v3"
)

// This file banks the selection-config equivalence oracle for the kickstart
// rebuild. Each scenario names SEMANTIC wizard inputs (which projects, branches,
// and sessions the user picks, at which scope, plus the auto-ingest answer) and
// the SelectionConfig the current onboarding wizard persists for them. The
// captured goldens are produced by driving the REAL persistence path — the
// wizard's exported SaveAnswers, through the same atomic config authority the
// mounted wizard uses, then re-read through config.Parse — so a golden is what a
// user's config.yaml actually holds, not a hand-authored guess, and it round-trips
// through the config types so it cannot rot silently.
//
// One scenario is a deliberate DIVERGENCE rather than a captured golden:
// selecting everything now persists the exact resolver-produced clone paths.

//go:embed testdata/equivalence/legacy_goldens.yaml
var legacyGoldenFixtureBytes []byte

//go:embed testdata/equivalence/ratified_divergence.yaml
var ratifiedDivergenceFixtureBytes []byte

//go:embed testdata/equivalence/reject-unknown-oracle.yaml
var rejectUnknownOracleBytes []byte

//go:embed testdata/equivalence/reject-divergence-missing-target.yaml
var rejectDivergenceMissingTargetBytes []byte

//go:embed testdata/equivalence/reject-legacy-carries-target.yaml
var rejectLegacyCarriesTargetBytes []byte

// legacyGoldenFloor and ratifiedDivergenceFloor are row counts the committed
// corpora must not fall below. Each floor EQUALS the current count rather than a
// hand-picked minimum: any slack between the two is a scenario that can be deleted
// in silence. The floor does not move with the corpus, so a row dropping is caught
// even when expectedScenarioCount is decremented in the same edit.
const (
	legacyGoldenFloor       = 5
	ratifiedDivergenceFloor = 1
)

// equivalenceOracle records how a scenario's captured selection should be read.
type equivalenceOracle string

const (
	// oracleLegacyCaptured means the golden is what the current wizard actually
	// persists for the inputs — the equivalence target the rebuild must match.
	oracleLegacyCaptured equivalenceOracle = "legacy-captured"
	// oracleRatifiedDivergence means the current wizard's captured output (golden)
	// is a documented DIVERGENCE from what the rebuilt flow must emit
	// (ratifiedExpected). Used only for the exact-current-list transition.
	oracleRatifiedDivergence equivalenceOracle = "ratified-divergence"
)

func (o equivalenceOracle) valid() bool {
	return o == oracleLegacyCaptured || o == oracleRatifiedDivergence
}

type selectionEquivalenceDocument struct {
	ExpectedScenarioCount int                            `yaml:"expectedScenarioCount"`
	Scenarios             []selectionEquivalenceScenario `yaml:"scenarios"`
}

type selectionEquivalenceScenario struct {
	Name                  string            `yaml:"name"`
	Doc                   string            `yaml:"doc"`
	Oracle                equivalenceOracle `yaml:"oracle"`
	WantImport            bool              `yaml:"wantImport"`
	AutoIngestNewBranches bool              `yaml:"autoIngestNewBranches"`
	Providers             []providerInput   `yaml:"providers"`
	Scopes                []scopeInput      `yaml:"scopes"`
	// Golden is the SelectionConfig the current wizard actually persists for the
	// inputs. For legacy-captured rows it is the equivalence oracle; for a
	// ratified-divergence row it records what the wizard writes TODAY (mode:selected,
	// enumerated) so the divergence is concrete and testable rather than asserted.
	Golden goldenSelection `yaml:"golden"`
	// RatifiedExpected is set ONLY on a ratified-divergence row: the SelectionConfig
	// the rebuilt kickstart MUST emit for these inputs, including physical clone
	// paths. The current wizard has no code path that produces those identities.
	RatifiedExpected *goldenSelection `yaml:"ratifiedExpected,omitempty"`
}

type providerInput struct {
	Harness   string `yaml:"harness"`
	ImportAll bool   `yaml:"importAll"`
}

type scopeInput struct {
	Level    string           `yaml:"level"`
	Sessions []SessionListing `yaml:"sessions"`
}

// goldenSelection mirrors config.SelectionConfig in a fixture-authorable shape.
type goldenSelection struct {
	Mode                  string                   `yaml:"mode"`
	AutoIngestNewBranches bool                     `yaml:"autoIngestNewBranches"`
	Harnesses             map[string]goldenHarness `yaml:"harnesses,omitempty"`
}

type goldenHarness struct {
	Projects []goldenProject `yaml:"projects,omitempty"`
	Sessions []string        `yaml:"sessions,omitempty"`
}

type goldenProject struct {
	GitRemote  string   `yaml:"gitRemote,omitempty"`
	Name       string   `yaml:"name,omitempty"`
	ClonePaths []string `yaml:"clonePaths,omitempty"`
	Branches   []string `yaml:"branches,omitempty"`
}

func (g goldenSelection) toConfig() config.SelectionConfig {
	var harnesses map[string]config.SelectionHarnessConfig
	if len(g.Harnesses) > 0 {
		harnesses = make(map[string]config.SelectionHarnessConfig, len(g.Harnesses))
		for name, h := range g.Harnesses {
			var projects []config.ProjectSelection
			for _, p := range h.Projects {
				projects = append(projects, config.ProjectSelection{
					GitRemote:  p.GitRemote,
					Name:       p.Name,
					ClonePaths: p.ClonePaths,
					Branches:   p.Branches,
				})
			}
			harnesses[name] = config.SelectionHarnessConfig{
				Projects: projects,
				Sessions: h.Sessions,
			}
		}
	}
	return config.SelectionConfig{
		Mode:                  config.SelectionMode(g.Mode),
		AutoIngestNewBranches: g.AutoIngestNewBranches,
		Harnesses:             harnesses,
	}
}

// scopeLevelFromName maps the fixture's readable scope word to the wizard's
// internal projectScopeLevel. Keeping the mapping here lets the fixtures stay
// self-describing while the unexported enum stays the single source of truth.
func scopeLevelFromName(name string) (projectScopeLevel, error) {
	switch name {
	case "project":
		return projectScopeProject, nil
	case "branch":
		return projectScopeBranch, nil
	case "session":
		return projectScopeSession, nil
	}
	return 0, fmt.Errorf("unknown scope level %q (valid: project, branch, session)", name)
}

func decodeSelectionEquivalence(data []byte) (selectionEquivalenceDocument, error) {
	var doc selectionEquivalenceDocument
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&doc); err != nil {
		return doc, equivalenceFixtureError(
			"typed YAML fields must match the scenario schema",
			"loader=first-document decode",
			fmt.Sprintf("fix=remove unknown fields and match the typed schema: %v", err))
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found another YAML document")
		}
		return doc, equivalenceFixtureError(
			"exactly one YAML document is allowed; scenarios below a second one prove nothing",
			"loader=end-of-document check",
			fmt.Sprintf("fix=remove the second document so the next decode returns EOF: %v", err))
	}
	if len(doc.Scenarios) == 0 || doc.ExpectedScenarioCount != len(doc.Scenarios) {
		return doc, equivalenceFixtureError(
			fmt.Sprintf("declared and actual scenario counts must match and be non-zero, got expectedScenarioCount=%d scenarios=%d",
				doc.ExpectedScenarioCount, len(doc.Scenarios)),
			"loader=scenario-count validation",
			"fix=set expectedScenarioCount to the number of scenarios present")
	}
	seen := map[string]bool{}
	for index, scenario := range doc.Scenarios {
		where := fmt.Sprintf("loader=scenario index %d", index)
		if strings.TrimSpace(scenario.Name) == "" || seen[scenario.Name] {
			return doc, equivalenceFixtureError(
				fmt.Sprintf("scenario name %q is missing or duplicated", scenario.Name), where,
				"fix=give every scenario a unique, behaviour-naming name")
		}
		seen[scenario.Name] = true
		if !scenario.Oracle.valid() {
			return doc, equivalenceFixtureError(
				fmt.Sprintf("scenario %q names oracle %q, which the loader does not define", scenario.Name, scenario.Oracle), where,
				fmt.Sprintf("fix=use %q or %q", oracleLegacyCaptured, oracleRatifiedDivergence))
		}
		if len(scenario.Providers) == 0 {
			return doc, equivalenceFixtureError(
				fmt.Sprintf("scenario %q selects no providers", scenario.Name), where,
				"fix=name at least one provider; the wizard cannot persist a selection with no enabled harness")
		}
		for _, scope := range scenario.Scopes {
			if _, err := scopeLevelFromName(scope.Level); err != nil {
				return doc, equivalenceFixtureError(
					fmt.Sprintf("scenario %q: %v", scenario.Name, err), where,
					"fix=use one of project, branch, session for every scope level")
			}
		}
		if !config.SelectionMode(scenario.Golden.Mode).IsValid() {
			return doc, equivalenceFixtureError(
				fmt.Sprintf("scenario %q golden names mode %q, which the config engine does not define", scenario.Name, scenario.Golden.Mode), where,
				"fix=use a mode from the SelectionMode enum (all, selected)")
		}
		// A ratified-divergence row MUST carry the forward target, and a captured
		// golden MUST NOT — otherwise the divergence is invisible in one direction
		// and a phantom expectation leaks into the equivalence oracle in the other.
		switch scenario.Oracle {
		case oracleRatifiedDivergence:
			if scenario.RatifiedExpected == nil {
				return doc, equivalenceFixtureError(
					fmt.Sprintf("scenario %q is a ratified-divergence row but sets no ratifiedExpected", scenario.Name), where,
					"fix=add the ratifiedExpected selection the rebuilt flow must emit; a divergence with no target asserts nothing")
			}
			if !config.SelectionMode(scenario.RatifiedExpected.Mode).IsValid() {
				return doc, equivalenceFixtureError(
					fmt.Sprintf("scenario %q ratifiedExpected names mode %q, which the config engine does not define", scenario.Name, scenario.RatifiedExpected.Mode), where,
					"fix=use a mode from the SelectionMode enum (all, selected)")
			}
		case oracleLegacyCaptured:
			if scenario.RatifiedExpected != nil {
				return doc, equivalenceFixtureError(
					fmt.Sprintf("scenario %q is a legacy-captured golden but sets ratifiedExpected", scenario.Name), where,
					"fix=drop ratifiedExpected from captured goldens; a captured golden is the equivalence target itself, not a divergence")
			}
		}
	}
	return doc, nil
}

func equivalenceFixtureError(what, where, fix string) error {
	return fmt.Errorf(
		"selection equivalence fixture rule failed: %s; a malformed or incomplete corpus invalidates the only banked evidence "+
			"of what the current onboarding wizard persists, which the kickstart rebuild is measured against; where=%s; "+
			"when=test fixture loading; impact=the rebuild could silently change a user's ingest scope; %s",
		what, where, fix)
}

func loadSelectionEquivalence(t *testing.T, data []byte) selectionEquivalenceDocument {
	t.Helper()
	doc, err := decodeSelectionEquivalence(data)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// driveLegacySelection runs a scenario's inputs through the REAL persistence path:
// the wizard's exported SaveAnswers writes a config.yaml via the same atomic
// authority the mounted wizard uses, and config.Parse reads it back. The returned
// SelectionConfig is exactly what the user's file would hold.
func driveLegacySelection(t *testing.T, s selectionEquivalenceScenario) config.SelectionConfig {
	t.Helper()
	answers := WizardAnswers{
		WantImport:            s.WantImport,
		AutoIngestNewBranches: s.AutoIngestNewBranches,
	}
	for _, p := range s.Providers {
		answers.ProviderSelections = append(answers.ProviderSelections, ProviderSelection{
			Harness:   p.Harness,
			ImportAll: p.ImportAll,
		})
	}
	for _, scope := range s.Scopes {
		level, err := scopeLevelFromName(scope.Level)
		if err != nil {
			t.Fatalf("scenario %q: %v", s.Name, err)
		}
		answers.ScopeSelections = append(answers.ScopeSelections, ProjectScopeSelection{
			Level:    level,
			Sessions: scope.Sessions,
		})
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := SaveAnswers(path, nil, nil, false, answers); err != nil {
		t.Fatalf("scenario %q: SaveAnswers persisted no config: %v", s.Name, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("scenario %q: read persisted config: %v", s.Name, err)
	}
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("scenario %q: re-parse persisted config: %v", s.Name, err)
	}
	return cfg.Selection
}

// TestSelectionEquivalence_LegacyGoldens is the equivalence oracle: for every
// non-select-everything scenario, the SelectionConfig the current wizard persists
// must match the captured golden. When the kickstart rebuild lands, its flow is
// driven over the same scenarios and asserted against the same goldens — a green
// run here plus a green run there is the field-equivalence proof.
func TestSelectionEquivalence_LegacyGoldens(t *testing.T) {
	doc := loadSelectionEquivalence(t, legacyGoldenFixtureBytes)
	if len(doc.Scenarios) < legacyGoldenFloor {
		t.Fatalf("the legacy golden corpus holds %d scenarios, below the floor of %d. Restore the scenario, or lower "+
			"the floor deliberately and say in the fixture header which behaviour stopped being covered.",
			len(doc.Scenarios), legacyGoldenFloor)
	}
	for _, scenario := range doc.Scenarios {
		scenario := scenario
		if scenario.Oracle != oracleLegacyCaptured {
			t.Fatalf("scenario %q is a %q row in the legacy golden file; keep divergence rows in ratified_divergence.yaml",
				scenario.Name, scenario.Oracle)
		}
		t.Run(scenario.Name, func(t *testing.T) {
			t.Parallel()
			got := driveLegacySelection(t, scenario)
			want := scenario.Golden.toConfig()
			if !reflect.DeepEqual(got, want) {
				t.Errorf("current wizard persisted selection\n got  = %#v\n want = %#v", got, want)
			}
		})
	}
}

// TestSelectionEquivalence_RatifiedDivergence pins the physical-identity
// divergence. The current wizard enumerates today's projects in mode:selected
// but cannot persist resolver-produced clone paths. The rebuilt target keeps the
// exact-current policy and adds those paths. Driving the current wizard keeps the
// pathless golden as a live non-vacuity control.
func TestSelectionEquivalence_RatifiedDivergence(t *testing.T) {
	doc := loadSelectionEquivalence(t, ratifiedDivergenceFixtureBytes)
	if len(doc.Scenarios) < ratifiedDivergenceFloor {
		t.Fatalf("the ratified divergence corpus holds %d scenarios, below the floor of %d.",
			len(doc.Scenarios), ratifiedDivergenceFloor)
	}
	for _, scenario := range doc.Scenarios {
		scenario := scenario
		if scenario.Oracle != oracleRatifiedDivergence {
			t.Fatalf("scenario %q is a %q row in the ratified divergence file; captured goldens belong in legacy_goldens.yaml",
				scenario.Name, scenario.Oracle)
		}
		t.Run(scenario.Name, func(t *testing.T) {
			t.Parallel()
			// The current wizard really does write the recorded golden for these inputs.
			got := driveLegacySelection(t, scenario)
			legacyActual := scenario.Golden.toConfig()
			if !reflect.DeepEqual(got, legacyActual) {
				t.Errorf("current wizard persisted selection\n got  = %#v\n want = %#v", got, legacyActual)
			}

			// The ratified target the rebuild must emit, and the divergence proof.
			target := scenario.RatifiedExpected.toConfig()
			if reflect.DeepEqual(got, target) {
				t.Fatalf("the ratified divergence is vacuous: the current wizard already emits the target mode:%s selection, "+
					"so there is nothing for the rebuild to change", target.Mode)
			}
			if target.Mode != config.SelectionModeSelected {
				t.Errorf("physical-identity target mode = %q, want %q (select all saves the exact current list)", target.Mode, config.SelectionModeSelected)
			}
			for harness, configured := range target.Harnesses {
				if len(configured.Projects) != 1 || len(configured.Projects[0].ClonePaths) != 1 {
					t.Errorf("physical-identity target for %q lacks one clone path: %#v", harness, configured)
				}
			}

			// The target is itself a well-formed config the rebuilt flow can persist:
			// it survives the same marshal + validate + parse round-trip as any real
			// config.yaml, so the oracle cannot pin a shape the config engine rejects.
			base := config.BaseConfig()
			base.Selection = target
			data, err := yaml.Marshal(base)
			if err != nil {
				t.Fatalf("marshal ratified target config: %v", err)
			}
			parsed, err := config.Parse(data)
			if err != nil {
				t.Fatalf("ratified target is not a persistable config: %v", err)
			}
			if !reflect.DeepEqual(parsed.Selection, target) {
				t.Errorf("ratified target did not round-trip through the config types\n got  = %#v\n want = %#v",
					parsed.Selection, target)
			}
		})
	}
}

// --- loader guards ----------------------------------------------------------
//
// These prove the corpus loader actually bites, so a malformed or mislabelled
// scenario cannot slip in and quietly weaken the oracle.

func TestDecodeSelectionEquivalence_RejectsUnknownOracle(t *testing.T) {
	t.Parallel()
	_, err := decodeSelectionEquivalence(rejectUnknownOracleBytes)
	if err == nil || !strings.Contains(err.Error(), "which the loader does not define") {
		t.Fatalf("error = %v, want rejection of a scenario naming an unknown oracle", err)
	}
}

func TestDecodeSelectionEquivalence_RejectsDivergenceWithNoTarget(t *testing.T) {
	t.Parallel()
	_, err := decodeSelectionEquivalence(rejectDivergenceMissingTargetBytes)
	if err == nil || !strings.Contains(err.Error(), "sets no ratifiedExpected") {
		t.Fatalf("error = %v, want rejection of a divergence row with no forward target", err)
	}
}

func TestDecodeSelectionEquivalence_RejectsLegacyGoldenCarryingATarget(t *testing.T) {
	t.Parallel()
	_, err := decodeSelectionEquivalence(rejectLegacyCarriesTargetBytes)
	if err == nil || !strings.Contains(err.Error(), "legacy-captured golden but sets ratifiedExpected") {
		t.Fatalf("error = %v, want rejection of a captured golden carrying a divergence target", err)
	}
}
