package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/retained_unknown_report.yaml
var retainedReportYAML []byte

//go:embed testdata/retained_unknown_report.manifest.yaml
var retainedReportManifest []byte

func TestRetainedUnknownReport(t *testing.T) {
	var fixture struct {
		CoverageExpected []string `yaml:"coverage_expected"`
		Cases            []struct {
			Name        string `yaml:"name"`
			Harness     string `yaml:"harness"`
			Namespace   string `yaml:"namespace"`
			Kind        string `yaml:"kind"`
			Occurrences int    `yaml:"occurrences"`
			Sessions    int    `yaml:"sessions"`
			Expected    string `yaml:"expected"`
		} `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(retainedReportYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatal("expected one YAML document")
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(retainedReportManifest, "retained report")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range fixture.Cases {
		names = append(names, c.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "retained report"); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			harness := schema.Harness(c.Harness)
			if !harness.IsKnown() {
				t.Fatalf("unknown fixture harness %q", c.Harness)
			}
			counts := []ingest.RetainedUnknownKindCount{{Harness: harness, Namespace: c.Namespace, Kind: c.Kind, Occurrences: c.Occurrences, Sessions: c.Sessions}}
			var output bytes.Buffer
			printRecordKindsReport(&output, nil, counts)
			if !strings.Contains(output.String(), c.Expected) {
				t.Fatalf("missing occurrence/session distinction: %s", output.String())
			}
			if !strings.Contains(output.String(), "registry coverage (not run observations)") {
				t.Fatal("global registry inventory is presented as run observations")
			}
			for _, expected := range fixture.CoverageExpected {
				if !strings.Contains(output.String(), expected+"\n") {
					t.Fatalf("missing qualified registry coverage %q: %s", expected, output.String())
				}
			}
			output.Reset()
			if err := printJSON(&output, &ingest.PipelineResult{Summary: ingest.PipelineSummary{RetainedUnknownKinds: counts}}); err != nil {
				t.Fatal(err)
			}
			var decoded jsonPipelineResult
			if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(decoded.Summary.RetainedUnknownKinds, counts) {
				t.Fatal("JSON and human report disagree")
			}
			registry, err := ingest.LoadRecordKindRegistry()
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(decoded.TrackedNotVisualized, registry.TrackedNotVisualized()) {
				t.Fatal("JSON registry coverage lost qualified identities")
			}
		})
	}
}
