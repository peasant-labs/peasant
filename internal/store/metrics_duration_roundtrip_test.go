package store

import (
	_ "embed"
	"reflect"
	"strings"
	"testing"
	"time"

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
	if err := yaml.Unmarshal(durationRoundtripYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	required, err := decodeRecoveryRequiredNames(durationRoundtripManifestYAML)
	if err != nil {
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
	var actual []string
	for name := range seen {
		actual = append(actual, name)
	}
	if err := validateRecoveryRequiredNames(required, actual, "duration"); err != nil {
		t.Fatal(err)
	}
}

func TestMetricsDurationWithoutActiveGeneration(t *testing.T) {
	s, _ := openGenerationStore(t)
	defer s.Close()
	const id = "7a7a7a7a-7777-4777-8777-7a7a7a7a7a7a"
	seedGenerationSession(t, s, id)
	before, beforeErr := s.ReadCapturedStats(t.Context(), schema.SessionID(id))
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
	after, afterErr := s.ReadCapturedStats(t.Context(), schema.SessionID(id))
	if (beforeErr == nil) != (afterErr == nil) || !reflect.DeepEqual(before, after) {
		t.Fatalf("legacy save changed captured stats: before=%+v after=%+v error=%v", before, after, afterErr)
	}
}

func TestLegacyMetricsSaveWithFutureCaptureClock(t *testing.T) {
	s, _ := openGenerationStore(t)
	defer s.Close()
	const id = "7a7a7a7a-7777-4777-8777-7a7a7a7a7a7a"
	seedGenerationSession(t, s, id)
	sid := schema.SessionID(id)
	candidate, blobs := buildTestGeneration(t, sid, "clock-gen", "hello", "in", "out")
	if err := activateTestGeneration(t, s, candidate, blobs); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour).UnixMilli()
	if _, err := s.UpsertCapturedStats(t.Context(), CapturedStats{SessionID: sid, Source: StatsSourceHarness, UpdatedAtMs: future}); err != nil {
		t.Fatal(err)
	}
	duration := 16.65
	version := 1
	computed := time.Now().UnixMilli()
	if err := s.SaveMetrics(t.Context(), &ingest.SessionMetrics{SessionID: sid, QualityMetrics: schema.QualityMetrics{DurationMinutes: &duration, ComputeVersion: &version, ComputedAt: &computed}}); err != nil {
		t.Fatalf("legacy save with future captured clock: %v", err)
	}
	value, err := s.GetMetrics(t.Context(), sid)
	if err != nil || value == nil || value.DurationMinutes == nil || *value.DurationMinutes != duration {
		t.Fatalf("legacy save did not persist duration: %+v %v", value, err)
	}
	stats, err := s.ReadCapturedStats(t.Context(), sid)
	if err != nil || stats.UpdatedAtMs != future {
		t.Fatalf("legacy save regressed captured clock: %+v %v", stats, err)
	}
}

func TestMetricsSaveRejectsMismatchedReadbackAndRollsBack(t *testing.T) {
	s, _ := openGenerationStore(t)
	defer s.Close()
	const id = "7a7a7a7a-7777-4777-8777-7a7a7a7a7a7a"
	seedGenerationSession(t, s, id)
	sid := schema.SessionID(id)
	candidate, blobs := buildTestGeneration(t, sid, "rollback-gen", "hello", "in", "out")
	if err := activateTestGeneration(t, s, candidate, blobs); err != nil {
		t.Fatal(err)
	}
	duration := 16.65
	version := 1
	if err := s.SaveMetrics(t.Context(), &ingest.SessionMetrics{SessionID: sid, QualityMetrics: schema.QualityMetrics{DurationMinutes: &duration, ComputeVersion: &version}}); err != nil {
		t.Fatal(err)
	}
	input, err := s.ReadMetricInput(t.Context(), sid, false)
	if err != nil {
		t.Fatal(err)
	}
	beforeStats, err := s.ReadCapturedStats(t.Context(), sid)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	inputHash := input.DatabaseHash
	value := &ingest.SessionMetrics{SessionID: sid, InputHash: &inputHash, QualityMetrics: schema.QualityMetrics{ComputeVersion: &version, ComputedAt: &now}}
	outputHash, err := ingest.MetricOutputHash(value)
	if err != nil {
		t.Fatal(err)
	}
	value.OutputHash = &outputHash
	err = s.SaveMetricsForInput(t.Context(), input, value)
	if err == nil || !strings.Contains(err.Error(), "stored values differ from computed output") || !strings.Contains(err.Error(), "retry the harvest") {
		t.Fatalf("expected actionable readback refusal, got %v", err)
	}
	after, err := s.GetMetrics(t.Context(), sid)
	if err != nil || !reflect.DeepEqual(input.Existing, after) {
		t.Fatalf("refusal changed prior metrics: %v", err)
	}
	afterStats, err := s.ReadCapturedStats(t.Context(), sid)
	if err != nil || !reflect.DeepEqual(beforeStats, afterStats) {
		t.Fatalf("refusal changed captured stats: %v", err)
	}
}
