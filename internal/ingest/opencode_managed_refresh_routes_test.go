package ingest_test

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/peasant-labs/peasant/internal/export"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
	regexp "github.com/wasilibs/go-re2"
)

//go:embed testdata/opencode_managed_refresh_routes.yaml
var openCodeManagedRefreshRoutesYAML []byte

//go:embed testdata/opencode_managed_refresh_routes.manifest.yaml
var openCodeManagedRefreshRoutesManifestYAML []byte

type withholdingOpenCodeAdapter struct {
	*ingest.OpenCodeAdapter
}

func (*withholdingOpenCodeAdapter) WithholdsChangeScope() bool { return true }

var _ ingest.ChangeScopeWithholder = (*withholdingOpenCodeAdapter)(nil)

type routeVersionRedactor struct {
	redact.Redactor
	version string
}

func (r *routeVersionRedactor) RuleSetVersion() string { return r.version }

var _ ingest.TextRedactor = (*routeVersionRedactor)(nil)

type openCodeRouteSeed struct {
	root         string
	sourceRoot   string
	nativeDBPath string
	peasantDBDir string
	managedDir   string
	repoDir      string
	db           *store.Store
	useEnvDB     bool
	redactor     ingest.TextRedactor
}

func seedOpenCodeCurrentRoute(t *testing.T, corpus string, redactor ingest.TextRedactor) *openCodeRouteSeed {
	t.Helper()
	root := t.TempDir()
	repoDir := filepath.Join(root, "repo")
	nativeDBPath := filepath.Join(root, "native", "opencode.db")
	peasantDBDir := filepath.Join(root, "peasant-db")
	managedDir := filepath.Join(root, "managed")
	mustMkdir(t, repoDir)
	mustMkdir(t, peasantDBDir)
	mustMkdir(t, managedDir)

	createOpenCodeOracleNativeDB(t, nativeDBPath, repoDir, corpus)

	seed := &openCodeRouteSeed{
		root:         root,
		sourceRoot:   filepath.Dir(nativeDBPath),
		nativeDBPath: nativeDBPath,
		peasantDBDir: peasantDBDir,
		managedDir:   managedDir,
		repoDir:      repoDir,
		useEnvDB:     true,
		redactor:     redactor,
	}
	seed.db = openManagedRefreshPipelineStore(t, filepath.Join(peasantDBDir, "peasant.db"), managedDir)
	t.Cleanup(func() {
		if seed.db != nil {
			_ = seed.db.Close()
		}
	})
	first := runOpenCodeRoute(t, seed, seed.db, openCodeRouteRun{
		versions: openCodeMemoVersions(4),
	})
	if first.Summary.Errors != 0 || first.Summary.Indexed == 0 {
		t.Fatalf("seed current OpenCode route (%s): err=%v summary=%+v", corpus, first.Summary.StoreError, first.Summary)
	}
	target := openCodeMemoVersions(5)[ingest.HarnessOpenCode].AdapterVersion - 1
	managedRefreshPipelineDowngrade(t, seed.db, target)
	seed.snapshot(t)
	return seed
}

func (s *openCodeRouteSeed) snapshot(t *testing.T) {
	t.Helper()
	if err := s.db.Close(); err != nil {
		t.Fatal(err)
	}
	s.db = nil
	for _, pair := range [][2]string{
		{s.peasantDBDir, s.peasantDBDir + ".snapshot"},
		{s.managedDir, s.managedDir + ".snapshot"},
	} {
		_ = os.RemoveAll(pair[1])
		copyTree(t, pair[0], pair[1])
	}
	if s.nativeDBPath != "" {
		copyFile(t, s.nativeDBPath, s.nativeDBPath+".snapshot")
	}
	s.db = openManagedRefreshPipelineStore(t, filepath.Join(s.peasantDBDir, "peasant.db"), s.managedDir)
}

func (s *openCodeRouteSeed) restore(t *testing.T) {
	t.Helper()
	if s.db != nil {
		if err := s.db.Close(); err != nil {
			t.Fatal(err)
		}
		s.db = nil
	}
	for _, pair := range [][2]string{
		{s.peasantDBDir + ".snapshot", s.peasantDBDir},
		{s.managedDir + ".snapshot", s.managedDir},
	} {
		if err := os.RemoveAll(pair[1]); err != nil {
			t.Fatal(err)
		}
		copyTree(t, pair[0], pair[1])
	}
	if s.nativeDBPath != "" {
		copyFile(t, s.nativeDBPath+".snapshot", s.nativeDBPath)
	}
	s.db = openManagedRefreshPipelineStore(t, filepath.Join(s.peasantDBDir, "peasant.db"), s.managedDir)
}

type openCodeRouteRun struct {
	disableSkips bool
	force        bool
	reindex      bool
	profiler     *ingest.IndexProfiler
	halves       *halfRecorder
	git          ingest.GitResolver
	redactor     ingest.TextRedactor
	withhold     bool
	versions     map[ingest.Harness]ingest.HarvesterVersions
	beforeBuild  func()
}

