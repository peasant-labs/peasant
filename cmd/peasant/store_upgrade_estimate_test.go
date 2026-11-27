package main

import (
	_ "embed"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/store_upgrade_estimate.yaml
var storeUpgradeEstimateYAML []byte

type storeUpgradeEstimateFixture struct {
	RequiredNames []string `yaml:"requiredNames"`
	Cases         []struct {
		Name         string `yaml:"name"`
		LowSeconds   int    `yaml:"lowSeconds"`
		HighSeconds  int    `yaml:"highSeconds"`
		Extrapolated bool   `yaml:"extrapolated"`
		Want         string `yaml:"want"`
	} `yaml:"cases"`
	PlanCases []struct {
		Name  string `yaml:"name"`
		Steps []struct {
			Version     int    `yaml:"version"`
			Title       string `yaml:"title"`
			Cost        string `yaml:"cost"`
			LowSeconds  int    `yaml:"lowSeconds"`
			HighSeconds int    `yaml:"highSeconds"`
		} `yaml:"steps"`
		WantLines         []string `yaml:"wantLines"`
		WantJSONEstimates []struct {
			Version     int   `yaml:"version"`
			LowSeconds  int64 `yaml:"lowSeconds"`
			HighSeconds int64 `yaml:"highSeconds"`
		} `yaml:"wantJSONEstimates"`
	} `yaml:"planCases"`
}

func loadStoreUpgradeEstimateFixture(t *testing.T) storeUpgradeEstimateFixture {
	t.Helper()
	var fixture storeUpgradeEstimateFixture
	if err := yaml.Unmarshal(storeUpgradeEstimateYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	present := map[string]bool{}
	for _, c := range fixture.Cases {
		present[c.Name] = true
	}
	for _, c := range fixture.PlanCases {
		present[c.Name] = true
	}
	if err := testutil.RequireFixtureNames("store upgrade estimate", "case", fixture.RequiredNames, present); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func seconds(n int) time.Duration { return time.Duration(n) * time.Second }

func TestStoreUpgradeEstimateLabel(t *testing.T) {
	t.Parallel()
	for _, c := range loadStoreUpgradeEstimateFixture(t).Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			got := storeUpgradeEstimateLabel(store.UpgradeEstimate{Low: seconds(c.LowSeconds), High: seconds(c.HighSeconds), Extrapolated: c.Extrapolated})
			if got != c.Want {
				t.Errorf("estimate label = %q, want %q", got, c.Want)
			}
		})
	}
}

// TestStoreUpgradePlanCarriesTheEstimate proves the printed plan and the
// --dry-run --json plan carry a step's estimate, and only where it has one.
func TestStoreUpgradePlanCarriesTheEstimate(t *testing.T) {
	t.Parallel()
	for _, c := range loadStoreUpgradeEstimateFixture(t).PlanCases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			plan := store.UpgradePlan{DBPath: "/synthetic/peasant.db", From: 75, To: 77, StoreBytes: 1 << 30}
			for _, step := range c.Steps {
				cost, err := store.NewUpgradeCost(step.Cost)
				if err != nil {
					t.Fatal(err)
				}
				plan.Steps = append(plan.Steps, store.UpgradeStep{
					Version: step.Version, Kind: store.UpgradeStepSchema, Title: step.Title, Cost: cost,
					Estimate: store.UpgradeEstimate{Low: seconds(step.LowSeconds), High: seconds(step.HighSeconds)},
				})
			}
			var out strings.Builder
			writeStoreUpgradePlan(&out, plan)
			lines := strings.Split(out.String(), "\n")
			for _, want := range c.WantLines {
				found := false
				for _, line := range lines {
					found = found || line == want
				}
				if !found {
					t.Errorf("plan output lacks the line %q:\n%s", want, out.String())
				}
			}

			raw, err := json.Marshal(schemaUpgradeJSON(plan))
			if err != nil {
				t.Fatal(err)
			}
			var decoded struct {
				Steps []struct {
					Version  int             `json:"version"`
					Estimate json.RawMessage `json:"estimate"`
				} `json:"steps"`
			}
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatal(err)
			}
			if len(decoded.Steps) != len(c.WantJSONEstimates) {
				t.Fatalf("JSON plan has %d steps, want %d", len(decoded.Steps), len(c.WantJSONEstimates))
			}
			for i, want := range c.WantJSONEstimates {
				got := decoded.Steps[i]
				if got.Version != want.Version {
					t.Fatalf("JSON step %d is version %d, want %d", i, got.Version, want.Version)
				}
				if want.HighSeconds == 0 {
					if got.Estimate != nil {
						t.Errorf("JSON step %d carries estimate %s, want none", want.Version, got.Estimate)
					}
					continue
				}
				var estimate struct {
					LowSeconds   int64 `json:"low_seconds"`
					HighSeconds  int64 `json:"high_seconds"`
					Extrapolated *bool `json:"extrapolated"`
				}
				if err := json.Unmarshal(got.Estimate, &estimate); err != nil {
					t.Fatalf("JSON step %d estimate %s: %v", want.Version, got.Estimate, err)
				}
				if estimate.LowSeconds != want.LowSeconds || estimate.HighSeconds != want.HighSeconds || estimate.Extrapolated == nil {
					t.Errorf("JSON step %d estimate = %s, want low %d, high %d and an extrapolated flag", want.Version, got.Estimate, want.LowSeconds, want.HighSeconds)
				}
			}
		})
	}
}
