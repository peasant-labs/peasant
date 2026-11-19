package ingest_test

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/metrics"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/metrics_duration_roundtrip.yaml
var metricsDurationRoundtripYAML []byte

//go:embed testdata/metrics_duration_roundtrip.manifest.yaml
var metricsDurationRoundtripManifestYAML []byte

func TestManagedMetricsDurationRoundtrip(t *testing.T) {
	var fixture struct {
		Cases []struct {
			Name                 string `yaml:"name"`
			Entries              int    `yaml:"entries"`
			DurationMS           int64  `yaml:"durationMs"`
			Refresh              bool   `yaml:"refresh"`
			Recovery             bool   `yaml:"recovery"`
			SpanNS               int64  `yaml:"spanNs"`
			FutureClock          bool   `yaml:"futureClock"`
			RetainedDurationMS   int64  `yaml:"retainedDurationMs"`
			RetainedOutputTokens int    `yaml:"retainedOutputTokens"`
			NoMetrics            bool   `yaml:"noMetrics"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(metricsDurationRoundtripYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	if err := testutil.DecodeFixtureYAML(metricsDurationRoundtripManifestYAML, &manifest); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, row := range fixture.Cases {
		if seen[row.Name] {
			t.Fatalf("duplicate fixture %q", row.Name)
		}
		seen[row.Name] = true
		t.Run(row.Name, func(t *testing.T) {
			root := t.TempDir()
			source := filepath.Join(root, "source")
			output := filepath.Join(root, "output")
			db := durationStore(t, filepath.Join(root, "store.db"), output)
			defer db.Close()
			durationCorpus(t, source, row.Entries, row.SpanNS)
			pipeline := durationPipeline(t, db, source, output, false)
			result, err := pipeline.Run(t.Context())
			if err != nil || result.Summary.Indexed != 1 {
				t.Fatalf("import: %+v %v", result, err)
			}
			sid := ingest.SessionID("bec00000-0000-4000-8000-000000000001")
			input, err := db.ReadMetricInput(t.Context(), sid, true)
			if err != nil || input == nil {
				t.Fatalf("read input: %v", err)
			}
			if row.Refresh {
				old := input.GenerationID
				version := ingest.GenerationTargets(ingest.HarvesterVersionRegistry)[ingest.HarnessClaudeCode].AdapterVersion - 1
				durationDowngradeAdapter(t, db, version)
				if count := durationStampMetadata(t, output, version, "changed project"); count != 1 {
					t.Fatalf("metadata count %d", count)
				}
				if err := os.Rename(source, source+"-unavailable"); err != nil {
					t.Fatal(err)
				}
				// The changed project fails the unchanged proof, so the
				// refresh parses the stored transcript (the source is gone)
				// and installs the new generation.
				result, err = pipeline.Run(t.Context())
				if err != nil || result.Summary.Indexed != 1 {
					t.Fatalf("adapter refresh did not parse the stored transcript once: %+v %v", result, err)
				}
				input, err = db.ReadMetricInput(t.Context(), sid, true)
				if err != nil || input == nil || input.GenerationID == old {
					t.Fatalf("generation not installed: %+v %v", result, err)
				}
			}
			if row.Recovery {
				conn, err := db.Pool().Take(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				err = sqlitex.ExecuteTransient(conn, `UPDATE session_captured_stats SET duration_ms = 998999 WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{string(sid)}})
				db.Pool().Put(conn)
				if err != nil {
					t.Fatal(err)
				}
				input, err = db.ReadMetricInput(t.Context(), sid, true)
				if err != nil || metrics.MetricsCurrentForInput(input) {
					t.Fatalf("legacy truncated row unexpectedly current: %v", err)
				}
				result, err = pipeline.Run(t.Context())
				if err != nil || result.Summary.Computed != 1 {
					t.Fatalf("harvest did not recompute legacy metrics: %+v %v", result, err)
				}
				input, err = db.ReadMetricInput(t.Context(), sid, true)
				if err != nil || input == nil {
					t.Fatalf("read recomputed metrics: %v", err)
				}
			}
			if row.FutureClock {
				duration := int64(1)
				_, err := db.UpsertCapturedStats(t.Context(), store.CapturedStats{SessionID: sid, DurationMs: &duration, Source: store.StatsSourceHarness, UpdatedAtMs: time.Now().Add(time.Hour).UnixMilli()})
				if err != nil {
					t.Fatal(err)
				}
				result, err = pipeline.Run(t.Context())
				if err != nil || result.Summary.Computed != 1 {
					t.Fatalf("future capture clock did not recompute: %+v %v", result, err)
				}
				input, err = db.ReadMetricInput(t.Context(), sid, true)
				if err != nil || input == nil {
					t.Fatalf("read future-clock metrics: %v", err)
				}
			}
			if row.RetainedDurationMS != 0 {
				conn, err := db.Pool().Take(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				for _, statement := range []string{
					`UPDATE sessions SET start_ms = 2, end_ms = 1 WHERE session_id = ?`,
					`UPDATE session_metrics SET output_hash = NULL WHERE session_id = ?`,
				} {
					if err := sqlitex.ExecuteTransient(conn, statement, &sqlitex.ExecOptions{Args: []any{string(sid)}}); err != nil {
						db.Pool().Put(conn)
						t.Fatal(err)
					}
				}
				if row.NoMetrics {
					if err := sqlitex.ExecuteTransient(conn, `DELETE FROM session_metrics WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{string(sid)}}); err != nil {
						db.Pool().Put(conn)
						t.Fatal(err)
					}
				}
				err = sqlitex.ExecuteTransient(conn, `UPDATE session_captured_stats SET seed_json = NULL, duration_ms = ?, tokens_out = ? WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{row.RetainedDurationMS, row.RetainedOutputTokens, string(sid)}})
				db.Pool().Put(conn)
				if err != nil {
					t.Fatal(err)
				}
				pipeline = durationPipeline(t, db, source, output, true)
				computed, current, err := metrics.NewEngineWithModels(db, db).EnsureSessionMetrics(t.Context(), sid)
				if err != nil || !computed || !current {
					t.Fatalf("metrics did not retain uncomputed stats: computed=%v current=%v err=%v", computed, current, err)
				}
				if err := metrics.NewClassifierAnnotator(db, db).Annotate(t.Context(), sid); err != nil {
					t.Fatalf("classify retained metrics: %v", err)
				}
				input, err = db.ReadMetricInput(t.Context(), sid, true)
				if err != nil || input == nil {
					t.Fatalf("read retained stats: %v", err)
				}
				if input.Existing == nil || input.Existing.OutputTokens == nil || *input.Existing.OutputTokens != row.RetainedOutputTokens {
					t.Errorf("retained output tokens not reflected in metrics: %+v", input.Existing)
				}
			}
			if !metrics.MetricsCurrentForInput(input) {
				t.Errorf("reread metrics do not prove captured input current: %+v", input.Existing)
			}
			if input.Existing == nil || input.Existing.DurationMinutes == nil || *input.Existing.DurationMinutes != float64(row.DurationMS)/60000 {
				t.Errorf("reread duration does not equal %d milliseconds: %+v", row.DurationMS, input.Existing)
			}
			state, err := db.GetAnnotationRunState(t.Context(), sid)
			if err != nil || state == nil {
				t.Errorf("classifier run state missing: %v", err)
			} else if input.Existing == nil || input.Existing.OutputHash == nil || state.MetricsOutputHash != *input.Existing.OutputHash {
				t.Errorf("classifier run state does not cover the persisted metrics")
			}
			annotations, err := db.GetAnnotationsForSession(t.Context(), string(sid))
			if err != nil || len(annotations) == 0 {
				t.Errorf("annotations missing: %v", err)
			}
			second, err := pipeline.Run(t.Context())
			if err != nil || second.Summary.Computed != 0 {
				t.Fatalf("second harvest recomputed metrics: %+v %v", second, err)
			}
			afterState, err := db.GetAnnotationRunState(t.Context(), sid)
			if err != nil || !reflect.DeepEqual(state, afterState) {
				t.Errorf("second harvest changed classification state: %v", err)
			}
			afterAnnotations, err := db.GetAnnotationsForSession(t.Context(), string(sid))
			if err != nil || !reflect.DeepEqual(annotations, afterAnnotations) {
				t.Errorf("second harvest changed annotations: %v", err)
			}
		})
	}
	if err := testutil.RequireFixtureNames("metrics_duration_roundtrip.yaml", "cases", manifest.RequiredNames, seen); err != nil {
		t.Fatal(err)
	}
}

