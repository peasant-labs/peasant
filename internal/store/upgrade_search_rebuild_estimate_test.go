package store

import (
	_ "embed"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/upgrade_search_rebuild_estimate.yaml
var upgradeSearchRebuildEstimateYAML []byte

// TestUpgradePlanEstimatesTheSearchRebuild proves that the plan estimates
// the search trigger repair exactly when it rebuilds the index, that the
// estimate's basis is the search index as dbstat measures it, and that its
// band is ordered and positive. The quick repair, and every other step of
// the plan, carries no estimate.
func TestUpgradePlanEstimatesTheSearchRebuild(t *testing.T) {
	t.Parallel()
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Filler        struct {
			Rows       int `yaml:"rows"`
			RowBytes   int `yaml:"rowBytes"`
			Vocabulary int `yaml:"vocabulary"`
		} `yaml:"filler"`
		Cases []struct {
			Name         string `yaml:"name"`
			Window       string `yaml:"window"`
			WantEstimate bool   `yaml:"wantEstimate"`
		} `yaml:"cases"`
	}
	if err := yaml.Unmarshal(upgradeSearchRebuildEstimateYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range fixture.Cases {
		names = append(names, c.Name)
	}
	if err := validateRecoveryRequiredNames(fixture.RequiredNames, names, "upgrade search rebuild estimate"); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "estimate.db")
			conn := openMigrationV76Store(t, path, c.Window, false)
			removeBodySentinel(t, conn)
			// Filler text makes the index's pages, not its fixed tables,
			// the size the estimate reads.
			seedSearchRebuildFiller(t, conn, v76BodiesSession, fixture.Filler.Rows, fixture.Filler.RowBytes, fixture.Filler.Vocabulary)
			index := dbstatBytesOnConn(t, conn, `name LIKE 'session_search_fts%'`)
			if err := conn.Close(); err != nil {
				t.Fatal(err)
			}
			plan := mustPlan(t, path)
			found := false
			for _, step := range plan.Steps {
				if step.Version != searchRepairSchemaVersion || step.Kind != UpgradeStepSchema {
					if step.Estimate.Known() {
						t.Errorf("step %d (%s) carries an estimate; only the search index rebuild has one", step.Version, step.Title)
					}
					continue
				}
				found = true
				if step.Estimate.Known() != c.WantEstimate {
					t.Fatalf("search trigger repair (%s) estimate known = %v, want %v", step.Cost, step.Estimate.Known(), c.WantEstimate)
				}
				if step.Cost.Long() != c.WantEstimate {
					t.Errorf("search trigger repair is %s; an estimate belongs to the long rebuild only", step.Cost)
				}
				if !c.WantEstimate {
					continue
				}
				estimate := step.Estimate
				if estimate.Low <= 0 || estimate.Low > estimate.High {
					t.Errorf("estimate band = %s to %s, want a positive, ordered band", estimate.Low, estimate.High)
				}
				// The structure-record estimate came out about one percent
				// above dbstat on the measured shapes.
				if estimate.BasisBytes < index*9/10 || estimate.BasisBytes > index*3/2 {
					t.Errorf("estimate basis = %d bytes, want the search index's size (dbstat %d bytes)", estimate.BasisBytes, index)
				}
			}
			if !found {
				t.Fatal("the plan has no search trigger repair step")
			}
		})
	}
}
