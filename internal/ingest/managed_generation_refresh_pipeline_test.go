package ingest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

const managedRefreshPipelineForcedProject = "managed refresh pipeline project"

// TestManagedGenerationMetadataRefreshRunsThroughThePipeline is the production
// path for a metadata-only adapter bump on a managed generation: the worker
// extracts the metadata from the stored transcript, the pair install and the
// managed mirror commit, and the index stage installs a new generation from the
// refreshed capture without parsing the transcript. A following transcript
// change takes the ordinary native parse again.
func TestManagedGenerationMetadataRefreshRunsThroughThePipeline(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	output := filepath.Join(root, "managed")
	mustMkdir(t, source)
	sessionIDs := writeManagedRefreshPipelineCorpus(t, source, 2, 24)

	db := openManagedRefreshPipelineStore(t, filepath.Join(root, "db"), output)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close the managed store: %v", err)
		}
	})

	first := runManagedRefreshPipeline(t, db, source, output)
	if first.Summary.New != len(sessionIDs) || first.Summary.Errors != 0 || first.Summary.Indexed != len(sessionIDs) {
		t.Fatalf("initial import summary = %+v, want %d new, indexed and no errors", first.Summary, len(sessionIDs))
	}
	initial := make(map[ingest.SessionID]string, len(sessionIDs))
	for _, sid := range sessionIDs {
		initial[sid] = managedRefreshPipelineActiveGeneration(t, db, sid)
	}
	initialEntries := managedRefreshPipelineRows(t, db, `SELECT partition_id || ':' || entry_index || ':' || source_entry_ref || ':' || body_digest FROM session_generation_entries WHERE session_id = ? AND generation_id = ? ORDER BY partition_id, entry_index`, sessionIDs[0], initial[sessionIDs[0]])
	initialAliases := managedRefreshPipelineRows(t, db, `SELECT native_key || ':' || source_entry_ref FROM session_projection_aliases WHERE session_id = ? AND generation_id = ? ORDER BY native_key`, sessionIDs[0], initial[sessionIDs[0]])
	initialSections := managedRefreshPipelineRows(t, db, `SELECT partition_id || ':' || COALESCE(earlier_state, '') FROM session_projection_sections WHERE session_id = ? AND generation_id = ? ORDER BY partition_id`, sessionIDs[0], initial[sessionIDs[0]])
	if len(initialEntries) == 0 {
		t.Fatal("the initial managed generation stored no entries")
	}

	// A version-only bump with a forced metadata change: the stored adapter
	// revision moves below the managed target and the project name moves in the
	// stored metadata. The transcript bytes stay untouched.
	storedAdapter := ingest.GenerationTargets(ingest.HarvesterVersionRegistry)[ingest.HarnessClaudeCode].AdapterVersion - 1
	if storedAdapter < 1 {
		t.Fatalf("claude managed adapter target %d cannot be downgraded", storedAdapter+1)
	}
	managedRefreshPipelineDowngrade(t, db, storedAdapter)
	if changed := managedRefreshPipelineChangeMetadata(t, output, storedAdapter, managedRefreshPipelineForcedProject); changed != len(sessionIDs) {
		t.Fatalf("changed %d metadata files, want %d", changed, len(sessionIDs))
	}

	emptySource := filepath.Join(root, "empty-source")
	mustMkdir(t, emptySource)
	refreshed := runManagedRefreshPipeline(t, db, emptySource, output)
	if refreshed.Summary.Errors != 0 || refreshed.Summary.New != 0 || refreshed.Summary.Unchanged != 0 {
		t.Fatalf("metadata-only refresh summary = %+v, want updated only and no errors", refreshed.Summary)
	}
	if refreshed.Summary.Updated != len(sessionIDs) {
		t.Fatalf("metadata-only refresh updated %d sessions, want %d", refreshed.Summary.Updated, len(sessionIDs))
	}
	if refreshed.Summary.Indexed != 0 {
		t.Fatalf("metadata-only refresh indexed %d sessions, want 0: the managed generation must not be parsed", refreshed.Summary.Indexed)
	}
	if refreshed.Census == nil || refreshed.Census.Sessions() != len(sessionIDs) {
		t.Fatalf("refresh census = %+v, want %d sessions", refreshed.Census, len(sessionIDs))
	}
	for _, row := range refreshed.Census.Rows {
		if row.Reason != ingest.OutcomeReasonRefreshAdapterBehind || row.Lane != ingest.RefreshLaneStoredMetadata {
			t.Fatalf("refresh census row = %+v, want adapter_behind on the stored-metadata lane", row)
		}
	}
	if len(refreshed.IndexLog) != len(sessionIDs) {
		t.Fatalf("metadata-only refresh produced %d index rows, want one per session", len(refreshed.IndexLog))
	}
	for _, entry := range refreshed.IndexLog {
		if entry.Outcome != ingest.IndexOutcomeSkipped {
			t.Fatalf("refresh index outcome for session %s = %q, want skipped", entry.SessionID, entry.Outcome)
		}
		if entry.ParseDuration != nil && *entry.ParseDuration > 0 {
			t.Fatalf("refresh parsed %v for session %s, want no parse", *entry.ParseDuration, entry.SessionID)
		}
		if entry.WriteDuration == nil || *entry.WriteDuration <= 0 {
			t.Fatalf("refresh reported no generation write time for session %s", entry.SessionID)
		}
	}
	for _, sid := range sessionIDs {
		active := managedRefreshPipelineActiveGeneration(t, db, sid)
		if active == initial[sid] {
			t.Fatalf("session %s kept the source generation active; want a refreshed generation", sid)
		}
		if name := managedRefreshPipelineProjectName(t, db, sid, active); name != managedRefreshPipelineForcedProject {
			t.Fatalf("refreshed generation of session %s records project %q, want %q", sid, name, managedRefreshPipelineForcedProject)
		}
	}
	// Entries, aliases and sections are copied by reference, not reparsed.
	refreshedEntries := managedRefreshPipelineRows(t, db, `SELECT partition_id || ':' || entry_index || ':' || source_entry_ref || ':' || body_digest FROM session_generation_entries WHERE session_id = ? AND generation_id = ? ORDER BY partition_id, entry_index`, sessionIDs[0], managedRefreshPipelineActiveGeneration(t, db, sessionIDs[0]))
	refreshedAliases := managedRefreshPipelineRows(t, db, `SELECT native_key || ':' || source_entry_ref FROM session_projection_aliases WHERE session_id = ? AND generation_id = ? ORDER BY native_key`, sessionIDs[0], managedRefreshPipelineActiveGeneration(t, db, sessionIDs[0]))
	refreshedSections := managedRefreshPipelineRows(t, db, `SELECT partition_id || ':' || COALESCE(earlier_state, '') FROM session_projection_sections WHERE session_id = ? AND generation_id = ? ORDER BY partition_id`, sessionIDs[0], managedRefreshPipelineActiveGeneration(t, db, sessionIDs[0]))
	assertManagedRefreshRowsEqual(t, "entries", initialEntries, refreshedEntries)
	assertManagedRefreshRowsEqual(t, "aliases", initialAliases, refreshedAliases)
	assertManagedRefreshRowsEqual(t, "sections", initialSections, refreshedSections)
	// The source generation is flagged for sweep and the existing reclaim pass
	// removes it without touching the refreshed authority.
	if pending := managedRefreshPipelineInt(t, db, `SELECT content_sweep_pending FROM sessions WHERE session_id = ?`, sessionIDs[0]); pending != 1 {
		t.Fatalf("refreshed session sweep flag = %d, want 1", pending)
	}
	refreshedAuthority := managedRefreshPipelineActiveGeneration(t, db, sessionIDs[0])
	if err := db.SweepSessionForHarvest(t.Context(), sessionIDs[0]); err != nil {
		t.Fatalf("sweep the superseded generation: %v", err)
	}
	if rows := managedRefreshPipelineInt(t, db, `SELECT count(*) FROM session_generations WHERE session_id = ? AND generation_id = ?`, sessionIDs[0], initial[sessionIDs[0]]); rows != 0 {
		t.Fatalf("superseded generation rows after sweep = %d, want 0", rows)
	}
	if active := managedRefreshPipelineActiveGeneration(t, db, sessionIDs[0]); active != refreshedAuthority {
		t.Fatalf("active generation after sweep = %q, want the refreshed authority %q", active, refreshedAuthority)
	}

	// A following transcript change re-enters the native parse path and writes
	// a new generation from the changed bytes.
	appendManagedRefreshPipelineRecord(t, filepath.Join(source, "-workspace", string(sessionIDs[0])+".jsonl"), string(sessionIDs[0]))
	changed := runManagedRefreshPipeline(t, db, source, output)
	if changed.Summary.Errors != 0 || changed.Summary.Indexed != 1 {
		t.Fatalf("changed-source run summary = %+v, want one re-indexed session and no errors", changed.Summary)
	}
	if active := managedRefreshPipelineActiveGeneration(t, db, sessionIDs[0]); active == refreshedAuthority {
		t.Fatal("a changed transcript left the old generation active; want a new parsed generation")
	}
	if len(changed.IndexLog) == 0 {
		t.Fatal("a changed transcript produced no index row")
	}
	parsed := false
	for _, entry := range changed.IndexLog {
		if entry.SessionID != sessionIDs[0] || entry.ParseDuration == nil {
			continue
		}
		if *entry.ParseDuration > 0 {
			parsed = true
		}
	}
	if !parsed {
		t.Fatal("a changed transcript was settled without a parse")
	}
}

