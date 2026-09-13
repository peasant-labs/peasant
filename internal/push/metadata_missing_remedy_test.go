package push_test

import (
	"bytes"
	"context"
	_ "embed"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/metadata_missing_remedy.yaml
var metadataMissingRemedyFixtureData []byte

// Missing database capture needs normal ingest regardless of the historical
// slug. Filesystem-path diagnosis no longer describes publication failures.
func TestPipeline_MetadataMissingRequiresNormalIngest(t *testing.T) {
	var cases []struct {
		Name     string `yaml:"name"`
		HostSlug string `yaml:"hostSlug"`
	}
	if err := yaml.Unmarshal(metadataMissingRemedyFixtureData, &cases); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, tc := range cases {
		seen[tc.Name] = true
	}
	if err := testutil.RequireFixtureNames("database recovery advice", "case", []string{"intact-slug", "redacted-slug", "partially-redacted-slug"}, seen); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			db := &testutil.StubPushStore{Sessions: []ingest.PushSessionRow{makeSession(testutil.TestSessionUUID, tc.HostSlug, string(defaults.HarnessClaudeCode), nil)}}
			publisher := &testutil.StubPublisher{}
			var output bytes.Buffer
			pipeline := newTestPipeline(db, publisher, testutil.NewMemFS(), baseTestConfig(), push.PipelineConfig{}, &output)
			result, err := pipeline.Run(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if result.Errors != 1 || len(result.Sessions) != 1 {
				t.Fatalf("result=%+v", result)
			}
			failure := result.Sessions[0].Error
			if push.ClassifyPushError(failure) != push.CategoryMetadataMissing {
				t.Fatalf("wrong category: %v", failure)
			}
			if !strings.Contains(failure.Error(), "run peasant ingest") || !strings.Contains(failure.Error(), "peasant.db") {
				t.Fatalf("unactionable failure: %v", failure)
			}
			if strings.Contains(failure.Error(), "--force") || strings.Contains(failure.Error(), "directory that was never written") {
				t.Fatalf("obsolete repair: %v", failure)
			}
			if len(publisher.Calls) != 0 || len(db.Publications) != 0 {
				t.Fatal("incomplete capture published")
			}
		})
	}
}
