package ingest_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/metrics"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
)

// BenchmarkImportProfilerOverhead imports one synthetic Claude corpus into a
// fresh store with the profiler on and with it off. It is the local
// measurement behind the claim that the always-on profiler costs under 1% of
// an import's wall time; it asserts nothing, since wall time is context and
// not a pass or fail threshold (docs/benchmarks/procedure.md).
//
// The corpus size comes from PEASANT_BENCH_SESSIONS and PEASANT_BENCH_ENTRIES
// (entries per session). The defaults are small so an unconfigured -bench run
// is quick; a measurement sets them so one import takes at least 30 seconds.
// Run the two sub-benchmarks in alternating processes, at least five each, and
// compare medians:
//
//	for i in 1 2 3 4 5; do for mode in off on; do
//	  PEASANT_BENCH_SESSIONS=400 PEASANT_BENCH_ENTRIES=4000 \
//	    go test ./internal/ingest -run '^$' -bench "ImportProfilerOverhead/profiler=$mode\$" -benchtime 1x
//	done; done
//
// The pipeline is wired as `peasant harvest` wires it: the real Claude adapter
// and indexer, the metrics engine, the classifier annotator, and both audit
// loggers, over a store with the managed-generation options.
func BenchmarkImportProfilerOverhead(b *testing.B) {
	sessions := benchEnvInt(b, "PEASANT_BENCH_SESSIONS", 8)
	entries := benchEnvInt(b, "PEASANT_BENCH_ENTRIES", 200)
	source := filepath.Join(b.TempDir(), "source")
	var corpusBytes int64
	for n := range sessions {
		id := fmt.Sprintf("97100000-0000-4000-8000-%012d", n+1)
		path := filepath.Join(source, "-workspace", id+".jsonl")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			b.Fatal(err)
		}
		content := benchClaudeTranscript(id, entries)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			b.Fatal(err)
		}
		stamp := time.Unix(1_700_000_000, 0)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			b.Fatal(err)
		}
		corpusBytes += int64(len(content))
	}
	for _, mode := range []string{"off", "on"} {
		b.Run("profiler="+mode, func(b *testing.B) {
			for b.Loop() {
				b.StopTimer()
				output := filepath.Join(b.TempDir(), "output")
				options, err := store.ManagedGenerationOptions(output)
				if err != nil {
					b.Fatal(err)
				}
				// The golden copy needs a *testing.T. A fresh open applies the
				// baseline snapshot, as a first install does, outside the timed
				// region.
				db, err := store.Open(filepath.Join(b.TempDir(), "bench.db"), options...)
				if err != nil {
					b.Fatal(err)
				}
				var profiler *ingest.IndexProfiler
				if mode == "on" {
					profiler = &ingest.IndexProfiler{}
				}
				fs := &ingest.OSFileSystem{}
				cfg := ingest.PipelineConfig{
					Sources:       map[ingest.Harness]ingest.SourceConfig{ingest.HarnessClaudeCode: {Enabled: true, Paths: []ingest.ResolvedPath{ingest.ResolvedPath(source)}}},
					OutputDir:     ingest.ResolvedPath(output),
					IndexProfiler: profiler,
				}
				pipeline, err := ingest.NewPipeline(fs, testutil.NoGitResolver(), ingest.DefaultAdapterRegistry, cfg,
					ingest.WithStore(db), ingest.WithMetricsStore(db), ingest.WithSalt(db.InstallationSalt()),
					ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})),
					ingest.WithAnalyzer(metrics.NewEngineWithModels(db, db)),
					ingest.WithClassifier(metrics.NewClassifierAnnotator(db, db)),
					ingest.WithLogger(db), ingest.WithIndexLogger(db))
				if err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				result, err := pipeline.Run(b.Context())
				b.StopTimer()
				if err != nil {
					b.Fatal(err)
				}
				if result.Summary.New != sessions || result.Summary.Indexed != sessions || result.Summary.Errors != 0 {
					b.Fatalf("the import did not store and index the whole corpus: %+v", result.Summary)
				}
				if profiler != nil {
					b.ReportMetric(float64(profiler.Observations()), "observations")
				}
				if err := db.Close(); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
			}
			b.ReportMetric(float64(corpusBytes), "corpus-bytes")
		})
	}
}

func benchEnvInt(b *testing.B, name string, fallback int) int {
	b.Helper()
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 {
		b.Fatalf("%s=%q is not a positive integer; set it to the corpus size to import", name, raw)
	}
	return value
}

// benchClaudeTranscript builds one synthetic session: alternating user and
// assistant records with enough text to move bytes through every stage.
func benchClaudeTranscript(sessionID string, entries int) string {
	filler := strings.Repeat("synthetic transcript text for the import measurement. ", 12)
	var content strings.Builder
	for n := range entries {
		at := time.Unix(1_700_000_000+int64(n), 0).UTC().Format(time.RFC3339)
		if n%2 == 0 {
			fmt.Fprintf(&content, `{"type":"user","sessionId":"%s","uuid":"u%d","cwd":"/work/project","timestamp":"%s","message":{"role":"user","content":"prompt %d: %s"}}`+"\n", sessionID, n, at, n, filler)
			continue
		}
		fmt.Fprintf(&content, `{"type":"assistant","sessionId":"%s","uuid":"a%d","parentUuid":"u%d","cwd":"/work/project","timestamp":"%s","message":{"role":"assistant","model":"claude-synthetic","content":[{"type":"text","text":"reply %d: %s"}],"usage":{"input_tokens":120,"output_tokens":80}}}`+"\n", sessionID, n, n-1, at, n, filler)
	}
	return content.String()
}
