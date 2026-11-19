package ingest_test

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/metrics"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/metrics_duration_roundtrip.yaml
var metricsDurationRoundtripYAML []byte

func TestManagedMetricsDurationRoundtrip(t *testing.T) {
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name    string `yaml:"name"`
			Entries int    `yaml:"entries"`
			Refresh bool   `yaml:"refresh"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(metricsDurationRoundtripYAML, &fixture); err != nil {
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
			durationCorpus(t, source, 1, row.Entries)
			pipeline := durationPipeline(t, db, source, output, nil, true)
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
				result, err = pipeline.Run(t.Context())
				if err != nil {
					t.Fatalf("refresh: %v", err)
				}
				input, err = db.ReadMetricInput(t.Context(), sid, true)
				if err != nil || input == nil || input.GenerationID == old {
					t.Fatalf("generation not installed: %+v %v", result, err)
				}
			}
			if !metrics.MetricsCurrentForInput(input) {
				t.Errorf("reread metrics do not prove captured input current: %+v", input.Existing)
			}
			state, err := db.GetAnnotationRunState(t.Context(), sid)
			if err != nil || state == nil {
				t.Errorf("classifier run state missing: %v", err)
			}
			annotations, err := db.GetAnnotationsForSession(t.Context(), string(sid))
			if err != nil || len(annotations) == 0 {
				t.Errorf("annotations missing: %v", err)
			}
		})
	}
	if err := testutil.RequireFixtureNames("metrics_duration_roundtrip.yaml", "cases", fixture.RequiredNames, seen); err != nil {
		t.Fatal(err)
	}
}

func durationPipeline(tb testing.TB, db *store.Store, source, output string, profiler *ingest.IndexProfiler, classify bool) *ingest.Pipeline {
	tb.Helper()
	fs := &ingest.OSFileSystem{}
	cfg := ingest.PipelineConfig{
		Sources: map[ingest.Harness]ingest.SourceConfig{
			ingest.HarnessClaudeCode: {Enabled: true, Paths: []ingest.ResolvedPath{ingest.ResolvedPath(source)}},
		},
		OutputDir:     ingest.ResolvedPath(output),
		IndexProfiler: profiler,
	}
	options := []ingest.PipelineOption{
		ingest.WithStore(db), ingest.WithMetricsStore(db), ingest.WithSalt(db.InstallationSalt()),
		ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})),
		ingest.WithLogger(db), ingest.WithIndexLogger(db),
	}
	if classify {
		options = append(options,
			ingest.WithAnalyzer(metrics.NewEngineWithModels(db, db)),
			ingest.WithClassifier(metrics.NewClassifierAnnotator(db, db)))
	}
	pipeline, err := ingest.NewPipeline(fs, testutil.NoGitResolver(), ingest.DefaultAdapterRegistry, cfg, options...)
	if err != nil {
		tb.Fatalf("build refresh pipeline: %v", err)
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
func durationCorpus(tb testing.TB, root string, sessions, entries int) int64 {
	tb.Helper()
	filler := strings.Repeat("synthetic transcript text for the managed refresh measurement. ", 12)
	var total int64
	workspace := filepath.Join(root, "-workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		tb.Fatal(err)
	}
	for n := range sessions {
		id := fmt.Sprintf("bec00000-0000-4000-8000-%012d", n+1)
		path := filepath.Join(workspace, id+".jsonl")
		content := durationTranscript(id, entries, filler)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			tb.Fatal(err)
		}
		stamp := time.Unix(1_700_000_000+int64(n), 0)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			tb.Fatal(err)
		}
		total += int64(len(content))
	}
	return total
}

func durationTranscript(sessionID string, entries int, filler string) string {
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
// stamp-only change reproduces the active generation, so the integrated
// metadata-only refresh would report unchanged and install nothing; the forced
// project name is what makes the generation install observable. It returns the
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
