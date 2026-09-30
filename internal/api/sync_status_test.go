package api

import (
	_ "embed"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/sync_status.yaml
var syncStatusYAML []byte

// TestSyncStatusNamesWhyAHeldRowWaits runs testdata/sync_status.yaml through
// the status every sync chooser row carries, on the flat list and the grouped
// view alike.
func TestSyncStatusNamesWhyAHeldRowWaits(t *testing.T) {
	t.Parallel()
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name           string `yaml:"name"`
			MetricsMissing bool   `yaml:"metricsMissing"`
			MetadataReady  bool   `yaml:"metadataReady"`
			Ingested       int64  `yaml:"ingested"`
			PushedAt       *int64 `yaml:"pushedAt"`
			Expect         struct {
				Status     schema.SyncStatus     `yaml:"status"`
				HoldReason schema.SyncHoldReason `yaml:"holdReason"`
			} `yaml:"expect"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeNamedFixtureYAML(syncStatusYAML, &fixture); err != nil {
		t.Fatalf("testdata/sync_status.yaml: %v", err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			row := ingest.PushSessionRow{SessionID: "s-1", IngestedMs: c.Ingested, PushedAt: c.PushedAt}
			status, hold := computeSyncStatus(row, c.MetricsMissing, c.MetadataReady)
			if status != c.Expect.Status || hold != c.Expect.HoldReason {
				t.Fatalf("status = %q, hold = %q; want %q, %q", status, hold, c.Expect.Status, c.Expect.HoldReason)
			}
			summary := schema.LocalSyncSummary{ID: "s-1", Harness: schema.HarnessClaudeCode, ProjectHash: schema.ProjectHash("abcdef1234abcdef1234abcdef1234abcdef1234abcdef1234abcdef12345678"), SyncStatus: status, HoldReason: hold}
			if err := summary.Validate(); err != nil {
				t.Fatalf("the row breaks the contract: %v", err)
			}
		})
	}
}
