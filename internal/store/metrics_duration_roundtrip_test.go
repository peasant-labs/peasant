package store

import (
	_ "embed"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/metrics_duration_roundtrip.yaml
var durationRoundtripYAML []byte

//go:embed testdata/metrics_duration_roundtrip.manifest.yaml
var durationRoundtripManifestYAML []byte

func TestMetricsDurationStorageRoundtrip(t *testing.T) {
	var fixture struct {
		Cases []struct {
			Name         string  `yaml:"name"`
			Minutes      float64 `yaml:"minutes"`
			Milliseconds int64   `yaml:"milliseconds"`
		} `yaml:"cases"`
	}
	var manifest struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	if err := yaml.Unmarshal(durationRoundtripYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(durationRoundtripManifestYAML, &manifest); err != nil {
		t.Fatal(err)
	}
	s, _ := openGenerationStore(t)
	defer s.Close()
	const id = "7a7a7a7a-7777-4777-8777-7a7a7a7a7a7a"
	seedGenerationSession(t, s, id)
	sid := schema.SessionID(id)
	candidate, blobs := buildTestGeneration(t, sid, "duration-gen", "hello", "in", "out")
	if err := activateTestGeneration(t, s, candidate, blobs); err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, row := range fixture.Cases {
		if seen[row.Name] {
			t.Fatalf("duplicate duration fixture %q", row.Name)
		}
		seen[row.Name] = true
		t.Run(row.Name, func(t *testing.T) {
			before, err := s.ReadMetricInput(t.Context(), sid, false)
			if err != nil {
				t.Fatal(err)
			}
			version := 1
			raw := row.Minutes
			value := &ingest.SessionMetrics{SessionID: sid, QualityMetrics: schema.QualityMetrics{DurationMinutes: &raw, ComputeVersion: &version}}
			if err := s.SaveMetrics(t.Context(), value); err != nil {
				t.Fatal(err)
			}
			got, err := s.GetMetrics(t.Context(), sid)
			if err != nil || got == nil || got.DurationMinutes == nil {
				t.Fatalf("read metrics: %v", err)
			}
			if *got.DurationMinutes != ingest.DurationMsToMinutes(row.Milliseconds) {
				t.Fatalf("stored minutes = %.17g; want %d ms", *got.DurationMinutes, row.Milliseconds)
			}
			canonical := ingest.CanonicalDurationMinutes(raw)
			expected := *got
			expected.DurationMinutes = &canonical
			wantHash, err := ingest.MetricOutputHash(&expected)
			if err != nil {
				t.Fatal(err)
			}
			gotHash, err := ingest.MetricOutputHash(got)
			if err != nil || gotHash != wantHash {
				t.Fatalf("output hash differs: got %s want %s: %v", gotHash, wantHash, err)
			}
			after, err := s.ReadMetricInput(t.Context(), sid, false)
			if err != nil || before.DatabaseHash != after.DatabaseHash {
				t.Fatalf("derived duration changed input hash: %v", err)
			}
		})
	}
	for _, name := range manifest.RequiredNames {
		if !seen[name] {
			t.Fatalf("missing required duration fixture %q", name)
		}
	}
}

func TestMetricsDurationWithoutActiveGeneration(t *testing.T) {
	s, _ := openGenerationStore(t)
	defer s.Close()
	const id = "7a7a7a7a-7777-4777-8777-7a7a7a7a7a7a"
	seedGenerationSession(t, s, id)
	duration := 16.65
	version := 1
	value := &ingest.SessionMetrics{SessionID: schema.SessionID(id), QualityMetrics: schema.QualityMetrics{DurationMinutes: &duration, ComputeVersion: &version}}
	if err := s.SaveMetrics(t.Context(), value); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetMetrics(t.Context(), value.SessionID)
	if err != nil || got == nil || got.DurationMinutes == nil || *got.DurationMinutes != duration {
		t.Fatalf("legacy duration lost without active generation: %+v %v", got, err)
	}
}