func durationPipeline(tb testing.TB, db *store.Store, source, output string, storedOnly bool) *ingest.Pipeline {
	tb.Helper()
	fs := &ingest.OSFileSystem{}
	cfg := ingest.PipelineConfig{
		Sources: map[ingest.Harness]ingest.SourceConfig{
			ingest.HarnessClaudeCode: {Enabled: true, Paths: []ingest.ResolvedPath{ingest.ResolvedPath(source)}},
		},
		OutputDir: ingest.ResolvedPath(output),
	}
	if storedOnly {
		cfg.SessionFilter = func(ingest.DiscoveredSession) bool { return false }
	}
	options := []ingest.PipelineOption{
		ingest.WithStore(db), ingest.WithMetricsStore(db), ingest.WithSalt(db.InstallationSalt()),
		ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})),
		ingest.WithLogger(db), ingest.WithIndexLogger(db),
	}
	options = append(options,
		ingest.WithAnalyzer(metrics.NewEngineWithModels(db, db)),
		ingest.WithClassifier(metrics.NewClassifierAnnotator(db, db)))
	pipeline, err := ingest.NewPipeline(fs, testutil.NoGitResolver(), ingest.DefaultAdapterRegistry, cfg, options...)
	if err != nil {
		tb.Fatalf("build duration pipeline: %v", err)
	}
	return pipeline
}

