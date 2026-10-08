package store_test

import (
	"bytes"
	_ "embed"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/harvest_oversize_advisory.yaml
var harvestOversizeAdvisoryYAML []byte

// TestHarvestOversizeAdvisory exercises the staging entry the harvest writer
// uses, retaining warnings independently of a progress renderer's logger.
func TestHarvestOversizeAdvisory(t *testing.T) {
	t.Parallel()
	var fixtures struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name         string `yaml:"name"`
			BatchBytes   int64  `yaml:"batchBytes"`
			WantAdvisory bool   `yaml:"wantAdvisory"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeNamedFixtureYAML(harvestOversizeAdvisoryYAML, &fixtures); err != nil {
		t.Fatal(err)
	}
	budgetFixtures := loadContentWriteBudgetFixtures(t)
	for _, c := range fixtures.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			var output bytes.Buffer
			ctx := ingest.WithWriteAdvisoryReporter(t.Context(), func(diagnostic ingest.DiagnosticEntry) {
				output.WriteString(diagnostic.Location + ": " + diagnostic.Message)
			})
			cfg := ingest.WriteConfig{BatchBytes: c.BatchBytes}.WithDefaults(1)
			db := openWriteBudgetHarmonized(t, storetest.CopyGoldenDB(t), store.WithWriteConfig(cfg))
			sid := seedWriteBudgetSession(t, t.Context(), db, "44444444-4444-4444-8444-444444444444", budgetFixtures)
			v2, blobs := writeBudgetV2(t, sid, "gen_advisory", []string{"oversize advisory body"})
			if _, err := db.StageGeneration(ctx, writeBudgetActivation(v2, blobs)); err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(output.String(), "no other writer running is recommended"); got != c.WantAdvisory {
				t.Fatalf("advisory=%v, want %v; output: %s", got, c.WantAdvisory, output.String())
			}
			if c.WantAdvisory && (!strings.Contains(output.String(), string(sid)) || !strings.Contains(output.String(), "writer hold target")) {
				t.Fatalf("advisory must identify the session and widened hold: %s", output.String())
			}
		})
	}
}