func runOpenCodeRoute(t *testing.T, seed *openCodeRouteSeed, db managedRefreshPipelineStore, run openCodeRouteRun) *ingest.PipelineResult {
	t.Helper()
	fs := &ingest.OSFileSystem{}
	cfg := makePipelineConfig(seed.managedDir)
	if validator, ok := db.(ingest.ManagedGenerationValidator); ok {
		cfg.Validator = validator
	}
	cfg.Sources = map[ingest.Harness]ingest.SourceConfig{
		ingest.HarnessOpenCode: {Paths: []ingest.ResolvedPath{ingest.ResolvedPath(seed.sourceRoot)}, Enabled: true},
	}
	cfg.StalenessThreshold = 0
	cfg.DisableRefreshSkips = run.disableSkips
	cfg.Force = run.force
	cfg.Reindex = run.reindex
	cfg.IndexProfiler = run.profiler

	env := mountedCurrentEnvironment{}
	if seed.useEnvDB && seed.nativeDBPath != "" {
		env["OPENCODE_DB"] = seed.nativeDBPath
	}
	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessOpenCode: func(f ingest.FileSystem, g ingest.GitResolver, s salt.Salt) ingest.SourceAdapter {
			adapter, err := ingest.NewOpenCodeAdapterWithCandidateProbe(
				f, g, s, "latest", env, fs, ingest.OpenOpenCodeSQLiteSource, ingest.DefaultOpenCodeSQLiteSourceOptions(),
			)
			if err != nil {
				t.Fatalf("NewOpenCodeAdapterWithCandidateProbe: %v", err)
			}
			if run.withhold {
				return &withholdingOpenCodeAdapter{OpenCodeAdapter: adapter}
			}
			return adapter
		},
	}
	indexers := ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})
	indexers[ingest.HarnessOpenCode] = &seamOpenCodeIndexer{
		OpenCodeIndexer: ingest.NewOpenCodeIndexer(fs),
		before:          run.beforeBuild,
		halves:          run.halves,
	}
	git := run.git
	if git == nil {
		git = &openCodeOracleGitResolver{
			remote:   openCodeOracleDefaultRemote,
			branch:   openCodeOracleDefaultBranch,
			tracking: openCodeOracleDefaultTracking,
		}
	}
	versions := run.versions
	if versions == nil {
		versions = openCodeMemoVersions(5)
	}
	opts := []ingest.PipelineOption{
		ingest.WithStore(db),
		ingest.WithMetricsStore(db),
		ingest.WithSalt(db.InstallationSalt()),
		ingest.WithIndexers(indexers),
		ingest.WithLogger(db),
		ingest.WithIndexLogger(db),
		ingest.WithHarvesterVersions(versions),
	}
	redactor := run.redactor
	if redactor == nil {
		redactor = seed.redactor
	}
	if redactor != nil {
		opts = append(opts, ingest.WithRedactor(redactor))
	}
	p, err := newTestPipeline(fs, git, adapters, cfg, opts...)
	if err != nil {
		t.Fatalf("build OpenCode route pipeline: %v", err)
	}
	res, err := p.Run(t.Context())
	if err != nil {
		t.Fatalf("run OpenCode route pipeline: %v", err)
	}
	return res
}

