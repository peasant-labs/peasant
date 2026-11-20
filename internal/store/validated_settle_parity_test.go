package store_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/validated_settle_parity.yaml
var validatedSettleParityYAML []byte

// parityStatsStamp pins the settle's stats stamp in both forks.
const parityStatsStamp = 1_800_000_000_000

const paritySession = "abc00000-0000-4000-8000-000000000101"

// parityTranscript is a synthetic Claude session; it carries no user data.
const parityTranscript = `{"type":"user","sessionId":"abc00000-0000-4000-8000-000000000101","uuid":"u1","cwd":"/work/project","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"synthetic prompt"}}
{"type":"assistant","sessionId":"abc00000-0000-4000-8000-000000000101","uuid":"a1","parentUuid":"u1","cwd":"/work/project","timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","model":"claude-synthetic","content":[{"type":"text","text":"synthetic reply"}],"usage":{"input_tokens":12,"output_tokens":8}}}
{"type":"user","sessionId":"abc00000-0000-4000-8000-000000000101","uuid":"u2","parentUuid":"a1","cwd":"/work/project","timestamp":"2026-01-01T00:00:02Z","message":{"role":"user","content":"second synthetic prompt"}}
`

// paritySettleStore settles through the store with the stats stamp pinned,
// and, when the pipeline supplies no candidate, validates with the real
// Claude metadata half and carries the proven answer into the settle.
type paritySettleStore struct {
	*store.Store
	indexer *ingest.ClaudeIndexer
	output  string

	mu         sync.Mutex
	validation *ingest.ManagedGenerationValidationResult
	validated  *ingest.ValidatedContext
	settled    int
	settleErr  error
}

var _ ingest.ManagedGenerationValidator = (*paritySettleStore)(nil)

func (s *paritySettleStore) ValidateManagedGenerationUnchanged(ctx context.Context, request ingest.ManagedGenerationValidationRequest) (ingest.ManagedGenerationValidationResult, error) {
	if request.Candidate == nil {
		transcript, err := parityManagedFile(s.output, string(request.SessionID)+"--transcript.jsonl")
		if err != nil {
			return ingest.ManagedGenerationValidationResult{}, err
		}
		recorded := request.Metadata
		candidate, err := s.indexer.BuildGenerationMetadata(ctx, ingest.GenerationBuildInput{
			Session:            ingest.DiscoveredSession{SessionID: request.SessionID, Harness: ingest.HarnessClaudeCode, SourceFormat: ingest.SourceFormatJSONL},
			GenerationID:       "candidate-only",
			RecordedMetadata:   &recorded,
			CapturedTranscript: transcript,
		})
		if err == nil {
			request.Candidate = &candidate
		}
	}
	result, err := s.Store.ValidateManagedGenerationUnchanged(ctx, request)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		s.validation = &result
		if result.Outcome == ingest.ValidationProven {
			s.validated = &ingest.ValidatedContext{GenerationID: request.GenerationID, Candidate: *request.Candidate}
		}
	}
	return result, err
}

func (s *paritySettleStore) SettleCapturedGenerationUnchanged(ctx context.Context, request ingest.ManagedGenerationUnchangedProof) (ingest.ManagedGenerationUnchangedSettleOutcome, error) {
	s.mu.Lock()
	if request.Validated == nil {
		request.Validated = s.validated
	}
	s.mu.Unlock()
	outcome, err := s.Store.SettleCapturedGenerationUnchangedAtForTest(ctx, request, parityStatsStamp)
	s.mu.Lock()
	s.settled++
	s.settleErr = err
	s.mu.Unlock()
	return outcome, err
}

