package ingest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/export"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// realRouteSeed is one imported store with its adapter revision moved behind
// the managed target, snapshotted so a second run starts from the same bytes.
type realRouteSeed struct {
	root, source, output, empty, dbDir string
	harness                            ingest.Harness
	db                                 *store.Store
}

// codexRouteFixture is the repository's synthetic Codex home.
const codexRouteFixture = "../e2e/testdata/codex-fixture"

// seedRealRoute imports the corpus write puts under source for harness, then
// moves the stored adapter revision behind the managed target.
func seedRealRoute(t *testing.T, harness ingest.Harness, write func(t *testing.T, source string)) *realRouteSeed {
	return seedRealRouteAt(t, harness, "", write)
}

// seedRealRouteAt is seedRealRoute discovering from the sub directory of the
// source the harness reads (Codex reads its home's sessions tree).
func seedRealRouteAt(t *testing.T, harness ingest.Harness, sub string, write func(t *testing.T, source string)) *realRouteSeed {
	t.Helper()
	root := t.TempDir()
	seed := &realRouteSeed{root: root, source: filepath.Join(root, "source"), output: filepath.Join(root, "managed"), empty: filepath.Join(root, "empty"), dbDir: filepath.Join(root, "db"), harness: harness}
	mustMkdir(t, seed.source)
	mustMkdir(t, seed.empty)
	write(t, seed.source)
	seed.db = openManagedRefreshPipelineStore(t, seed.dbDir, seed.output)
	first := runRealRoute(t, seed, seed.db, filepath.Join(seed.source, sub), realRouteRun{})
	if first.Summary.Errors != 0 || first.Summary.Indexed == 0 {
		t.Fatalf("the seed import: %+v", first.Summary)
	}
	target := ingest.GenerationTargets(ingest.HarvesterVersionRegistry)[harness].AdapterVersion - 1
	managedRefreshPipelineDowngrade(t, seed.db, target)
	return seed
}

// snapshot closes the store and copies its database and managed tree aside.
func (s *realRouteSeed) snapshot(t *testing.T) {
	t.Helper()
	if err := s.db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{s.dbDir, s.output} {
		if out, err := exec.Command("cp", "-a", dir, dir+".snapshot").CombinedOutput(); err != nil {
			t.Fatalf("snapshot %s: %v: %s", dir, err, out)
		}
	}
	s.db = openManagedRefreshPipelineStore(t, s.dbDir, s.output)
}

// restore closes the store, puts the snapshot back, and reopens it.
func (s *realRouteSeed) restore(t *testing.T) {
	t.Helper()
	if err := s.db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{s.dbDir, s.output} {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("cp", "-a", dir+".snapshot", dir).CombinedOutput(); err != nil {
			t.Fatalf("restore %s: %v: %s", dir, err, out)
		}
	}
	s.db = openManagedRefreshPipelineStore(t, s.dbDir, s.output)
}

// realRouteRun names how one refresh run is wired.
type realRouteRun struct {
	disableSkips bool
	profiler     *ingest.IndexProfiler
	halves       *halfRecorder
}

func runRealRoute(t *testing.T, seed *realRouteSeed, db managedRefreshPipelineStore, source string, run realRouteRun) *ingest.PipelineResult {
	t.Helper()
	fs := &ingest.OSFileSystem{}
	cfg := makePipelineConfig(seed.output)
	cfg.Sources = map[ingest.Harness]ingest.SourceConfig{
		seed.harness: {Paths: []ingest.ResolvedPath{ingest.ResolvedPath(source)}, Enabled: true},
	}
	cfg.StalenessThreshold = 0
	cfg.DisableRefreshSkips = run.disableSkips
	cfg.IndexProfiler = run.profiler
	indexers := ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})
	indexers[ingest.HarnessClaudeCode] = seamClaudeIndexer{ClaudeIndexer: ingest.NewClaudeIndexer(fs, ingest.WithClaudeFullDepth(true)), halves: run.halves}
	pipeline, err := newTestPipeline(fs, testutil.NoGitResolver(), ingest.DefaultAdapterRegistry, cfg,
		ingest.WithStore(db), ingest.WithMetricsStore(db), ingest.WithSalt(db.InstallationSalt()),
		ingest.WithIndexers(indexers), ingest.WithLogger(db), ingest.WithIndexLogger(db))
	if err != nil {
		t.Fatalf("build the refresh pipeline: %v", err)
	}
	result, err := pipeline.Run(t.Context())
	if err != nil {
		t.Fatalf("run the refresh pipeline: %v", err)
	}
	return result
}