// staleManagedRefreshStore is a real store whose metadata-only refresh reports
// a stale captured proof. It exercises the pipeline's fallback to the ordinary
// parse path; every other store method is the real implementation.
type staleManagedRefreshStore struct {
	*store.Store
}

func (s staleManagedRefreshStore) RefreshManagedGenerationWithoutParse(context.Context, ingest.ManagedGenerationRefreshRequest) (ingest.ManagedGenerationRefreshOutcome, error) {
	return ingest.ManagedGenerationRefreshOutcome{}, &ingest.StaleIndexWorkError{SessionID: ingest.SessionID("stale")}
}

// TestManagedGenerationRefreshStaleProofFallsBackToTheParse pins the fallback:
// a refused refresh changes no authority and the ordinary parse path still
// runs, so the session settles with the refreshed capture instead of silently
// keeping a stale generation.
func TestManagedGenerationRefreshStaleProofFallsBackToTheParse(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	output := filepath.Join(root, "managed")
	mustMkdir(t, source)
	sessionIDs := writeManagedRefreshPipelineCorpus(t, source, 1, 8)

	base := openManagedRefreshPipelineStore(t, filepath.Join(root, "db"), output)
	t.Cleanup(func() {
		if err := base.Close(); err != nil {
			t.Errorf("close the managed store: %v", err)
		}
	})
	first := runManagedRefreshPipeline(t, base, source, output)
	if first.Summary.New != 1 || first.Summary.Errors != 0 {
		t.Fatalf("initial import summary = %+v", first.Summary)
	}
	initial := managedRefreshPipelineActiveGeneration(t, base, sessionIDs[0])

	storedAdapter := ingest.GenerationTargets(ingest.HarvesterVersionRegistry)[ingest.HarnessClaudeCode].AdapterVersion - 1
	managedRefreshPipelineDowngrade(t, base, storedAdapter)
	if changed := managedRefreshPipelineChangeMetadata(t, output, storedAdapter, managedRefreshPipelineForcedProject); changed != 1 {
		t.Fatalf("changed %d metadata files, want 1", changed)
	}
	emptySource := filepath.Join(root, "empty-source")
	mustMkdir(t, emptySource)

	stale := staleManagedRefreshStore{Store: base}
	result := runManagedRefreshPipelineWithStore(t, stale, emptySource, output)
	if result.Summary.Errors != 0 || result.Summary.Updated != 1 {
		t.Fatalf("fallback summary = %+v, want one updated session and no errors", result.Summary)
	}
	if result.Summary.Indexed != 1 {
		t.Fatalf("fallback indexed %d sessions, want the ordinary parse path to run", result.Summary.Indexed)
	}
	active := managedRefreshPipelineActiveGeneration(t, base, sessionIDs[0])
	if active == initial {
		t.Fatal("fallback left the source generation active; the ordinary parse must install the refreshed capture")
	}
	if name := managedRefreshPipelineProjectName(t, base, sessionIDs[0], active); name != managedRefreshPipelineForcedProject {
		t.Fatalf("fallback generation project = %q, want the refreshed capture %q", name, managedRefreshPipelineForcedProject)
	}
}

