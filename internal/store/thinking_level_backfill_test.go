package store_test

import (
	"context"
	_ "embed"
	"slices"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/thinking_level_backfill.yaml
var thinkingLevelBackfillYAML []byte

//go:embed testdata/thinking_level_backfill.manifest.yaml
var thinkingLevelBackfillManifest []byte

// TestStore_ThinkingLevelBackfillSelection exercises the maintenance inventories
// against the real targets, without a force flag or a schema-driven refresh.
func TestStore_ThinkingLevelBackfillSelection(t *testing.T) {
	t.Parallel()
	var fixtures struct {
		Cases []struct {
			Name    string         `yaml:"name"`
			Harness ingest.Harness `yaml:"harness"`
			Adapter int            `yaml:"adapter"`
			Indexer int            `yaml:"indexer"`
			Format  int            `yaml:"format"`
			Native  bool           `yaml:"native"`
			Stale   bool           `yaml:"stale"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(thinkingLevelBackfillYAML, &fixtures); err != nil {
		t.Fatal(err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(thinkingLevelBackfillManifest, "thinking-level back-fill")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(fixtures.Cases))
	for i, c := range fixtures.Cases {
		names[i] = c.Name
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "thinking-level back-fill"); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixtures.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			s := openTestStore(t)
			ctx := context.Background()
			sid := ingest.SessionID("99999999-9999-4999-8999-999999999999")
			seedRepairSession(t, s, harvesterRepairSession{ID: sid, Harness: c.Harness, AdapterVersion: &c.Adapter})
			inputSQL(t, s, sid, "UPDATE sessions SET index_version = ?, schema_version = ? WHERE session_id = ?", c.Indexer, 11)
			targets := ingest.HarvesterVersionRegistry
			if c.Native {
				targets = ingest.GenerationTargets(targets)
			}
			assertSelected := func(want bool) {
				t.Helper()
				adapters, err := s.ListStaleAdapterSessions(ctx, targets, nil)
				if err != nil {
					t.Fatal(err)
				}
				indexers, err := s.ListStaleIndexSessions(ctx, targets)
				if err != nil {
					t.Fatal(err)
				}
				if slices.Contains(adapters, sid) != want || slices.Contains(indexers, sid) != want {
					t.Fatalf("stale adapter/index inventories = %v / %v, want selected=%t", adapters, indexers, want)
				}
			}
			assertSelected(c.Stale)
			// Successful maintenance stamps the current targets; an optional-only
			// old metadata version must not keep either inventory stale.
			target := targets[c.Harness]
			if target.IndexVersion != c.Format {
				t.Fatalf("target format = %d, want unchanged format %d", target.IndexVersion, c.Format)
			}
			inputSQL(t, s, sid, "UPDATE sessions SET adapter_version = ?, index_version = ? WHERE session_id = ?", target.AdapterVersion, target.IndexerVersion)
			assertSelected(false)
		})
	}
}