func openCodeSessionDetailBytes(t *testing.T, seed *openCodeRouteSeed, sid ingest.SessionID) []byte {
	t.Helper()
	payload, err := export.ExportSession(t.Context(), seed.db, &ingest.OSFileSystem{}, string(sid), seed.managedDir)
	if err != nil {
		t.Fatalf("export session %s: %v", sid, err)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func openCodeDetailOfEvery(t *testing.T, seed *openCodeRouteSeed) map[ingest.SessionID][]byte {
	t.Helper()
	details := map[ingest.SessionID][]byte{}
	for _, sid := range routeSessions(t, seed.db) {
		details[sid] = openCodeSessionDetailBytes(t, seed, sid)
	}
	return details
}

func assertOpenCodeMatchesPlainParse(t *testing.T, seed *openCodeRouteSeed, refreshed map[ingest.SessionID][]byte, prepare func(*openCodeRouteSeed), run openCodeRouteRun) {
	t.Helper()
	seed.restore(t)
	known := map[string]bool{}
	for sid := range refreshed {
		for _, ref := range openCodeDetailRefPattern.FindAllSubmatch(openCodeSessionDetailBytes(t, seed, sid), -1) {
			known[string(ref[2])] = true
		}
	}
	if prepare != nil {
		prepare(seed)
	}
	run.disableSkips = true
	plain := runOpenCodeRoute(t, seed, seed.db, run)
	if plain.Summary.Errors != 0 {
		t.Fatalf("plain OpenCode parse failed: %+v", plain.Summary)
	}
	for sid, want := range refreshed {
		got := openCodeSessionDetailBytes(t, seed, sid)
		if !bytes.Equal(withNewRefsNumbered(got, known), withNewRefsNumbered(want, known)) {
			t.Fatalf("session %s: OpenCode refresh left session_detail bytes that differ from a plain parse:\nrefresh %s\nparse   %s", sid, want, got)
		}
	}
}

// openCodeDetailRefPattern matches one entry or submission reference in
// encoded session_detail bytes.
var openCodeDetailRefPattern = regexp.MustCompile(`"(sourceEntryRef|submissionRef)":"([es]_[A-Z2-7]+)"`)

// withNewRefsNumbered replaces every reference not in known with its order of
// first appearance. A new entry gets a random reference in each run, so two
// runs can only agree on where new references appear; every reference that
// existed before the run must still match byte for byte.
func withNewRefsNumbered(detail []byte, known map[string]bool) []byte {
	numbered := map[string]string{}
	return openCodeDetailRefPattern.ReplaceAllFunc(detail, func(match []byte) []byte {
		parts := openCodeDetailRefPattern.FindSubmatch(match)
		ref := string(parts[2])
		if known[ref] {
			return match
		}
		if _, ok := numbered[ref]; !ok {
			numbered[ref] = fmt.Sprintf("new-%d", len(numbered))
		}
		return []byte(fmt.Sprintf(`"%s":"%s"`, parts[1], numbered[ref]))
	})
}

func assertRefreshCensusNativeAdapterBehind(t *testing.T, result *ingest.PipelineResult, wantCount int) {
	t.Helper()
	if result.Census == nil {
		t.Fatalf("result.Census is nil, want %d adapter_behind_native_only sessions", wantCount)
	}
	got := 0
	for _, row := range result.Census.Rows {
		if row.Reason.Code() == string(ingest.RefreshReasonAdapterBehindNativeOnly) && row.Lane == ingest.RefreshLaneNative {
			got += row.Sessions
		}
	}
	if got != wantCount {
		t.Fatalf("Census[adapter_behind_native_only, native] = %d, want %d (rows: %+v)", got, wantCount, result.Census.Rows)
	}
}

func assertProofDoneZero(t *testing.T, profiler *ingest.IndexProfiler, wantTotal int) {
	t.Helper()
	total, done := routeStage(profiler, ingest.StageRefreshProof)
	if total != wantTotal || done != 0 {
		t.Fatalf("StageRefreshProof total=%d done=%d, want total=%d done=0", total, done, wantTotal)
	}
}

func readRouteRedactedAtMs(t *testing.T, managedDir string, sid ingest.SessionID) *int64 {
	t.Helper()
	var found *int64
	err := filepath.WalkDir(managedDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || !strings.HasSuffix(entry.Name(), string(sid)+"--metadata.json") {
			return walkErr
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var meta schema.UnifiedMetadata
		if err := json.Unmarshal(data, &meta); err != nil {
			return err
		}
		found = meta.Redaction.RedactedAtMs
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// openCodeLegacyIndexFormat is the stored index format of a session that stays
// on the legacy native path.
const openCodeLegacyIndexFormat = 1

// openCodeFormat1Versions keeps the registry's OpenCode indexer and format and
// sets only the adapter version. A format-1 session is stamped with the
// registry indexer, so the format-2 override in openCodeMemoVersions would
// refuse it before any parse ran.
func openCodeFormat1Versions(adapterVersion int) map[ingest.Harness]ingest.HarvesterVersions {
	versions := maps.Clone(ingest.HarvesterVersionRegistry)
	v := versions[ingest.HarnessOpenCode]
	v.AdapterVersion = adapterVersion
	versions[ingest.HarnessOpenCode] = v
	return versions
}

func openCodeFormat1AdapterVersion() int {
	return ingest.HarvesterVersionRegistry[ingest.HarnessOpenCode].AdapterVersion
}

func seedLegacySQLiteRoute(t *testing.T) *openCodeRouteSeed {
	t.Helper()
	root := t.TempDir()
	repoDir := filepath.Join(root, "repo")
	nativeDBPath := filepath.Join(root, "native", "opencode.db")
	peasantDBDir := filepath.Join(root, "peasant-db")
	managedDir := filepath.Join(root, "managed")
	mustMkdir(t, repoDir)
	mustMkdir(t, filepath.Dir(nativeDBPath))
	mustMkdir(t, peasantDBDir)
	mustMkdir(t, managedDir)

	conn, err := sqlite.OpenConn(nativeDBPath, sqlite.OpenReadWrite|sqlite.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	ddl := `
CREATE TABLE session (id TEXT PRIMARY KEY, parent_id TEXT, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, directory TEXT, title TEXT);
CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL);
CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT NOT NULL, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL);
`
	if err := sqlitex.ExecuteScript(conn, ddl, nil); err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	for _, stmt := range []struct {
		q    string
		args []any
	}{
		{`INSERT INTO session (id, time_created, time_updated, directory, title) VALUES (?1, 1000, 2000, ?2, 'legacy sqlite')`, []any{openCodeOracleOrdinaryID, repoDir}},
		{`INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES ('msg_leg_1', ?1, 1000, 2000, '{"role":"assistant"}')`, []any{openCodeOracleOrdinaryID}},
		{`INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES ('part_leg_1', 'msg_leg_1', ?1, 1001, 2000, '{"type":"text","text":"legacy sqlite text"}')`, []any{openCodeOracleOrdinaryID}},
	} {
		if err := sqlitex.Execute(conn, stmt.q, &sqlitex.ExecOptions{Args: stmt.args}); err != nil {
			_ = conn.Close()
			t.Fatal(err)
		}
	}
	_ = conn.Close()

	seed := &openCodeRouteSeed{
		root:         root,
		sourceRoot:   filepath.Dir(nativeDBPath),
		nativeDBPath: nativeDBPath,
		peasantDBDir: peasantDBDir,
		managedDir:   managedDir,
		repoDir:      repoDir,
		useEnvDB:     true,
	}
	seed.db = openManagedRefreshPipelineStore(t, filepath.Join(peasantDBDir, "peasant.db"), managedDir)
	t.Cleanup(func() {
		if seed.db != nil {
			_ = seed.db.Close()
		}
	})
	first := runOpenCodeRoute(t, seed, seed.db, openCodeRouteRun{
		versions: openCodeFormat1Versions(openCodeFormat1AdapterVersion() - 1),
	})
	if first.Summary.Errors != 0 || first.Summary.Indexed == 0 {
		t.Fatalf("seed legacy SQLite route failed: %+v", first.Summary)
	}
	managedRefreshPipelineDowngrade(t, seed.db, openCodeFormat1AdapterVersion()-1)
	return seed
}

func seedJSONTreeRoute(t *testing.T) *openCodeRouteSeed {
	t.Helper()
	root := t.TempDir()
	repoDir := filepath.Join(root, "repo")
	sourceRoot := filepath.Join(root, "json-root")
	peasantDBDir := filepath.Join(root, "peasant-db")
	managedDir := filepath.Join(root, "managed")
	mustMkdir(t, repoDir)
	mustMkdir(t, peasantDBDir)
	mustMkdir(t, managedDir)

	sessionDir := filepath.Join(sourceRoot, "storage", "session", openCodeOracleProjectID)
	msgDir := filepath.Join(sourceRoot, "storage", "message", openCodeOracleOrdinaryID)
	partDir := filepath.Join(sourceRoot, "storage", "part", "msg_json_1")
	mustMkdir(t, sessionDir)
	mustMkdir(t, msgDir)
	mustMkdir(t, partDir)

	writeJSON := func(path, body string) {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeJSON(filepath.Join(sessionDir, openCodeOracleOrdinaryID+".json"),
		`{"id":"`+openCodeOracleOrdinaryID+`","directory":`+mustJSONString(t, repoDir)+`,"title":"json tree","time":{"created":1000,"updated":2000}}`)
	writeJSON(filepath.Join(msgDir, "msg_json_1.json"),
		`{"id":"msg_json_1","sessionID":"`+openCodeOracleOrdinaryID+`","role":"assistant","time":{"created":1000}}`)
	writeJSON(filepath.Join(partDir, "part_json_1.json"),
		`{"id":"part_json_1","sessionID":"`+openCodeOracleOrdinaryID+`","messageID":"msg_json_1","type":"text","text":"json tree answer"}`)

	seed := &openCodeRouteSeed{
		root:         root,
		sourceRoot:   sourceRoot,
		peasantDBDir: peasantDBDir,
		managedDir:   managedDir,
		repoDir:      repoDir,
		useEnvDB:     false,
	}
	seed.db = openManagedRefreshPipelineStore(t, filepath.Join(peasantDBDir, "peasant.db"), managedDir)
	t.Cleanup(func() {
		if seed.db != nil {
			_ = seed.db.Close()
		}
	})
	first := runOpenCodeRoute(t, seed, seed.db, openCodeRouteRun{
		versions: openCodeFormat1Versions(openCodeFormat1AdapterVersion() - 1),
	})
	if first.Summary.Errors != 0 || first.Summary.Indexed == 0 {
		t.Fatalf("seed JSON tree route failed: %+v", first.Summary)
	}
	managedRefreshPipelineDowngrade(t, seed.db, openCodeFormat1AdapterVersion()-1)
	return seed
}

func seedControlOnlyCurrentFallbackRoute(t *testing.T) *openCodeRouteSeed {
	t.Helper()
	root := t.TempDir()
	repoDir := filepath.Join(root, "repo")
	nativeDBPath := filepath.Join(root, "native", "opencode.db")
	peasantDBDir := filepath.Join(root, "peasant-db")
	managedDir := filepath.Join(root, "managed")
	mustMkdir(t, repoDir)
	mustMkdir(t, filepath.Dir(nativeDBPath))
	mustMkdir(t, peasantDBDir)
	mustMkdir(t, managedDir)

	conn, err := sqlite.OpenConn(nativeDBPath, sqlite.OpenReadWrite|sqlite.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	ddl := `
CREATE TABLE session (id TEXT PRIMARY KEY, parent_id TEXT, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, directory TEXT, title TEXT);
CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL);
CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT NOT NULL, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL);
CREATE TABLE session_message (id TEXT NOT NULL, session_id TEXT NOT NULL, type TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL, seq INTEGER NOT NULL, PRIMARY KEY (session_id, id));
CREATE INDEX session_message_seq_idx ON session_message(seq);
CREATE UNIQUE INDEX session_message_session_seq_idx ON session_message(session_id, seq);
`
	if err := sqlitex.ExecuteScript(conn, ddl, nil); err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	for _, stmt := range []struct {
		q    string
		args []any
	}{
		{`INSERT INTO session (id, time_created, time_updated, directory, title) VALUES (?1, 1000, 2000, ?2, 'legacy sibling')`, []any{openCodeOracleOrdinaryID, repoDir}},
		{`INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES ('msg_leg_1', ?1, 1000, 1010, json_object('id', 'msg_leg_1', 'sessionID', ?1, 'role', 'user', 'path', json_object('cwd', ?2), 'time', json_object('created', 1000, 'completed', 1010)))`, []any{openCodeOracleOrdinaryID, repoDir}},
		{`INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES ('part_leg_1', 'msg_leg_1', ?1, 1001, 1001, '{"id":"part_leg_1","messageID":"msg_leg_1","type":"text","text":"legacy sibling text","time":{"created":1001}}')`, []any{openCodeOracleOrdinaryID}},
		{`INSERT INTO session_message (id, session_id, type, time_created, time_updated, data, seq) VALUES ('msg_ctrl_1', ?1, 'model-switched', 2000, 2000, '{"time":{"created":2000},"model":{"id":"synthetic-model","providerID":"synthetic-provider"}}', 1)`, []any{openCodeOracleOrdinaryID}},
	} {
		if err := sqlitex.Execute(conn, stmt.q, &sqlitex.ExecOptions{Args: stmt.args}); err != nil {
			_ = conn.Close()
			t.Fatal(err)
		}
	}
	_ = conn.Close()

	seed := &openCodeRouteSeed{
		root:         root,
		sourceRoot:   filepath.Dir(nativeDBPath),
		nativeDBPath: nativeDBPath,
		peasantDBDir: peasantDBDir,
		managedDir:   managedDir,
		repoDir:      repoDir,
		useEnvDB:     true,
	}
	seed.db = openManagedRefreshPipelineStore(t, filepath.Join(peasantDBDir, "peasant.db"), managedDir)
	t.Cleanup(func() {
		if seed.db != nil {
			_ = seed.db.Close()
		}
	})
	first := runOpenCodeRoute(t, seed, seed.db, openCodeRouteRun{
		versions: openCodeFormat1Versions(openCodeFormat1AdapterVersion() - 1),
	})
	if first.Summary.Errors != 0 || first.Summary.Indexed == 0 {
		t.Fatalf("seed control-only current route failed: %+v", first.Summary)
	}
	managedRefreshPipelineDowngrade(t, seed.db, openCodeFormat1AdapterVersion()-1)
	return seed
}

func mustJSONString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func openCodeManagedRefreshRoutes() map[string]func(*testing.T) {
	return map[string]func(*testing.T){
		"stamp-only-validated-settles": func(t *testing.T) {
			seed := seedOpenCodeCurrentRoute(t, "ordinary", nil)
			observed := newHalfValidatingStore(seed.db)
			profiler := &ingest.IndexProfiler{}
			res := runOpenCodeRoute(t, seed, observed, openCodeRouteRun{profiler: profiler})
			assertRefreshCensusNativeAdapterBehind(t, res, 1)
			assertProofDoneZero(t, profiler, 1)
			if total, done := routeStage(profiler, ingest.StageRefreshValidate); total != 1 || done != 1 {
				t.Fatalf("refresh_validate total=%d done=%d, want 1 and 1; validator outcomes %+v", total, done, observed.outcomes)
			}
			if len(observed.settled) != 1 || routeParses(res) != 0 {
				t.Fatalf("settled=%v parses=%d, want 1 settle and 0 parses", observed.settled, routeParses(res))
			}
			assertOpenCodeMatchesPlainParse(t, seed, openCodeDetailOfEvery(t, seed), nil, openCodeRouteRun{})
		},
		"stale-hash-validated-settles": func(t *testing.T) {
			seed := seedOpenCodeCurrentRoute(t, "ordinary", nil)
			forgeStoredMetadataHash(t, seed.db)
			seed.snapshot(t)
			observed := newHalfValidatingStore(seed.db)
			profiler := &ingest.IndexProfiler{}
			res := runOpenCodeRoute(t, seed, observed, openCodeRouteRun{profiler: profiler})
			assertRefreshCensusNativeAdapterBehind(t, res, 1)
			assertProofDoneZero(t, profiler, 1)
			if total, done := routeStage(profiler, ingest.StageRefreshValidate); total != 1 || done != 1 {
				t.Fatalf("refresh_validate total=%d done=%d, want 1 and 1; validator outcomes %+v", total, done, observed.outcomes)
			}
			if len(observed.settled) != 1 || routeParses(res) != 0 {
				t.Fatalf("settled=%v parses=%d, want 1 settle and 0 parses", observed.settled, routeParses(res))
			}
			assertOpenCodeMatchesPlainParse(t, seed, openCodeDetailOfEvery(t, seed), nil, openCodeRouteRun{})
		},
		"native-fork-validated-settles": func(t *testing.T) {
			seed := seedOpenCodeCurrentRoute(t, "fork", nil)
			observed := newHalfValidatingStore(seed.db)
			profiler := &ingest.IndexProfiler{}
			res := runOpenCodeRoute(t, seed, observed, openCodeRouteRun{profiler: profiler})
			assertRefreshCensusNativeAdapterBehind(t, res, 2)
			assertProofDoneZero(t, profiler, 2)
			if total, done := routeStage(profiler, ingest.StageRefreshValidate); total != 2 || done != 2 {
				t.Fatalf("refresh_validate total=%d done=%d, want 2 and 2; validator outcomes %+v", total, done, observed.outcomes)
			}
			if len(observed.settled) != 2 || routeParses(res) != 0 {
				t.Fatalf("settled=%v parses=%d, want 2 settles and 0 parses", observed.settled, routeParses(res))
			}
			assertOpenCodeMatchesPlainParse(t, seed, openCodeDetailOfEvery(t, seed), nil, openCodeRouteRun{})
		},
		"live-db-mutated-after-extract-parses": func(t *testing.T) {
			seed := seedOpenCodeCurrentRoute(t, "ordinary", nil)
			sid := routeSessions(t, seed.db)[0]
			before := managedRefreshPipelineActiveGeneration(t, seed.db, sid)
			observed := newHalfValidatingStore(seed.db)
			var once sync.Once
			appendLive := func() {
				once.Do(func() {
					execNativeOpenCodeSQL(t, seed.nativeDBPath,
						`INSERT INTO session_message (id, session_id, type, time_created, time_updated, data, seq) VALUES ('msg_live_after_extract', ?1, 'user', 3000, 3000, '{"id":"msg_live_after_extract","type":"user","text":"live after extract","time":{"created":3000}}', 2)`,
						openCodeOracleOrdinaryID)
				})
			}
			prove := &openCodeOracleProveSeam{halfValidatingStore: observed, before: appendLive}
			profiler := &ingest.IndexProfiler{}
			res := runOpenCodeRoute(t, seed, prove, openCodeRouteRun{profiler: profiler})
			assertRefreshCensusNativeAdapterBehind(t, res, 1)
			assertProofDoneZero(t, profiler, 1)
			if total, done := routeStage(profiler, ingest.StageRefreshValidate); total != 1 || done != 0 {
				t.Fatalf("refresh_validate total=%d done=%d, want 1 and 0", total, done)
			}
			if routeParses(res) != 1 || managedRefreshPipelineActiveGeneration(t, seed.db, sid) == before {
				t.Fatalf("parses=%d active=%s (before=%s), want 1 parse and new generation", routeParses(res), managedRefreshPipelineActiveGeneration(t, seed.db, sid), before)
			}
			// The plain parse meets the same race: the row lands after the
			// metadata extraction and before the build.
			var plainOnce sync.Once
			appendLivePlain := func() {
				plainOnce.Do(func() {
					execNativeOpenCodeSQL(t, seed.nativeDBPath,
						`INSERT INTO session_message (id, session_id, type, time_created, time_updated, data, seq) VALUES ('msg_live_after_extract', ?1, 'user', 3000, 3000, '{"id":"msg_live_after_extract","type":"user","text":"live after extract","time":{"created":3000}}', 2)`,
						openCodeOracleOrdinaryID)
				})
			}
			assertOpenCodeMatchesPlainParse(t, seed, openCodeDetailOfEvery(t, seed), nil, openCodeRouteRun{beforeBuild: appendLivePlain})
		},
		"context-change-references": func(t *testing.T) {
			seed := seedOpenCodeCurrentRoute(t, "ordinary", nil)
			execNativeOpenCodeSQL(t, seed.nativeDBPath, `UPDATE project SET name = 'renamed-opencode-project' WHERE id = ?1`, openCodeOracleProjectID)
			seed.snapshot(t)
			sid := routeSessions(t, seed.db)[0]
			before := managedRefreshPipelineActiveGeneration(t, seed.db, sid)
			observed := newReferenceRouteStore(seed.db)
			profiler := &ingest.IndexProfiler{}
			res := runOpenCodeRoute(t, seed, observed, openCodeRouteRun{profiler: profiler})
			assertRefreshCensusNativeAdapterBehind(t, res, 1)
			assertProofDoneZero(t, profiler, 1)
			if total, done := routeStage(profiler, ingest.StageRefreshValidate); total != 1 || done != 0 {
				t.Fatalf("refresh_validate total=%d done=%d, want 1 and 0", total, done)
			}
			if total, done := routeStage(profiler, ingest.StageRefreshReference); total != 1 || done != 1 {
				t.Fatalf("refresh_reference total=%d done=%d, want 1 and 1", total, done)
			}
			if routeParses(res) != 0 || managedRefreshPipelineActiveGeneration(t, seed.db, sid) == before {
				t.Fatalf("parses=%d active=%s (before=%s), want 0 parses and new reference generation", routeParses(res), managedRefreshPipelineActiveGeneration(t, seed.db, sid), before)
			}
			assertOpenCodeMatchesPlainParse(t, seed, openCodeDetailOfEvery(t, seed), nil, openCodeRouteRun{})
		},
		"relocated-host-slug-parses": func(t *testing.T) {
			seed := seedOpenCodeCurrentRoute(t, "ordinary", nil)
			sid := routeSessions(t, seed.db)[0]
			before := managedRefreshPipelineActiveGeneration(t, seed.db, sid)
			relocatedGit := &openCodeOracleGitResolver{
				remote:   "https://example.invalid/synthetic/relocated.git",
				branch:   openCodeOracleDefaultBranch,
				tracking: openCodeOracleDefaultTracking,
			}
			profiler := &ingest.IndexProfiler{}
			res := runOpenCodeRoute(t, seed, newHalfValidatingStore(seed.db), openCodeRouteRun{profiler: profiler, git: relocatedGit})
			assertRefreshCensusNativeAdapterBehind(t, res, 1)
			assertProofDoneZero(t, profiler, 0)
			if routeParses(res) != 1 || managedRefreshPipelineActiveGeneration(t, seed.db, sid) == before {
				t.Fatalf("parses=%d, want 1 parse and new generation", routeParses(res))
			}
			assertOpenCodeMatchesPlainParse(t, seed, openCodeDetailOfEvery(t, seed), nil, openCodeRouteRun{git: relocatedGit})
		},
		"redacted-unchanged-config-preserves-redacted-at-and-settles": func(t *testing.T) {
			engine, err := redact.NewRedactor(redact.Standard, nil, redact.XDGPaths{})
			if err != nil {
				t.Fatal(err)
			}
			seed := seedOpenCodeCurrentRoute(t, "ordinary", engine)
			sid := routeSessions(t, seed.db)[0]
			beforeRedactedAt := readRouteRedactedAtMs(t, seed.managedDir, sid)
			if beforeRedactedAt == nil {
				t.Fatal("baseline RedactedAtMs is nil")
			}
			observed := newHalfValidatingStore(seed.db)
			profiler := &ingest.IndexProfiler{}
			res := runOpenCodeRoute(t, seed, observed, openCodeRouteRun{profiler: profiler, redactor: engine})
			assertProofDoneZero(t, profiler, 1)
			if total, done := routeStage(profiler, ingest.StageRefreshValidate); total != 1 || done != 1 {
				t.Fatalf("refresh_validate total=%d done=%d, want 1 and 1; validator outcomes %+v", total, done, observed.outcomes)
			}
			if len(observed.settled) != 1 || routeParses(res) != 0 {
				t.Fatalf("settled=%v parses=%d, want 1 settle and 0 parses", observed.settled, routeParses(res))
			}
			afterRedactedAt := readRouteRedactedAtMs(t, seed.managedDir, sid)
			if afterRedactedAt == nil || *afterRedactedAt != *beforeRedactedAt {
				t.Fatalf("RedactedAtMs changed across unchanged redactor config: before=%v after=%v", beforeRedactedAt, afterRedactedAt)
			}
			assertOpenCodeMatchesPlainParse(t, seed, openCodeDetailOfEvery(t, seed), nil, openCodeRouteRun{redactor: engine})
		},
		"redacted-changed-config-updates-redacted-at-and-references": func(t *testing.T) {
			engine, err := redact.NewRedactor(redact.Standard, nil, redact.XDGPaths{})
			if err != nil {
				t.Fatal(err)
			}
			seed := seedOpenCodeCurrentRoute(t, "ordinary", engine)
			sid := routeSessions(t, seed.db)[0]
			before := managedRefreshPipelineActiveGeneration(t, seed.db, sid)
			changedRedactor := &routeVersionRedactor{Redactor: engine, version: "99.0.0"}
			observed := newReferenceRouteStore(seed.db)
			profiler := &ingest.IndexProfiler{}
			res := runOpenCodeRoute(t, seed, observed, openCodeRouteRun{profiler: profiler, redactor: changedRedactor})
			assertProofDoneZero(t, profiler, 1)
			if total, done := routeStage(profiler, ingest.StageRefreshValidate); total != 1 || done != 0 {
				t.Fatalf("refresh_validate total=%d done=%d, want 1 and 0", total, done)
			}
			if total, done := routeStage(profiler, ingest.StageRefreshReference); total != 1 || done != 1 {
				t.Fatalf("refresh_reference total=%d done=%d, want 1 and 1", total, done)
			}
			if routeParses(res) != 0 || managedRefreshPipelineActiveGeneration(t, seed.db, sid) == before {
				t.Fatalf("parses=%d, want 0 parses and new reference generation", routeParses(res))
			}
			assertOpenCodeMatchesPlainParse(t, seed, openCodeDetailOfEvery(t, seed), nil, openCodeRouteRun{redactor: changedRedactor})
		},
		"validator-error-parses": func(t *testing.T) {
			seed := seedOpenCodeCurrentRoute(t, "ordinary", nil)
			failing := &failingValidatorStore{halfValidatingStore: newHalfValidatingStore(seed.db)}
			profiler := &ingest.IndexProfiler{}
			res := runOpenCodeRoute(t, seed, failing, openCodeRouteRun{profiler: profiler})
			assertProofDoneZero(t, profiler, 1)
			if total, _ := routeStage(profiler, ingest.StageRefreshValidateUnsupported); total != 1 {
				t.Fatalf("refresh_validate_unsupported total=%d, want 1", total)
			}
			if routeParses(res) != 1 {
				t.Fatalf("parses=%d, want 1", routeParses(res))
			}
			assertOpenCodeMatchesPlainParse(t, seed, openCodeDetailOfEvery(t, seed), nil, openCodeRouteRun{})
		},
		"native-source-changed-parses": func(t *testing.T) {
			seed := seedOpenCodeCurrentRoute(t, "ordinary", nil)
			execNativeOpenCodeSQL(t, seed.nativeDBPath,
				`INSERT INTO session_message (id, session_id, type, time_created, time_updated, data, seq) VALUES ('msg_before_extract', ?1, 'user', 3000, 3000, '{"id":"msg_before_extract","type":"user","text":"before extract","time":{"created":3000}}', 2)`,
				openCodeOracleOrdinaryID)
			seed.snapshot(t)
			profiler := &ingest.IndexProfiler{}
			res := runOpenCodeRoute(t, seed, newHalfValidatingStore(seed.db), openCodeRouteRun{profiler: profiler})
			assertProofDoneZero(t, profiler, 0)
			if routeParses(res) != 1 {
				t.Fatalf("parses=%d, want 1", routeParses(res))
			}
			assertOpenCodeMatchesPlainParse(t, seed, openCodeDetailOfEvery(t, seed), nil, openCodeRouteRun{})
		},
		"legacy-sqlite-stays-format-1": func(t *testing.T) {
			seed := seedLegacySQLiteRoute(t)
			profiler := &ingest.IndexProfiler{}
			res := runOpenCodeRoute(t, seed, newHalfValidatingStore(seed.db), openCodeRouteRun{profiler: profiler, versions: openCodeFormat1Versions(openCodeFormat1AdapterVersion())})
			assertProofDoneZero(t, profiler, 0)
			if res.Summary.Errors != 0 || routeParses(res) != 1 {
				t.Fatalf("legacy sqlite refresh: parses=%d summary=%+v, want 1 parse and no errors", routeParses(res), res.Summary)
			}
			for _, row := range managedRefreshPipelineRows(t, seed.db, `SELECT index_format_version FROM sessions WHERE session_id = ?`, openCodeOracleOrdinaryID) {
				if row != strconv.Itoa(openCodeLegacyIndexFormat) {
					t.Fatalf("legacy sqlite index_format_version = %s, want 1", row)
				}
			}
		},
		"json-tree-stays-format-1": func(t *testing.T) {
			seed := seedJSONTreeRoute(t)
			profiler := &ingest.IndexProfiler{}
			res := runOpenCodeRoute(t, seed, newHalfValidatingStore(seed.db), openCodeRouteRun{profiler: profiler, versions: openCodeFormat1Versions(openCodeFormat1AdapterVersion())})
			assertProofDoneZero(t, profiler, 0)
			if res.Summary.Errors != 0 || routeParses(res) != 1 {
				t.Fatalf("json tree refresh: parses=%d summary=%+v, want 1 parse and no errors", routeParses(res), res.Summary)
			}
			for _, row := range managedRefreshPipelineRows(t, seed.db, `SELECT index_format_version FROM sessions WHERE session_id = ?`, openCodeOracleOrdinaryID) {
				if row != strconv.Itoa(openCodeLegacyIndexFormat) {
					t.Fatalf("json tree index_format_version = %s, want 1", row)
				}
			}
		},
		"control-only-current-falls-back-to-legacy-format-1": func(t *testing.T) {
			seed := seedControlOnlyCurrentFallbackRoute(t)
			profiler := &ingest.IndexProfiler{}
			res := runOpenCodeRoute(t, seed, newHalfValidatingStore(seed.db), openCodeRouteRun{profiler: profiler, versions: openCodeFormat1Versions(openCodeFormat1AdapterVersion())})
			assertProofDoneZero(t, profiler, 0)
			if res.Summary.Errors != 0 || routeParses(res) != 1 {
				t.Fatalf("control-only current fallback refresh: parses=%d summary=%+v, want 1 parse and no errors", routeParses(res), res.Summary)
			}
			for _, row := range managedRefreshPipelineRows(t, seed.db, `SELECT index_format_version FROM sessions WHERE session_id = ?`, openCodeOracleOrdinaryID) {
				if row != strconv.Itoa(openCodeLegacyIndexFormat) {
					t.Fatalf("control-only current fallback index_format_version = %s, want 1", row)
				}
			}
		},
		"undeclared-opencode-adapter-parses": func(t *testing.T) {
			seed := seedOpenCodeCurrentRoute(t, "ordinary", nil)
			profiler := &ingest.IndexProfiler{}
			res := runOpenCodeRoute(t, seed, newHalfValidatingStore(seed.db), openCodeRouteRun{profiler: profiler, withhold: true})
			assertProofDoneZero(t, profiler, 0)
			if routeParses(res) != 1 {
				t.Fatalf("parses=%d, want 1", routeParses(res))
			}
			assertOpenCodeMatchesPlainParse(t, seed, openCodeDetailOfEvery(t, seed), nil, openCodeRouteRun{withhold: true})
		},
		"incomplete-over-complete-refuses-and-preserves-active": func(t *testing.T) {
			seed := seedOpenCodeCurrentRoute(t, "ordinary", nil)
			sid := routeSessions(t, seed.db)[0]
			before := managedRefreshPipelineActiveGeneration(t, seed.db, sid)
			beforeDetail := openCodeSessionDetailBytes(t, seed, sid)
			observed := newHalfValidatingStore(seed.db)
			halves := newHalfRecorder()
			var once sync.Once
			injectIncomplete := func() {
				once.Do(func() {
					execNativeOpenCodeSQL(t, seed.nativeDBPath,
						`INSERT INTO session_message (id, session_id, type, time_created, time_updated, data, seq) VALUES ('msg_inc_route', ?1, 'assistant', 3000, 3000, '{"id":"msg_inc_route","type":"assistant","agent":"build","model":{"id":"synthetic-model","providerID":"synthetic-provider"},"content":[{"type":"tool","id":"call_inc_route","name":"read","state":{"status":"streaming","input":"partial"},"time":{"created":3000}}],"time":{"created":3000}}', 2)`,
						openCodeOracleOrdinaryID)
				})
			}
			prove := &openCodeOracleProveSeam{halfValidatingStore: observed, before: injectIncomplete}
			profiler := &ingest.IndexProfiler{}
			runOpenCodeRoute(t, seed, prove, openCodeRouteRun{profiler: profiler, halves: halves, beforeBuild: injectIncomplete})
			assertProofDoneZero(t, profiler, 1)
			var incErr *ingest.OpenCodeIncompleteProvenanceError
			if !errors.As(halves.refusal(sid), &incErr) {
				t.Fatalf("half refusal = %v, want *OpenCodeIncompleteProvenanceError", halves.refusal(sid))
			}
			if total, _ := routeStage(profiler, ingest.StageRefreshValidateUnsupported); total != 1 {
				t.Fatalf("refresh_validate_unsupported total=%d, want 1", total)
			}
			if after := managedRefreshPipelineActiveGeneration(t, seed.db, sid); after != before {
				t.Fatalf("active generation moved on incomplete refusal: %s -> %s", before, after)
			}
			if afterDetail := openCodeSessionDetailBytes(t, seed, sid); !bytes.Equal(afterDetail, beforeDetail) {
				t.Fatalf("session_detail changed on incomplete refusal")
			}
		},
		"disable-refresh-skips-parses": func(t *testing.T) {
			seed := seedOpenCodeCurrentRoute(t, "ordinary", nil)
			profiler := &ingest.IndexProfiler{}
			res := runOpenCodeRoute(t, seed, newHalfValidatingStore(seed.db), openCodeRouteRun{profiler: profiler, disableSkips: true})
			assertProofDoneZero(t, profiler, 0)
			if routeParses(res) != 1 {
				t.Fatalf("parses=%d, want 1", routeParses(res))
			}
		},
		"force-parses": func(t *testing.T) {
			seed := seedOpenCodeCurrentRoute(t, "ordinary", nil)
			profiler := &ingest.IndexProfiler{}
			res := runOpenCodeRoute(t, seed, newHalfValidatingStore(seed.db), openCodeRouteRun{profiler: profiler, force: true})
			assertProofDoneZero(t, profiler, 0)
			if routeParses(res) != 1 {
				t.Fatalf("parses=%d, want 1", routeParses(res))
			}
		},
		"reindex-parses": func(t *testing.T) {
			seed := seedOpenCodeCurrentRoute(t, "ordinary", nil)
			profiler := &ingest.IndexProfiler{}
			res := runOpenCodeRoute(t, seed, newHalfValidatingStore(seed.db), openCodeRouteRun{profiler: profiler, reindex: true})
			assertProofDoneZero(t, profiler, 0)
			if routeParses(res) != 1 {
				t.Fatalf("parses=%d, want 1", routeParses(res))
			}
		},
	}
}

func TestOpenCodeManagedRefreshRoutes(t *testing.T) {
	var fixture struct {
		Cases []struct {
			Name  string `yaml:"name"`
			Given string `yaml:"given"`
			Then  string `yaml:"then"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(openCodeManagedRefreshRoutesYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(openCodeManagedRefreshRoutesManifestYAML, "opencode managed refresh routes")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range fixture.Cases {
		if c.Given == "" || c.Then == "" || slices.Contains(names, c.Name) {
			t.Fatalf("route %q is duplicated or misses given/then", c.Name)
		}
		names = append(names, c.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "opencode managed refresh routes"); err != nil {
		t.Fatal(err)
	}
	routes := openCodeManagedRefreshRoutes()
	if got := slices.Sorted(maps.Keys(routes)); !slices.Equal(got, slices.Sorted(slices.Values(names))) {
		t.Fatalf("route functions %v differ from fixture %v", got, names)
	}
	for _, name := range names {
		t.Run(name, routes[name])
	}
}