// forgeStoredMetadataHash forges the active generation's stored metadata_hash
// of every named session (all when none is named) to another non-zero value,
// with the immutability trigger lifted for the update and restored from its
// own stored definition.
func forgeStoredMetadataHash(t *testing.T, db *store.Store, sids ...ingest.SessionID) {
	t.Helper()
	conn, err := db.Pool().Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Pool().Put(conn)
	var trigger string
	if err := sqlitex.Execute(conn, `SELECT sql FROM sqlite_schema WHERE type='trigger' AND name='session_generations_immutable'`, &sqlitex.ExecOptions{ResultFunc: func(st *sqlite.Stmt) error {
		trigger = st.ColumnText(0)
		return nil
	}}); err != nil || trigger == "" {
		t.Fatalf("read the immutability trigger: %q, %v", trigger, err)
	}
	update := `UPDATE session_generations SET metadata_hash = ? WHERE generation_id IN (SELECT active_generation_id FROM sessions)`
	args := []any{strings.Repeat("f", 64)}
	if len(sids) == 1 {
		update += ` AND session_id = ?`
		args = append(args, string(sids[0]))
	}
	for _, step := range []struct {
		query string
		args  []any
	}{{`DROP TRIGGER session_generations_immutable`, nil}, {update, args}, {trigger, nil}} {
		if err := sqlitex.Execute(conn, step.query, &sqlitex.ExecOptions{Args: step.args}); err != nil {
			t.Fatalf("forge the stored metadata hash: %s: %v", step.query, err)
		}
	}
}

