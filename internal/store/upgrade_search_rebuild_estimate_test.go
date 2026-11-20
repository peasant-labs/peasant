package store

import (
	_ "embed"
	"path/filepath"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/upgrade_search_rebuild_estimate.yaml
var upgradeSearchRebuildEstimateYAML []byte

type upgradeSearchRebuildEstimateFixture struct {
	RequiredNames []string `yaml:"requiredNames"`
	Filler        struct {
		Rows       int `yaml:"rows"`
		RowBytes   int `yaml:"rowBytes"`
		Vocabulary int `yaml:"vocabulary"`
	} `yaml:"filler"`
	PlanCases []struct {
		Name         string `yaml:"name"`
		Window       string `yaml:"window"`
		WantEstimate bool   `yaml:"wantEstimate"`
	} `yaml:"planCases"`
	BandCases []struct {
		Name             string `yaml:"name"`
		BasisBytes       int64  `yaml:"basisBytes"`
		WantEstimate     bool   `yaml:"wantEstimate"`
		WantExtrapolated bool   `yaml:"wantExtrapolated"`
	} `yaml:"bandCases"`
}

func loadUpgradeSearchRebuildEstimateFixture(t *testing.T) upgradeSearchRebuildEstimateFixture {
	t.Helper()
	var fixture upgradeSearchRebuildEstimateFixture
	if err := yaml.Unmarshal(upgradeSearchRebuildEstimateYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range fixture.PlanCases {
		names = append(names, c.Name)
	}
	for _, c := range fixture.BandCases {
		names = append(names, c.Name)
	}
	if err := validateRecoveryRequiredNames(fixture.RequiredNames, names, "upgrade search rebuild estimate"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// TestSearchRebuildEstimateBand proves the band divides the basis by the
// measured rates in defaults and marks a basis past the measurements.
func TestSearchRebuildEstimateBand(t *testing.T) {
	t.Parallel()
	for _, c := range loadUpgradeSearchRebuildEstimateFixture(t).BandCases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			got := searchRebuildEstimate(c.BasisBytes)
			if got.Known() != c.WantEstimate {
				t.Fatalf("estimate known = %v, want %v", got.Known(), c.WantEstimate)
			}
			if !c.WantEstimate {
				if got != (UpgradeEstimate{}) {
					t.Errorf("estimate = %+v, want none", got)
				}
				return
			}
			at := func(rate int64) time.Duration {
				return time.Duration(float64(c.BasisBytes) / float64(rate) * float64(time.Second))
			}
			if want := at(defaults.StoreUpgradeSearchRebuildIndexBytesPerSecondFast); got.Low != want {
				t.Errorf("Low = %s, want %s (the basis at the fast rate)", got.Low, want)
			}
			if want := at(defaults.StoreUpgradeSearchRebuildIndexBytesPerSecondSlow); got.High != want {
				t.Errorf("High = %s, want %s (the basis at the slow rate)", got.High, want)
			}
			if got.Extrapolated != c.WantExtrapolated {
				t.Errorf("Extrapolated = %v, want %v", got.Extrapolated, c.WantExtrapolated)
			}
		})
	}
}

// TestUpgradePlanEstimatesTheSearchRebuild proves that the plan estimates
// the search trigger repair exactly when it rebuilds the index, from the
// search index's size, which is checked against dbstat. Every other step of
// the plan carries no estimate.
func TestUpgradePlanEstimatesTheSearchRebuild(t *testing.T) {
	t.Parallel()
	fixture := loadUpgradeSearchRebuildEstimateFixture(t)
	for _, c := range fixture.PlanCases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "estimate.db")
			conn := openMigrationV76Store(t, path, c.Window, false)
			removeBodySentinel(t, conn)
			// Filler text makes the index's pages, not its fixed tables,
			// the size the estimate reads.
			seedSearchRebuildFiller(t, conn, v76BodiesSession, fixture.Filler.Rows, fixture.Filler.RowBytes, fixture.Filler.Vocabulary)
			truth := dbstatBytesOnConn(t, conn, `name LIKE 'session_search_fts%'`)
			basis, err := fts5IndexBytesOnConn(conn, "session_search_fts")
			if err != nil {
				t.Fatal(err)
			}
			if err := conn.Close(); err != nil {
				t.Fatal(err)
			}
			// The structure-record estimate came out about one percent above
			// dbstat on the measured shapes.
			if basis < truth*9/10 || basis > truth*3/2 {
				t.Errorf("search index size from its structure record = %d bytes, want about dbstat's %d", basis, truth)
			}
			found := false
			for _, step := range mustPlan(t, path).Steps {
				if step.Version != searchRepairSchemaVersion || step.Kind != UpgradeStepSchema {
					if step.Estimate.Known() {
						t.Errorf("step %d (%s) carries an estimate; only the search index rebuild has one", step.Version, step.Title)
					}
					continue
				}
				found = true
				if step.Estimate.Known() != c.WantEstimate || step.Cost.Long() != c.WantEstimate {
					t.Fatalf("search trigger repair is %s with estimate known = %v; want long and estimated = %v", step.Cost, step.Estimate.Known(), c.WantEstimate)
				}
				if c.WantEstimate && step.Estimate != searchRebuildEstimate(basis) {
					t.Errorf("plan estimate = %+v, want the band for the %d-byte index, %+v", step.Estimate, basis, searchRebuildEstimate(basis))
				}
			}
			if !found {
				t.Fatal("the plan has no search trigger repair step")
			}
		})
	}
}
