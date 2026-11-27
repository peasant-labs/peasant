package ingest

import (
	"bytes"
	_ "embed"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/index_progress_fallback.yaml
var indexProgressFallbackYAML []byte

type progressFallbackMetrics struct{ MetricsStore }

func TestIndexFinalizeFallsBackToCountsWhenPriorLogIsMissing(t *testing.T) {
	var fixture struct {
		Name             string `yaml:"name"`
		StreamedOutcomes int    `yaml:"streamedOutcomes"`
		PriorLogs        int    `yaml:"priorLogs"`
		Diagnostic       string `yaml:"diagnostic"`
		WantDone         int    `yaml:"wantDone"`
		WantTotal        int    `yaml:"wantTotal"`
	}
	if err := yaml.Unmarshal(indexProgressFallbackYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Name != "missing-prior-log" || fixture.StreamedOutcomes <= fixture.PriorLogs {
		t.Fatal("invalid missing-log fixture")
	}
	p := coverageFinalizePipeline(t, nil)
	WithArenaSizeBytes(8 << 20)(p)
	p.config.Progress = NewProgressState()
	p.indexers = map[Harness]TranscriptIndexer{}
	p.metricsStore = progressFallbackMetrics{}
	var output bytes.Buffer
	p.progressLogger = slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))
	prior := make([]indexedMeta, fixture.StreamedOutcomes)
	for i := range prior {
		prior[i].indexed = true
	}
	logs := make([]IndexLogEntry, fixture.PriorLogs)
	_, err := p.indexComputeAndFinalize(t.Context(), nil, prior, nil, nil, time.Now(), logs, IndexOutcomeIndexed, "pipeline", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), fixture.Diagnostic) {
		t.Fatalf("missing diagnostic %q: %s", fixture.Diagnostic, output.String())
	}
	stage := p.config.Progress.Snapshot()[StageIndex]
	if stage.Done != fixture.WantDone || stage.Total != fixture.WantTotal || stage.BytesTotal != 0 || stage.BytesDone != 0 {
		t.Fatalf("index progress = %+v, want count-only %d/%d", stage, fixture.WantDone, fixture.WantTotal)
	}
}