func runManagedRefreshPipeline(t *testing.T, db *store.Store, source, output string) *ingest.PipelineResult {
	t.Helper()
	return runManagedRefreshPipelineWithStore(t, db, source, output)
}

func runManagedRefreshPipelineWithStore(t *testing.T, db managedRefreshPipelineStore, source, output string) *ingest.PipelineResult {
	t.Helper()
	cfg := makePipelineConfig(output)
	cfg.Sources = map[ingest.Harness]ingest.SourceConfig{
		ingest.HarnessClaudeCode: {Paths: []ingest.ResolvedPath{ingest.ResolvedPath(source)}, Enabled: true},
	}
	cfg.StalenessThreshold = 0
	fs := &ingest.OSFileSystem{}
	pipeline, err := newTestPipeline(fs, testutil.NoGitResolver(), ingest.DefaultAdapterRegistry, cfg,
		ingest.WithStore(db),
		ingest.WithMetricsStore(db),
		ingest.WithSalt(db.InstallationSalt()),
		ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})),
		ingest.WithLogger(db),
		ingest.WithIndexLogger(db),
	)
	if err != nil {
		t.Fatalf("build managed refresh pipeline: %v", err)
	}
	result, err := pipeline.Run(t.Context())
	if err != nil {
		t.Fatalf("run managed refresh pipeline: %v", err)
	}
	return result
}

