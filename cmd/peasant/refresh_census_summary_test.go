package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/refresh_census_summary.yaml
var refreshCensusSummaryYAML []byte

func TestHarvestAndKickstartCensusSummary(t *testing.T) {
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name     string   `yaml:"name"`
			Stored   bool     `yaml:"stored"`
			Census   bool     `yaml:"census"`
			Contains []string `yaml:"contains"`
			Absent   []string `yaml:"absent"`
		} `yaml:"cases"`
	}
	if err := yaml.Unmarshal(refreshCensusSummaryYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, row := range fixture.Cases {
		if seen[row.Name] {
			t.Fatalf("duplicate case %q", row.Name)
		}
		seen[row.Name] = true
		t.Run(row.Name, func(t *testing.T) {
			result := &ingest.PipelineResult{CensusStored: row.Stored}
			if row.Census {
				result.Census = &ingest.RefreshCensus{Rows: []ingest.RefreshCensusRow{{Reason: ingest.OutcomeReasonRefreshNew, Lane: ingest.RefreshLaneNative, Sessions: 1, SourceBytes: 1024}}}
			}
			var output bytes.Buffer
			if err := printJSON(&output, result); err != nil {
				t.Fatal(err)
			}
			var document map[string]json.RawMessage
			if err := json.Unmarshal(output.Bytes(), &document); err != nil {
				t.Fatal(err)
			}
			var stored bool
			if err := json.Unmarshal(document["census_stored"], &stored); err != nil || stored != row.Stored {
				t.Fatalf("census_stored = %s, want %t (error %v)", document["census_stored"], row.Stored, err)
			}
			output.Reset()
			printSummary(&output, result, false, false, "", "", nil, 0)
			for _, want := range row.Contains {
				if !strings.Contains(output.String(), want) {
					t.Errorf("summary missing %q", want)
				}
			}
			for _, missing := range row.Absent {
				if strings.Contains(output.String(), missing) {
					t.Errorf("summary unexpectedly contains %q", missing)
				}
			}
			view := kickstartIngestResult(result)
			if (view.Census != nil) != (row.Stored && row.Census) {
				t.Fatalf("kickstart census = %+v", view.Census)
			}
		})
	}
	for _, name := range fixture.RequiredNames {
		if !seen[name] {
			t.Fatalf("missing case %q", name)
		}
	}
}