func parityManagedFile(output, name string) ([]byte, error) {
	var found string
	err := filepath.WalkDir(output, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && entry.Name() == name {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if found == "" {
		return nil, errors.New("no managed file " + name)
	}
	return os.ReadFile(found)
}

func runParityPipeline(t *testing.T, db *store.Store, wrapped interface {
	ingest.SessionStore
	ingest.MetricsStore
}, source, output string, validator ingest.ManagedGenerationValidator) *ingest.PipelineResult {
	t.Helper()
	fs := &ingest.OSFileSystem{}
	cfg := ingest.PipelineConfig{
		Sources: map[ingest.Harness]ingest.SourceConfig{
			ingest.HarnessClaudeCode: {Paths: []ingest.ResolvedPath{ingest.ResolvedPath(source)}, Enabled: true},
		},
		OutputDir: ingest.ResolvedPath(output),
		Validator: validator,
	}
	pipeline, err := ingest.NewPipeline(fs, testutil.NoGitResolver(), ingest.DefaultAdapterRegistry, cfg,
		ingest.WithStore(wrapped), ingest.WithMetricsStore(wrapped), ingest.WithSalt(db.InstallationSalt()),
		ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})),
		ingest.WithArenaSizeBytes(64<<20))
	if err != nil {
		t.Fatal(err)
	}
	result, err := pipeline.Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func openParityStore(t *testing.T, path, output string) *store.Store {
	t.Helper()
	options, err := store.ManagedGenerationOptions(output)
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(path, options...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func parityExec(t *testing.T, db *store.Store, query string, args ...any) {
	t.Helper()
	conn, err := db.PoolForTest().Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer db.PoolForTest().Put(conn)
	if err := sqlitex.Execute(conn, query, &sqlitex.ExecOptions{Args: args}); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

// parityForgeMetadataHash forges the stored metadata_hash of the session's
// active generation with the immutability trigger lifted for one update and
// restored from its own stored definition.
func parityForgeMetadataHash(t *testing.T, db *store.Store) {
	t.Helper()
	conn, err := db.PoolForTest().Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer db.PoolForTest().Put(conn)
	var trigger string
	if err := sqlitex.Execute(conn, `SELECT sql FROM sqlite_schema WHERE type='trigger' AND name='session_generations_immutable'`, &sqlitex.ExecOptions{ResultFunc: func(st *sqlite.Stmt) error {
		trigger = st.ColumnText(0)
		return nil
	}}); err != nil || trigger == "" {
		t.Fatalf("read the immutability trigger: %q, %v", trigger, err)
	}
	for _, step := range []struct {
		query string
		args  []any
	}{
		{`DROP TRIGGER session_generations_immutable`, nil},
		{`UPDATE session_generations SET metadata_hash=? WHERE session_id=? AND generation_id=(SELECT active_generation_id FROM sessions WHERE session_id=?)`, []any{strings.Repeat("f", 64), paritySession, paritySession}},
		{trigger, nil},
	} {
		if err := sqlitex.Execute(conn, step.query, &sqlitex.ExecOptions{Args: step.args}); err != nil {
			t.Fatalf("forge the metadata hash: %s: %v", step.query, err)
		}
	}
}

// parityDump renders every cell of every table as table.column[row]=value,
// rows ordered by their full rendering, so two stores compare cell by cell.
func parityDump(t *testing.T, path string) map[string]string {
	t.Helper()
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var tables []string
	if err := sqlitex.Execute(conn, `SELECT name FROM sqlite_schema WHERE type='table' AND name NOT LIKE 'sqlite_%' AND sql NOT LIKE 'CREATE VIRTUAL TABLE%' ORDER BY name`, &sqlitex.ExecOptions{ResultFunc: func(st *sqlite.Stmt) error {
		tables = append(tables, st.ColumnText(0))
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	cells := map[string]string{}
	for _, table := range tables {
		var rows [][]string
		var columns []string
		if err := sqlitex.Execute(conn, fmt.Sprintf(`SELECT * FROM "%s"`, table), &sqlitex.ExecOptions{ResultFunc: func(st *sqlite.Stmt) error {
			columns = columns[:0]
			var row []string
			for i := range st.ColumnCount() {
				columns = append(columns, st.ColumnName(i))
				row = append(row, fmt.Sprintf("%d:%s", st.ColumnType(i), st.ColumnText(i)))
			}
			rows = append(rows, row)
			return nil
		}}); err != nil {
			t.Fatalf("dump %s: %v", table, err)
		}
		sort.Slice(rows, func(i, j int) bool { return strings.Join(rows[i], "\x00") < strings.Join(rows[j], "\x00") })
		cells[table+".rows"] = fmt.Sprint(len(rows))
		for r, row := range rows {
			for c, value := range row {
				cells[fmt.Sprintf("%s.%s[%d]", table, columns[c], r)] = value
			}
		}
	}
	return cells
}

// parityDifferences lists the table.column names whose cells differ, leaving
// out the ignored columns.
func parityDifferences(left, right map[string]string, ignored []string) []string {
	columns := map[string]bool{}
	for key, value := range left {
		if right[key] != value {
			columns[strings.SplitN(key, "[", 2)[0]] = true
		}
	}
	for key := range right {
		if _, ok := left[key]; !ok {
			columns[strings.SplitN(key, "[", 2)[0]] = true
		}
	}
	var out []string
	for column := range columns {
		if !slices.Contains(ignored, column) {
			out = append(out, column)
		}
	}
	sort.Strings(out)
	return out
}

// TestValidatedSettleWritesLikeTheProvenSettle forks one Claude store after an
// adapter bump: fork 1 runs the proven settle; fork 2 forges the stored
// metadata hash, validates, and runs the validated settle. Every table must
// match except the forged hash, and removing either settle write must break
// the match.
func TestValidatedSettleWritesLikeTheProvenSettle(t *testing.T) {
	var fixture struct {
		AllowedDifferences []string `yaml:"allowedDifferences"`
		Mutations          []string `yaml:"mutations"`
		DrainClockColumns  []string `yaml:"drainClockColumns"`
	}
	if err := yaml.Unmarshal(validatedSettleParityYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.AllowedDifferences) == 0 || len(fixture.Mutations) == 0 {
		t.Fatal("the parity fixture names no allowed difference or no mutation")
	}
	root := t.TempDir()
	source, output, empty := filepath.Join(root, "source"), filepath.Join(root, "managed"), filepath.Join(root, "empty")
	for _, dir := range []string{filepath.Join(source, "-workspace"), empty} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(source, "-workspace", paritySession+".jsonl"), []byte(parityTranscript), 0o600); err != nil {
		t.Fatal(err)
	}
	seedPath := filepath.Join(root, "seed", "peasant.db")
	if err := os.MkdirAll(filepath.Dir(seedPath), 0o700); err != nil {
		t.Fatal(err)
	}
	seed := openParityStore(t, seedPath, output)
	if first := runParityPipeline(t, seed, seed, source, output, nil); first.Summary.Indexed != 1 {
		t.Fatalf("the seed import indexed %d sessions, want 1", first.Summary.Indexed)
	}
	target := ingest.GenerationTargets(ingest.HarvesterVersionRegistry)[ingest.HarnessClaudeCode].AdapterVersion - 1
	parityExec(t, seed, `UPDATE sessions SET adapter_version = ?`, target)
	// Clear the session's input-count mirror in the seed, in every fork
	// alike, so the settle's mirror write shows in the dump.
	parityExec(t, seed, `UPDATE sessions SET input_submission_count = NULL WHERE session_id = ?`, paritySession)
	parityDowngradeSidecars(t, output, target)
	outputBackup := filepath.Join(root, "managed-seed")
	if out, err := exec.Command("cp", "-a", output, outputBackup).CombinedOutput(); err != nil {
		t.Fatalf("snapshot the managed tree: %v: %s", err, out)
	}
	runs := append([]string{"proven", "validated"}, fixture.Mutations...)
	var forks []string
	for _, run := range runs {
		forks = append(forks, filepath.Join(root, run, "peasant.db"))
	}
	if _, err := storetest.ForkStore(t, seed, seedPath, forks...); err != nil {
		t.Fatal(err)
	}
	dumps := map[string]map[string]string{}
	for i, run := range runs {
		// Every fork refreshes against the same managed tree.
		if err := os.RemoveAll(output); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("cp", "-a", outputBackup, output).CombinedOutput(); err != nil {
			t.Fatalf("restore the managed tree: %v: %s", err, out)
		}
		t.Run(run, func(t *testing.T) {
			db := openParityStore(t, forks[i], output)
			if run != "proven" {
				parityForgeMetadataHash(t, db)
			}
			if slices.Contains(fixture.Mutations, run) {
				store.SetValidatedSettleMutationForTest(t, run)
			}
			wrapped := &paritySettleStore{Store: db, indexer: ingest.NewClaudeIndexer(&ingest.OSFileSystem{}), output: output}
			runParityPipeline(t, db, wrapped, empty, output, wrapped)
			if wrapped.settled != 1 || wrapped.settleErr != nil {
				t.Fatalf("settles %d, error %v; want one settle", wrapped.settled, wrapped.settleErr)
			}
			if run == "proven" && wrapped.validation != nil {
				t.Fatalf("the proven fork asked the validator: %+v", wrapped.validation)
			}
			if run != "proven" && (wrapped.validation == nil || wrapped.validation.Outcome != ingest.ValidationProven || wrapped.validated == nil) {
				t.Fatalf("the forged fork did not validate: %+v", wrapped.validation)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			dumps[run] = parityDump(t, forks[i])
		})
	}
	if t.Failed() {
		return
	}
	if got := parityDifferences(dumps["proven"], dumps["validated"], fixture.DrainClockColumns); !slices.Equal(got, fixture.AllowedDifferences) {
		t.Fatalf("the validated settle wrote differently from the proven settle: differing columns %v, want only %v", got, fixture.AllowedDifferences)
	}
	for _, mutation := range fixture.Mutations {
		if got := parityDifferences(dumps["proven"], dumps[mutation], fixture.DrainClockColumns); slices.Equal(got, fixture.AllowedDifferences) {
			t.Errorf("mutation %s left the stores in parity; the parity test cannot see that write", mutation)
		} else {
			t.Logf("mutation %s differs in %v", mutation, got)
		}
	}
}

// parityDowngradeSidecars moves every managed metadata sidecar's adapter
// revision to stored, re-sealing its hash, as an adapter bump finds it.
func parityDowngradeSidecars(t *testing.T, output string, stored int) {
	t.Helper()
	err := filepath.WalkDir(output, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(entry.Name(), defaults.MetadataSuffix) {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var metadata schema.UnifiedMetadata
		if err := json.Unmarshal(data, &metadata); err != nil {
			return err
		}
		metadata.AdapterVersion = &stored
		metadata.MetadataHash = schema.ComputeMetadataHash(&metadata)
		encoded, err := json.Marshal(&metadata)
		if err != nil {
			return err
		}
		return os.WriteFile(path, encoded, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
}
