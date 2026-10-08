package main

import (
	"bytes"
	_ "embed"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/cli/index_lane_profile.yaml
var indexLaneProfileYAML []byte

func TestPrintIndexLaneProfile(t *testing.T) {
	var fixture struct {
		Count     int         `yaml:"wait_count"`
		P50       int         `yaml:"p50_ms"`
		Max       int         `yaml:"max_ms"`
		Peak      int64       `yaml:"peak_bytes"`
		Histogram map[int]int `yaml:"histogram"`
		Expected  []string    `yaml:"expected"`
	}
	if err := yaml.Unmarshal(indexLaneProfileYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	printIndexProfile(&output, ingest.IndexProfileSnapshot{FlushWaitCount: fixture.Count,
		FlushWaitP50: time.Duration(fixture.P50) * time.Millisecond, FlushWaitMax: time.Duration(fixture.Max) * time.Millisecond,
		StagedPeakBytes: fixture.Peak, ActivationSizes: fixture.Histogram})
	for _, expected := range fixture.Expected {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("profile output missing %q: %s", expected, output.String())
		}
	}
}