func durationStore(tb testing.TB, dbPath, output string) *store.Store {
	tb.Helper()
	options, err := store.ManagedGenerationOptions(output)
	if err != nil {
		tb.Fatalf("managed-generation options: %v", err)
	}
	db, err := store.Open(dbPath, options...)
	if err != nil {
		tb.Fatalf("open managed-generation store: %v", err)
	}
	return db
}

// durationCorpus writes one deterministic JSONL file per session.
// The text is synthetic and carries no user data.
func durationCorpus(tb testing.TB, root string, entries int, spanNS int64) {
	tb.Helper()
	filler := strings.Repeat("synthetic transcript text for the managed refresh measurement. ", 12)
	workspace := filepath.Join(root, "-workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		tb.Fatal(err)
	}
	const id = "bec00000-0000-4000-8000-000000000001"
	path := filepath.Join(workspace, id+".jsonl")
	content := durationTranscript(id, entries, filler, spanNS)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		tb.Fatal(err)
	}
	stamp := time.Unix(1_700_000_000, 0)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		tb.Fatal(err)
	}
}

func durationTranscript(sessionID string, entries int, filler string, spanNS int64) string {
	var content strings.Builder
	for n := range entries {
		atTime := time.Unix(1_700_000_000+int64(n), 0)
		if spanNS > 0 {
			atTime = time.Unix(1_700_000_000, int64(n)*spanNS)
		}
		at := atTime.UTC().Format(time.RFC3339Nano)
		timestamp := fmt.Sprintf(`,"timestamp":"%s"`, at)
		if spanNS < 0 {
			timestamp = ""
		}
		if n%2 == 0 {
			fmt.Fprintf(&content, `{"type":"user","sessionId":"%s","uuid":"u%d","cwd":"/work/project"%s,"message":{"role":"user","content":"prompt %d: %s"}}`+"\n", sessionID, n, timestamp, n, filler)
			continue
		}
		usage := `,"usage":{"input_tokens":120,"output_tokens":80}`
		if spanNS < 0 {
			usage = ""
		}
		fmt.Fprintf(&content, `{"type":"assistant","sessionId":"%s","uuid":"a%d","parentUuid":"u%d","cwd":"/work/project"%s,"message":{"role":"assistant","model":"claude-synthetic","content":[{"type":"text","text":"reply %d: %s"}]%s}}`+"\n", sessionID, n, n-1, timestamp, n, filler, usage)
	}
	return content.String()
}

// durationDowngradeAdapter sets every stored session row's adapter
// revision below the current target, as an older build would have left it.
func durationDowngradeAdapter(tb testing.TB, db *store.Store, stored int) {
	tb.Helper()
	conn, err := db.Pool().Take(tb.Context())
	if err != nil {
		tb.Fatal(err)
	}
	defer db.Pool().Put(conn)
	if err := sqlitex.Execute(conn, `UPDATE sessions SET adapter_version = ?`, &sqlitex.ExecOptions{Args: []any{stored}}); err != nil {
		tb.Fatalf("downgrade session adapter rows: %v", err)
	}
}

// durationStampMetadata rewrites every managed metadata file's adapter
// revision, forces a context change, and recomputes the metadata hash. A
// stamp-only change reproduces the active generation, so the unchanged proof
// would settle it and install nothing; the forced project name fails the proof,
// so the run parses and installs an observable new generation. It returns the
// file count.
func durationStampMetadata(tb testing.TB, output string, stored int, projectName string) int {
	tb.Helper()
	files, err := durationMetadataFiles(output)
	if err != nil {
		tb.Fatal(err)
	}
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			tb.Fatal(err)
		}
		var meta schema.UnifiedMetadata
		if err := json.Unmarshal(data, &meta); err != nil {
			tb.Fatalf("decode managed metadata: %v", err)
		}
		meta.AdapterVersion = &stored
		if projectName != "" {
			meta.Project.Name = projectName
		}
		meta.MetadataHash = schema.ComputeMetadataHash(&meta)
		encoded, err := json.Marshal(&meta)
		if err != nil {
			tb.Fatal(err)
		}
		temp, err := os.CreateTemp(filepath.Dir(path), ".managed-refresh-stamp-*")
		if err != nil {
			tb.Fatal(err)
		}
		tempName := temp.Name()
		if _, err := temp.Write(encoded); err != nil {
			_ = temp.Close()
			_ = os.Remove(tempName)
			tb.Fatal(err)
		}
		if err := temp.Close(); err != nil {
			_ = os.Remove(tempName)
			tb.Fatal(err)
		}
		if err := os.Chmod(tempName, 0o600); err != nil {
			_ = os.Remove(tempName)
			tb.Fatal(err)
		}
		if err := os.Rename(tempName, path); err != nil {
			_ = os.Remove(tempName)
			tb.Fatal(err)
		}
	}
	return len(files)
}

func durationMetadataFiles(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(d.Name(), defaults.MetadataSuffix) {
			files = append(files, path)
		}
		return nil
	})
	sort.Strings(files)
	return files, err
}
