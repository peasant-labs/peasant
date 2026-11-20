package main

import (
	_ "embed"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/store_upgrade_estimate.yaml
var storeUpgradeEstimateYAML []byte

func TestStoreUpgradeEstimateLabel(t *testing.T) {
	t.Parallel()
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name        string `yaml:"name"`
			LowSeconds  int    `yaml:"lowSeconds"`
			HighSeconds int    `yaml:"highSeconds"`
			Want        string `yaml:"want"`
		} `yaml:"cases"`
	}
	if err := yaml.Unmarshal(storeUpgradeEstimateYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	present := map[string]bool{}
	for _, c := range fixture.Cases {
		present[c.Name] = true
	}
	if err := testutil.RequireFixtureNames("store upgrade estimate", "case", fixture.RequiredNames, present); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			got := storeUpgradeEstimateLabel(store.UpgradeEstimate{
				BasisBytes: 1,
				Low:        time.Duration(c.LowSeconds) * time.Second,
				High:       time.Duration(c.HighSeconds) * time.Second,
			})
			if got != c.Want {
				t.Errorf("estimate label = %q, want %q", got, c.Want)
			}
		})
	}
}

// TestStoreUpgradePlanPrintsTheEstimate proves the printed plan carries a
// step's estimate on the step's own line, and no estimate where the step has
// none.
func TestStoreUpgradePlanPrintsTheEstimate(t *testing.T) {
	t.Parallel()
	plan := store.UpgradePlan{DBPath: "/synthetic/peasant.db", From: 75, To: 76, StoreBytes: 1 << 30, Steps: []store.UpgradeStep{
		{Version: 76, Kind: store.UpgradeStepSchema, Title: "rebuild the search index", Cost: store.UpgradeCostScalesWithText,
			Estimate: store.UpgradeEstimate{BasisBytes: 1 << 30, Low: 330 * time.Second, High: 1210 * time.Second}},
		{Version: 77, Kind: store.UpgradeStepSchema, Title: "mark the stored turn counts", Cost: store.UpgradeCostQuick},
	}}
	var out strings.Builder
	writeStoreUpgradePlan(&out, plan)
	for _, want := range []string{
		"  1. rebuild the search index (long: scales with the stored text; estimate: 5 to 21 minutes)\n",
		"  2. mark the stored turn counts (quick)\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("plan output lacks %q:\n%s", want, out.String())
		}
	}
}