type managedRefreshPipelineStore interface {
	ingest.SessionStore
	ingest.MetricsStore
	ingest.IngestLogger
	ingest.IndexLogger
	InstallationSalt() salt.Salt
}

func openManagedRefreshPipelineStore(t *testing.T, dbDir, output string) *store.Store {
	t.Helper()
	mustMkdir(t, dbDir)
	options, err := store.ManagedGenerationOptions(output)
	if err != nil {
		t.Fatalf("managed generation options: %v", err)
	}
	db, err := store.Open(filepath.Join(dbDir, "peasant.db"), options...)
	if err != nil {
		t.Fatalf("open managed generation store: %v", err)
	}
	return db
}

// writeManagedRefreshPipelineCorpus writes one deterministic synthetic Claude
// JSONL file per session. The text is synthetic and carries no user data.
func writeManagedRefreshPipelineCorpus(t *testing.T, root string, sessions, entries int) []ingest.SessionID {
	t.Helper()
	ids := make([]ingest.SessionID, 0, sessions)
	workspace := filepath.Join(root, "-workspace")
	mustMkdir(t, workspace)
	filler := strings.Repeat("synthetic transcript text for the managed refresh pipeline test. ", 6)
	for n := range sessions {
		id := ingest.SessionID(fmt.Sprintf("abc00000-0000-4000-8000-%012d", n+1))
		ids = append(ids, id)
		path := filepath.Join(workspace, string(id)+".jsonl")
		content := syntheticManagedRefreshPipelineTranscript(string(id), entries, filler)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		stamp := time.Unix(1_700_000_000+int64(n), 0)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	return ids
}

func syntheticManagedRefreshPipelineTranscript(sessionID string, entries int, filler string) string {
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

func appendManagedRefreshPipelineRecord(t *testing.T, path, sessionID string) {
	t.Helper()
	at := time.Now().UTC().Format(time.RFC3339)
	record := fmt.Sprintf(`{"type":"user","sessionId":"%s","uuid":"u-appended","cwd":"/work/project","timestamp":"%s","message":{"role":"user","content":"appended transcript change"}}`+"\n", sessionID, at)
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(record); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
}

func managedRefreshPipelineDowngrade(t *testing.T, db *store.Store, stored int) {
	t.Helper()
	conn, err := db.Pool().Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Pool().Put(conn)
	if err := sqlitex.Execute(conn, `UPDATE sessions SET adapter_version = ?`, &sqlitex.ExecOptions{Args: []any{stored}}); err != nil {
		t.Fatalf("downgrade session adapter rows: %v", err)
	}
}

func managedRefreshPipelineChangeMetadata(t *testing.T, output string, stored int, projectName string) int {
	t.Helper()
	var files []string
	if err := filepath.WalkDir(output, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if strings.HasSuffix(entry.Name(), defaults.MetadataSuffix) {
			files = append(files, path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var metadata schema.UnifiedMetadata
		if err := json.Unmarshal(data, &metadata); err != nil {
			t.Fatalf("decode managed metadata %s: %v", path, err)
		}
		metadata.AdapterVersion = &stored
		metadata.Project.Name = projectName
		metadata.MetadataHash = schema.ComputeMetadataHash(&metadata)
		encoded, err := json.Marshal(&metadata)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, encoded, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return len(files)
}

func managedRefreshPipelineActiveGeneration(t *testing.T, db *store.Store, sid ingest.SessionID) string {
	t.Helper()
	conn, err := db.Pool().Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Pool().Put(conn)
	var active string
	if err := sqlitex.ExecuteTransient(conn, `SELECT COALESCE(active_generation_id, '') FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sid)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			active = stmt.ColumnText(0)
			return nil
		},
	}); err != nil {
		t.Fatalf("read active generation for %s: %v", sid, err)
	}
	if active == "" {
		t.Fatalf("session %s has no active generation", sid)
	}
	return active
}

func managedRefreshPipelineProjectName(t *testing.T, db *store.Store, sid ingest.SessionID, generationID string) string {
	t.Helper()
	conn, err := db.Pool().Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Pool().Put(conn)
	var name string
	if err := sqlitex.ExecuteTransient(conn, `SELECT project_name FROM session_generations WHERE session_id = ? AND generation_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sid), generationID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			name = stmt.ColumnText(0)
			return nil
		},
	}); err != nil {
		t.Fatalf("read generation project name for %s: %v", sid, err)
	}
	return name
}

func managedRefreshPipelineRows(t *testing.T, db *store.Store, query string, args ...any) []string {
	t.Helper()
	conn, err := db.Pool().Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Pool().Put(conn)
	var rows []string
	if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
		Args: args,
		ResultFunc: func(stmt *sqlite.Stmt) error {
			rows = append(rows, stmt.ColumnText(0))
			return nil
		},
	}); err != nil {
		t.Fatalf("query generation rows: %v", err)
	}
	return rows
}

func managedRefreshPipelineInt(t *testing.T, db *store.Store, query string, args ...any) int64 {
	t.Helper()
	conn, err := db.Pool().Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Pool().Put(conn)
	var value int64
	if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
		Args: args,
		ResultFunc: func(stmt *sqlite.Stmt) error {
			value = stmt.ColumnInt64(0)
			return nil
		},
	}); err != nil {
		t.Fatalf("query generation count: %v", err)
	}
	return value
}

func assertManagedRefreshRowsEqual(t *testing.T, label string, before, after []string) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("%s row count changed: %d -> %d", label, len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("%s row %d changed: %q -> %q", label, i, before[i], after[i])
		}
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
}