// insertContextCommit adds one commit row to a session's active generation:
// a stored context row the captured input does not carry.
func insertContextCommit(t *testing.T, db *store.Store, sid ingest.SessionID) {
	t.Helper()
	conn, err := db.Pool().Take(context.Background())
	if err != nil {
		t.Error(err)
		return
	}
	defer db.Pool().Put(conn)
	if err := sqlitex.Execute(conn, `INSERT INTO session_generation_commits(session_id,generation_id,ordinal,hash,message,author_name,author_email,commit_time,author_time)
SELECT session_id, active_generation_id, 0, ?, 'synthetic', 'a', 'a@example.invalid', 1, 1 FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{strings.Repeat("e", 40), string(sid)}}); err != nil {
		t.Errorf("insert a context row: %v", err)
	}
}

// sessionDetailBytes renders a session through the one conversion path the
// viewer and the export share.
func sessionDetailBytes(t *testing.T, seed *realRouteSeed, sid ingest.SessionID) []byte {
	t.Helper()
	payload, err := export.ExportSession(t.Context(), seed.db, &ingest.OSFileSystem{}, string(sid), seed.output)
	if err != nil {
		t.Fatalf("export session %s: %v", sid, err)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func routeSessions(t *testing.T, db *store.Store) []ingest.SessionID {
	t.Helper()
	var sids []ingest.SessionID
	for _, row := range managedRefreshPipelineRows(t, db, `SELECT session_id FROM sessions ORDER BY session_id`) {
		sids = append(sids, ingest.SessionID(row))
	}
	if len(sids) == 0 {
		t.Fatal("the seed holds no session")
	}
	return sids
}

// routeParses counts the index-log rows that measured a parse.
func routeParses(result *ingest.PipelineResult) int {
	parses := 0
	for _, entry := range result.IndexLog {
		if entry.ParseDuration != nil && *entry.ParseDuration > 0 {
			parses++
		}
	}
	return parses
}

func routeStage(p *ingest.IndexProfiler, stage ingest.Stage) (total, done int) {
	for _, s := range p.StageTotals() {
		if s.Stage == stage {
			return s.Total, s.Done
		}
	}
	return 0, 0
}

// assertMatchesPlainParse restores the snapshot, applies prepare, parses with
// the skips disabled, and requires every session's detail bytes to equal what
// the refresh left.
func assertMatchesPlainParse(t *testing.T, seed *realRouteSeed, refreshed map[ingest.SessionID][]byte, prepare func(*store.Store)) {
	t.Helper()
	seed.restore(t)
	if prepare != nil {
		prepare(seed.db)
	}
	plain := runRealRoute(t, seed, seed.db, seed.empty, realRouteRun{disableSkips: true})
	if plain.Summary.Errors != 0 {
		t.Fatalf("the plain parse failed: %+v", plain.Summary)
	}
	for sid, want := range refreshed {
		if got := sessionDetailBytes(t, seed, sid); !bytes.Equal(got, want) {
			t.Fatalf("session %s: the refresh left session_detail bytes that differ from a plain parse:\nrefresh %s\nparse   %s", sid, want, got)
		}
	}
}

func detailOfEvery(t *testing.T, seed *realRouteSeed) map[ingest.SessionID][]byte {
	t.Helper()
	details := map[ingest.SessionID][]byte{}
	for _, sid := range routeSessions(t, seed.db) {
		details[sid] = sessionDetailBytes(t, seed, sid)
	}
	return details
}

// failingValidatorStore is the real store whose validator fails with an I/O
// error: a dependency failure the pipeline must answer with a parse.
type failingValidatorStore struct {
	*halfValidatingStore
	calls int
}

func (s *failingValidatorStore) ValidateManagedGenerationUnchanged(context.Context, ingest.ManagedGenerationValidationRequest) (ingest.ManagedGenerationValidationResult, error) {
	s.calls++
	return ingest.ManagedGenerationValidationResult{}, errors.New("synthetic read failure: disk I/O error")
}

// realStoreValidationRoutes are the refresh routes of context validation on
// the real store, each compared with a plain parse of the same snapshot.
func realStoreValidationRoutes() map[string]func(*testing.T) {
	claudeCorpus := func(t *testing.T, source string) { writeManagedRefreshPipelineCorpus(t, source, 1, 8) }
	return map[string]func(*testing.T){
		"validated-settles-with-the-real-store": func(t *testing.T) {
			seed := seedRealRoute(t, ingest.HarnessClaudeCode, claudeCorpus)
			forgeStoredMetadataHash(t, seed.db)
			seed.snapshot(t)
			sid := routeSessions(t, seed.db)[0]
			before := managedRefreshPipelineActiveGeneration(t, seed.db, sid)
			observed := newHalfValidatingStore(seed.db)
			profiler := &ingest.IndexProfiler{}
			result := runRealRoute(t, seed, observed, seed.empty, realRouteRun{profiler: profiler})
			observed.assertWiring(t)
			outcome, asked := observed.outcome(sid)
			if observed.cheapProven[sid] || !asked || outcome.Outcome != ingest.ValidationProven || observed.settled[sid] != nil || len(observed.settled) != 1 {
				t.Fatalf("cheap proof %t, validation %+v (asked %t), settles %v; want a failed cheap proof, one proven validation and one settle", observed.cheapProven[sid], outcome, asked, observed.settled)
			}
			if parses := routeParses(result); parses != 0 || len(result.IndexLog) != 1 || result.IndexLog[0].Outcome != ingest.IndexOutcomeSkipped ||
				result.IndexLog[0].Reason == nil || !strings.HasSuffix(*result.IndexLog[0].Reason, "(unchanged)") {
				t.Fatalf("parses %d, index log %+v; want no parse and one skipped (unchanged) row", parses, result.IndexLog)
			}
			if total, done := routeStage(profiler, ingest.StageRefreshValidate); total != 1 || done != 1 {
				t.Fatalf("refresh_validate total %d done %d, want 1 and 1", total, done)
			}
			if after := managedRefreshPipelineActiveGeneration(t, seed.db, sid); after != before {
				t.Fatalf("the validated settle moved the active generation %s -> %s", before, after)
			}
			assertMatchesPlainParse(t, seed, detailOfEvery(t, seed), nil)
		},
		"unsupported-harness-parses": func(t *testing.T) {
			seed := seedRealRouteAt(t, ingest.HarnessCodex, "sessions", func(t *testing.T, source string) {
				if out, err := exec.Command("cp", "-a", codexRouteFixture+"/.", source).CombinedOutput(); err != nil {
					t.Fatalf("copy the Codex fixture: %v: %s", err, out)
				}
			})
			forgeStoredMetadataHash(t, seed.db)
			seed.snapshot(t)
			sessions := len(routeSessions(t, seed.db))
			observed := newHalfValidatingStore(seed.db)
			// A Codex generation's stored content hash is not the captured
			// one, so its cheap proof stops on content-hash before the context
			// terms. The route under test starts after a context mismatch.
			observed.forceContext = true
			profiler := &ingest.IndexProfiler{}
			result := runRealRoute(t, seed, observed, seed.empty, realRouteRun{profiler: profiler})
			if result.Summary.Errors != 0 {
				t.Fatalf("the Codex refresh failed: %+v", result.Summary)
			}
			if len(observed.outcomes) != 0 || len(observed.settled) != 0 {
				t.Fatalf("the Codex refresh asked the validator %v or settled %v; Codex has no metadata candidate", observed.outcomes, observed.settled)
			}
			if total, _ := routeStage(profiler, ingest.StageRefreshValidateUnsupported); total != sessions {
				t.Fatalf("refresh_validate_unsupported total %d, want %d", total, sessions)
			}
			if parses := routeParses(result); parses != sessions {
				t.Fatalf("parses %d, want %d", parses, sessions)
			}
			assertMatchesPlainParse(t, seed, detailOfEvery(t, seed), nil)
		},
		"validator-error-parses-on-the-real-store": func(t *testing.T) {
			seed := seedRealRoute(t, ingest.HarnessClaudeCode, claudeCorpus)
			forgeStoredMetadataHash(t, seed.db)
			seed.snapshot(t)
			failing := &failingValidatorStore{halfValidatingStore: newHalfValidatingStore(seed.db)}
			profiler := &ingest.IndexProfiler{}
			result := runRealRoute(t, seed, failing, seed.empty, realRouteRun{profiler: profiler})
			if result.Summary.Errors != 0 || failing.calls != 1 || len(failing.settled) != 0 || routeParses(result) != 1 {
				t.Fatalf("summary %+v, validator calls %d, settles %v, parses %d; want one failed validation and one parse", result.Summary, failing.calls, failing.settled, routeParses(result))
			}
			if total, _ := routeStage(profiler, ingest.StageRefreshValidateUnsupported); total != 1 {
				t.Fatalf("refresh_validate_unsupported total %d, want 1", total)
			}
			assertMatchesPlainParse(t, seed, detailOfEvery(t, seed), nil)
		},
		"validated-settle-refused-parses-on-the-real-store": func(t *testing.T) {
			seed := seedRealRoute(t, ingest.HarnessClaudeCode, claudeCorpus)
			forgeStoredMetadataHash(t, seed.db)
			seed.snapshot(t)
			sid := routeSessions(t, seed.db)[0]
			observed := newHalfValidatingStore(seed.db)
			// Between the proven validation and the settle, a context row
			// lands on the active generation; the settle's re-check under the
			// lock must refuse it.
			observed.afterValidation = func(got ingest.SessionID) { insertContextCommit(t, seed.db, got) }
			result := runRealRoute(t, seed, observed, seed.empty, realRouteRun{})
			outcome, _ := observed.outcome(sid)
			var stale *ingest.StaleIndexWorkError
			if outcome.Outcome != ingest.ValidationProven || !errors.As(observed.settled[sid], &stale) {
				t.Fatalf("validation %+v, settle %v; want proven, then a stale-work refusal", outcome, observed.settled[sid])
			}
			if result.Summary.Errors != 0 || routeParses(result) != 1 {
				t.Fatalf("summary %+v, parses %d; want the refused settle to parse once", result.Summary, routeParses(result))
			}
			assertMatchesPlainParse(t, seed, detailOfEvery(t, seed), func(db *store.Store) { insertContextCommit(t, db, sid) })
		},
		"claude-without-candidate-parses": func(t *testing.T) {
			document := loadOracleDocument(t)
			const parent, fork = "-workspace/22222222-2222-4222-8222-222222222222.jsonl", ingest.SessionID("99d59925-36bc-424c-a789-8be54d9702ba")
			seed := seedRealRoute(t, ingest.HarnessClaudeCode, func(t *testing.T, source string) {
				for _, file := range document.Corpus {
					if file.Path == parent || strings.Contains(file.Path, string(fork)) {
						writeOracleFile(t, source, document, file.Path)
					}
				}
			})
			forgeStoredMetadataHash(t, seed.db, fork)
			before := managedRefreshPipelineActiveGeneration(t, seed.db, fork)
			observed := newHalfValidatingStore(seed.db)
			halves := newHalfRecorder()
			// The parent's native source disappears after the capture and
			// before the validator: the fork's metadata half then has no
			// structural proof and refuses.
			prove := &oracleProveSeam{halfValidatingStore: observed, before: func() { removeOracleFiles(t, seed.source, []string{parent}) }}
			profiler := &ingest.IndexProfiler{}
			result := runRealRoute(t, seed, prove, seed.empty, realRouteRun{profiler: profiler, halves: halves})
			if halves.refusal(fork) == nil {
				t.Fatal("the fork's metadata half did not refuse")
			}
			if _, asked := observed.outcome(fork); asked {
				t.Fatal("the validator was asked for the fork without a candidate")
			}
			if _, settled := observed.settled[fork]; settled {
				t.Fatal("the fork settled without a candidate")
			}
			if total, _ := routeStage(profiler, ingest.StageRefreshValidateUnsupported); total != 1 {
				t.Fatalf("refresh_validate_unsupported total %d, want 1", total)
			}
			if parses := routeParses(result); parses != 1 {
				t.Fatalf("parses %d, want the fork to parse once", parses)
			}
			refused := false
			for _, entry := range result.IndexLog {
				if entry.SessionID == fork && entry.Outcome != ingest.IndexOutcomeSkipped && entry.Outcome != ingest.IndexOutcomeIndexed {
					refused = true
				}
			}
			for _, session := range result.Sessions {
				if session.SessionID == fork && session.Error != nil {
					refused = true
				}
			}
			if !refused {
				t.Fatalf("the fork's parse recorded no refusal: index log %+v", result.IndexLog)
			}
			if after := managedRefreshPipelineActiveGeneration(t, seed.db, fork); after != before {
				t.Fatalf("the refused parse moved the active generation %s -> %s", before, after)
			}
		},
	}
}
