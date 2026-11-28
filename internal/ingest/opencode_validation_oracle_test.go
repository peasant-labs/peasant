package ingest_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/opencode_validation_oracle.yaml
var openCodeValidationOracleYAML []byte

//go:embed testdata/opencode_validation_oracle.manifest.yaml
var openCodeValidationOracleManifestYAML []byte

const (
	openCodeOracleOrdinaryID      = "ses_3cd91f52effeXd3QAJ54jOyzn1"
	openCodeOracleForkParentID    = "ses_3cd91f52effeXd3QAJ54jOrc01"
	openCodeOracleForkChildID     = "ses_3cd91f52effeXd3QAJ54jOrc02"
	openCodeOracleSubagentForkID  = "ses_3cd91f52effeXd3QAJ54jOrc03"
	openCodeOracleProjectID       = "proj_oracle_1"
	openCodeOracleProjectName     = "oracle-project"
	openCodeOracleDefaultRemote   = "https://example.invalid/synthetic/before.git"
	openCodeOracleDefaultBranch   = "main"
	openCodeOracleDefaultTracking = "origin/main"
)

type openCodeOracleCase struct {
	Name                          string                       `yaml:"name"`
	Corpus                        string                       `yaml:"corpus"`
	Change                        oracleChange                 `yaml:"change"`
	DeleteParentRowsBeforeRefresh string                       `yaml:"deleteParentRowsBeforeRefresh"`
	PostExtractMutation           string                       `yaml:"postExtractMutation"`
	Expect                        map[string]oracleExpectation `yaml:"expect"`
	MayBeNotAsked                 string                       `yaml:"mayBeNotAsked"`

	inventory *metadataInventoryRow
}

type openCodeOracleDocument struct {
	Cases []openCodeOracleCase `yaml:"cases"`
}

func loadOpenCodeOracleDocument(t *testing.T) openCodeOracleDocument {
	t.Helper()
	var document openCodeOracleDocument
	if err := testutil.DecodeFixtureYAML(openCodeValidationOracleYAML, &document); err != nil {
		t.Fatalf("decode OpenCode validator oracle fixture: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(openCodeValidationOracleManifestYAML, "opencode validation oracle")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(document.Cases))
	for _, c := range document.Cases {
		names = append(names, c.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "opencode validation oracle"); err != nil {
		t.Fatal(err)
	}
	return document
}

// openCodeOracleGitResolver is a deterministic GitResolver for the OpenCode oracle
// that keeps ProjectWorktree fixed while allowing Branch and TrackingBranch to vary.
type openCodeOracleGitResolver struct {
	remote   string
	branch   string
	tracking string
	worktree string
}

var (
	_ ingest.GitResolver                  = (*openCodeOracleGitResolver)(nil)
	_ ingest.RecordedBranchRemoteResolver = (*openCodeOracleGitResolver)(nil)
)

func (g *openCodeOracleGitResolver) RemoteURL(context.Context, string) (string, error) {
	return g.remote, nil
}

func (g *openCodeOracleGitResolver) Branch(context.Context, string) (string, error) {
	return g.branch, nil
}

func (g *openCodeOracleGitResolver) Worktree(context.Context, string) (string, error) {
	return g.worktree, nil
}

func (g *openCodeOracleGitResolver) TrackingBranch(context.Context, string) (string, error) {
	return g.tracking, nil
}

func (g *openCodeOracleGitResolver) UserEmail(context.Context) (string, error) {
	return testutil.TestEmail, nil
}

func (g *openCodeOracleGitResolver) WalkUpRemoteURL(context.Context, string) (string, string, error) {
	return "", "", nil
}

func (g *openCodeOracleGitResolver) RemoteURLForBranch(context.Context, string, string) (string, string, error) {
	return g.remote, g.tracking, nil
}

func (g *openCodeOracleGitResolver) OriginRemoteURL(context.Context, string) (string, error) {
	return g.remote, nil
}

// seamOpenCodeIndexer captures the RecordedMetadata observed at the validator/builder
// seam (after any post-extraction mutation) and runs a pre-build seam for Fork P.
type seamOpenCodeIndexer struct {
	*ingest.OpenCodeIndexer
	before     func()
	halves     *halfRecorder
	mutateMeta func(*schema.UnifiedMetadata)
	recordedMu *sync.Mutex
	recorded   map[ingest.SessionID]schema.UnifiedMetadata
	mutated    map[*schema.UnifiedMetadata]bool
}

var (
	_ ingest.GenerationBuilder         = (*seamOpenCodeIndexer)(nil)
	_ ingest.GenerationMetadataBuilder = (*seamOpenCodeIndexer)(nil)
)

func (i *seamOpenCodeIndexer) applyPostExtract(in ingest.GenerationBuildInput) {
	if in.RecordedMetadata == nil {
		return
	}
	if i.mutateMeta != nil && i.recordedMu != nil {
		i.recordedMu.Lock()
		if !i.mutated[in.RecordedMetadata] {
			i.mutateMeta(in.RecordedMetadata)
			i.mutated[in.RecordedMetadata] = true
		}
		i.recordedMu.Unlock()
	}
	if i.recordedMu != nil && i.recorded != nil {
		i.recordedMu.Lock()
		i.recorded[in.Session.SessionID] = *in.RecordedMetadata
		i.recordedMu.Unlock()
	}
}

func (i *seamOpenCodeIndexer) BuildGenerationMetadata(ctx context.Context, in ingest.GenerationBuildInput) (ingest.GenerationMetadataCandidate, error) {
	i.applyPostExtract(in)
	candidate, err := i.OpenCodeIndexer.BuildGenerationMetadata(ctx, in)
	i.halves.record(in.Session.SessionID, err)
	return candidate, err
}

func (i *seamOpenCodeIndexer) BuildGeneration(ctx context.Context, in ingest.GenerationBuildInput) (ingest.GenerationCandidate, error) {
	if i.before != nil {
		i.before()
	}
	i.applyPostExtract(in)
	return i.OpenCodeIndexer.BuildGeneration(ctx, in)
}

// openCodeOracleProveSeam runs a post-EXTRACT+WRITE / pre-validation seam and
// optionally applies post-extraction metadata mutations to the proof request.
type openCodeOracleProveSeam struct {
	*halfValidatingStore
	before     func()
	mutateMeta func(*schema.UnifiedMetadata)
}

var (
	_ ingest.ManagedGenerationUnchangedProver = (*openCodeOracleProveSeam)(nil)
	_ ingest.ManagedGenerationValidator       = (*openCodeOracleProveSeam)(nil)
)

func (s *openCodeOracleProveSeam) ProveCapturedGenerationUnchanged(ctx context.Context, request ingest.ManagedGenerationUnchangedProof) (ingest.UnchangedProof, error) {
	if s.before != nil {
		s.before()
	}
	if s.mutateMeta != nil {
		s.mutateMeta(&request.Metadata)
	}
	return s.halfValidatingStore.ProveCapturedGenerationUnchanged(ctx, request)
}

// openCodeGoldenSeed holds a pre-imported OpenCode SQLite DB, Peasant store,
// managed artifact tree, repo directory, and baseline RecordedMetadata per session.
type openCodeGoldenSeed struct {
	nativeDBPath string
	peasantDBDir string
	managedDir   string
	repoDir      string
	baselineMeta map[string]schema.UnifiedMetadata
}

// openCodeOracleCorpus names the native corpus a case seeds; the empty name is
// the ordinary corpus.
func openCodeOracleCorpus(name string) string {
	switch name {
	case "", "ordinary":
		return "ordinary"
	case "fork", "subagent-fork":
		return name
	default:
		panic("unknown OpenCode oracle corpus: " + name)
	}
}

func createOpenCodeOracleNativeDB(t *testing.T, dbPath string, repoDir string, corpus string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	conn, err := sqlite.OpenConn(dbPath, sqlite.OpenReadWrite|sqlite.OpenCreate)
	if err != nil {
		t.Fatalf("open native opencode sqlite %s: %v", dbPath, err)
	}
	defer func() { _ = conn.Close() }()

	ddl := `
CREATE TABLE project (
	id TEXT PRIMARY KEY,
	worktree TEXT,
	vcs TEXT,
	name TEXT
);
CREATE TABLE project_directory (
	project_id TEXT NOT NULL,
	directory TEXT NOT NULL,
	type TEXT
);
CREATE TABLE session_v2 (
	id TEXT PRIMARY KEY,
	parent_id TEXT,
	time_created INTEGER NOT NULL,
	time_updated INTEGER NOT NULL,
	directory TEXT,
	title TEXT,
	agent TEXT,
	tokens_input INTEGER,
	tokens_output INTEGER,
	tokens_reasoning INTEGER,
	tokens_cache_read INTEGER,
	tokens_cache_write INTEGER,
	cost REAL,
	version TEXT,
	slug TEXT,
	revert TEXT,
	project_id TEXT,
	fork_session_id TEXT,
	fork_boundary TEXT,
	model TEXT
);
CREATE TABLE session_message (
	id TEXT NOT NULL,
	session_id TEXT NOT NULL,
	type TEXT NOT NULL,
	time_created INTEGER NOT NULL,
	time_updated INTEGER NOT NULL,
	data TEXT NOT NULL,
	seq INTEGER NOT NULL,
	PRIMARY KEY (session_id, id)
);
CREATE INDEX session_message_seq_idx ON session_message(seq);
CREATE UNIQUE INDEX session_message_session_seq_idx ON session_message(session_id, seq);
`
	if err := sqlitex.ExecuteScript(conn, ddl, nil); err != nil {
		t.Fatalf("create oracle opencode schema: %v", err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if err := sqlitex.Execute(conn, query, &sqlitex.ExecOptions{Args: args}); err != nil {
			t.Fatalf("seed oracle opencode db (%s): %v", query, err)
		}
	}
	exec(`INSERT INTO project (id, worktree, vcs, name) VALUES (?1, ?2, 'git', ?3)`,
		openCodeOracleProjectID, repoDir, openCodeOracleProjectName)
	exec(`INSERT INTO project_directory (project_id, directory, type) VALUES (?1, ?2, 'git_worktree')`,
		openCodeOracleProjectID, repoDir)

	insertSession := func(id, parentID, forkSource, forkBoundary string) {
		var pID, fID, fB any
		if parentID != "" {
			pID = parentID
		}
		if forkSource != "" {
			fID = forkSource
		}
		if forkBoundary != "" {
			fB = forkBoundary
		}
		exec(`INSERT INTO session_v2 (id, parent_id, time_created, time_updated, directory, title, agent, tokens_input, tokens_output, cost, version, slug, project_id, fork_session_id, fork_boundary)
VALUES (?1, ?2, 1000, 2000, ?3, 'oracle session', 'build', 100, 50, 0.01, '1.0.0', 'oracle-session', ?4, ?5, ?6)`,
			id, pID, repoDir, openCodeOracleProjectID, fID, fB)
	}
	insertMsg := func(id, sid, typ string, seq int64, data string) {
		exec(`INSERT INTO session_message (id, session_id, type, time_created, time_updated, data, seq) VALUES (?1, ?2, ?3, 1000, 2000, ?4, ?5)`,
			id, sid, typ, data, seq)
	}

	switch corpus {
	case "ordinary":
		insertSession(openCodeOracleOrdinaryID, "", "", "")
		insertMsg("msg_ord_1", openCodeOracleOrdinaryID, "user", 0,
			`{"id":"msg_ord_1","type":"user","text":"oracle user question","time":{"created":1000}}`)
		insertMsg("msg_ord_2", openCodeOracleOrdinaryID, "assistant", 1,
			`{"id":"msg_ord_2","type":"assistant","agent":"build","model":{"id":"synthetic-model","providerID":"synthetic-provider"},"content":[{"type":"text","text":"oracle assistant answer"}],"rawFinish":"stop","time":{"created":1000,"completed":2000,"streamed":2000}}`)
	case "fork", "subagent-fork":
		insertSession(openCodeOracleForkParentID, "", "", "")
		for i, id := range []string{"msg_fp_1", "msg_fp_2", "msg_fp_3"} {
			insertMsg(id, openCodeOracleForkParentID, "user", int64(i),
				fmt.Sprintf(`{"id":%q,"type":"user","text":"parent question %d","time":{"created":1000}}`, id, i+1))
		}
		childID := openCodeOracleForkChildID
		parentID := ""
		if corpus == "subagent-fork" {
			childID = openCodeOracleSubagentForkID
			parentID = openCodeOracleForkParentID
		}
		insertSession(childID, parentID, openCodeOracleForkParentID, `{"type":"through","messageID":"msg_fp_2"}`)
		for i, id := range []string{"msg_cp_1", "msg_cp_2"} {
			insertMsg(id, childID, "user", int64(i),
				fmt.Sprintf(`{"id":%q,"type":"user","text":"parent question %d","time":{"created":1000}}`, id, i+1))
		}
		insertMsg("msg_co_1", childID, "assistant", 2,
			`{"id":"msg_co_1","type":"assistant","agent":"build","model":{"id":"synthetic-model","providerID":"synthetic-provider"},"content":[{"type":"text","text":"child answer"}],"rawFinish":"stop","time":{"created":1500,"completed":2000,"streamed":2000}}`)
	default:
		t.Fatalf("unknown oracle corpus %q", corpus)
	}
}

func buildOpenCodeGoldenSeed(t *testing.T, root, corpus string) openCodeGoldenSeed {
	t.Helper()
	dir := filepath.Join(root, corpus)
	repoDir := filepath.Join(dir, "repo")
	nativeDBPath := filepath.Join(dir, "native", "opencode.db")
	peasantDBDir := filepath.Join(dir, "peasant-db")
	managedDir := filepath.Join(dir, "managed")
	mustMkdir(t, repoDir)
	mustMkdir(t, peasantDBDir)
	mustMkdir(t, managedDir)

	createOpenCodeOracleNativeDB(t, nativeDBPath, repoDir, corpus)

	db := openManagedRefreshPipelineStore(t, filepath.Join(peasantDBDir, "peasant.db"), managedDir)
	fs := &ingest.OSFileSystem{}
	git := &openCodeOracleGitResolver{
		remote:   openCodeOracleDefaultRemote,
		branch:   openCodeOracleDefaultBranch,
		tracking: openCodeOracleDefaultTracking,
	}
	cfg := makePipelineConfig(managedDir)
	cfg.Sources = map[ingest.Harness]ingest.SourceConfig{
		ingest.HarnessOpenCode: {Paths: []ingest.ResolvedPath{ingest.ResolvedPath(filepath.Dir(nativeDBPath))}, Enabled: true},
	}
	cfg.StalenessThreshold = 0
	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessOpenCode: func(f ingest.FileSystem, g ingest.GitResolver, s salt.Salt) ingest.SourceAdapter {
			adapter, err := ingest.NewOpenCodeAdapterWithCandidateProbe(
				f, g, s, "latest",
				mountedCurrentEnvironment{"OPENCODE_DB": nativeDBPath},
				fs, ingest.OpenOpenCodeSQLiteSource, ingest.DefaultOpenCodeSQLiteSourceOptions(),
			)
			if err != nil {
				t.Fatalf("NewOpenCodeAdapterWithCandidateProbe: %v", err)
			}
			return adapter
		},
	}
	indexers := ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})
	v4 := openCodeMemoVersions(4)
	p, err := newTestPipeline(
		fs, git, adapters, cfg,
		ingest.WithStore(db),
		ingest.WithMetricsStore(db),
		ingest.WithSalt(db.InstallationSalt()),
		ingest.WithIndexLogger(db),
		ingest.WithIndexers(indexers),
		ingest.WithHarvesterVersions(v4),
	)
	if err != nil {
		t.Fatalf("newTestPipeline golden seed (%s): %v", corpus, err)
	}
	res, err := p.Run(t.Context())
	if err != nil || res.Summary.Errors != 0 || res.Summary.Indexed == 0 {
		t.Fatalf("golden seed Run (%s) failed: err=%v summary=%+v diagnostics=%+v", corpus, err, res.Summary, res.Diagnostics)
	}

	target := openCodeMemoVersions(5)[ingest.HarnessOpenCode].AdapterVersion - 1
	managedRefreshPipelineDowngrade(t, db, target)
	if err := db.Close(); err != nil {
		t.Fatalf("close golden store (%s): %v", corpus, err)
	}

	baselineMeta := map[string]schema.UnifiedMetadata{}
	err = filepath.WalkDir(managedDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || !strings.HasSuffix(entry.Name(), defaults.MetadataSuffix) {
			return walkErr
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		var meta schema.UnifiedMetadata
		if jsonErr := json.Unmarshal(data, &meta); jsonErr != nil {
			return jsonErr
		}
		baselineMeta[string(meta.SessionID)] = meta
		return nil
	})
	if err != nil {
		t.Fatalf("read golden baseline metadata (%s): %v", corpus, err)
	}

	return openCodeGoldenSeed{
		nativeDBPath: nativeDBPath,
		peasantDBDir: peasantDBDir,
		managedDir:   managedDir,
		repoDir:      repoDir,
		baselineMeta: baselineMeta,
	}
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = in.Close() }()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			_ = out.Close()
			return err
		}
		return out.Close()
	})
	if err != nil {
		t.Fatalf("copyTree %s -> %s: %v", src, dst, err)
	}
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

type openCodeOracleFork struct {
	nativeDBPath string
	managedDir   string
	db           *store.Store
	before       map[string]string
}

// openOpenCodeOracleFork opens a seed's own store in place. A copy would move
// the native database, and the native path is OpenCode metadata context
// (source path and project hash), so every copied case would read as a
// context change.
func openOpenCodeOracleFork(t *testing.T, seed openCodeGoldenSeed) openCodeOracleFork {
	t.Helper()
	db := openManagedRefreshPipelineStore(t, filepath.Join(seed.peasantDBDir, "peasant.db"), seed.managedDir)
	t.Cleanup(func() { _ = db.Close() })
	return openCodeOracleFork{
		nativeDBPath: seed.nativeDBPath,
		managedDir:   seed.managedDir,
		db:           db,
		before:       oracleActiveGenerations(t, db),
	}
}

func execNativeOpenCodeSQL(t *testing.T, nativeDBPath, query string, args ...any) {
	t.Helper()
	conn, err := sqlite.OpenConn(nativeDBPath, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatalf("open native opencode sqlite %s: %v", nativeDBPath, err)
	}
	defer func() { _ = conn.Close() }()
	if err := sqlitex.Execute(conn, query, &sqlitex.ExecOptions{Args: args}); err != nil {
		t.Fatalf("exec native opencode sql (%s): %v", query, err)
	}
}

// applyOpenCodeInventoryMutation applies the exact mutation mechanism required
// for each metadata_field_inventory.yaml leaf: native SQLite column, injected
// GitResolver / GitDiffAnalyzer, or post-extraction boundary.
func applyOpenCodeInventoryMutation(
	t *testing.T,
	row metadataInventoryRow,
	nativeDBPath string,
	git *openCodeOracleGitResolver,
) (gitAnalyzer ingest.GitDiffAnalyzer, postExtract func(*schema.UnifiedMetadata)) {
	t.Helper()
	if row.OpenCodeMutation != "" {
		row.Mutation = row.OpenCodeMutation
	}
	switch row.Field {
	// Native SQLite columns (session_message rows unchanged so kept.transcript == true).
	case "Project.Name":
		var name string
		if err := json.Unmarshal([]byte(row.Mutation), &name); err != nil {
			t.Fatal(err)
		}
		execNativeOpenCodeSQL(t, nativeDBPath, `UPDATE project SET name = ?1 WHERE id = ?2`, name, openCodeOracleProjectID)
		return nil, nil
	case "CWD":
		var cwd string
		if err := json.Unmarshal([]byte(row.Mutation), &cwd); err != nil {
			t.Fatal(err)
		}
		execNativeOpenCodeSQL(t, nativeDBPath, `UPDATE session_v2 SET directory = ?1 WHERE id = ?2`, cwd, openCodeOracleOrdinaryID)
		return nil, nil
	case "ParentUUID":
		var parent string
		if err := json.Unmarshal([]byte(row.Mutation), &parent); err != nil {
			t.Fatal(err)
		}
		execNativeOpenCodeSQL(t, nativeDBPath, `UPDATE session_v2 SET parent_id = ?1 WHERE id = ?2`, parent, openCodeOracleOrdinaryID)
		return nil, nil
	case "Version":
		var version string
		if err := json.Unmarshal([]byte(row.Mutation), &version); err != nil {
			t.Fatal(err)
		}
		execNativeOpenCodeSQL(t, nativeDBPath, `UPDATE session_v2 SET version = ?1 WHERE id = ?2`, version, openCodeOracleOrdinaryID)
		return nil, nil
	case "Stats.TokensIn":
		var tokens int64
		if err := json.Unmarshal([]byte(row.Mutation), &tokens); err != nil {
			t.Fatal(err)
		}
		execNativeOpenCodeSQL(t, nativeDBPath, `UPDATE session_v2 SET tokens_input = ?1 WHERE id = ?2`, tokens, openCodeOracleOrdinaryID)
		return nil, nil
	case "Stats.TokensOut":
		var tokens int64
		if err := json.Unmarshal([]byte(row.Mutation), &tokens); err != nil {
			t.Fatal(err)
		}
		execNativeOpenCodeSQL(t, nativeDBPath, `UPDATE session_v2 SET tokens_output = ?1 WHERE id = ?2`, tokens, openCodeOracleOrdinaryID)
		return nil, nil
	case "ThinkingLevel":
		execNativeOpenCodeSQL(t, nativeDBPath, `UPDATE session_v2 SET model = ?1 WHERE id = ?2`, `{"variant":"high"}`, openCodeOracleOrdinaryID)
		return nil, nil

	// Injected GitResolver / GitDiffAnalyzer (ProjectWorktree fixed so HostSlug stays fixed).
	case "Git.Branch":
		var branch string
		if err := json.Unmarshal([]byte(row.Mutation), &branch); err != nil {
			t.Fatal(err)
		}
		git.branch = branch
		return nil, nil
	case "Git.Tracking":
		var tracking string
		if err := json.Unmarshal([]byte(row.Mutation), &tracking); err != nil {
			t.Fatal(err)
		}
		git.tracking = tracking
		return nil, nil
	// Post-extraction boundary for fields OpenCodeAdapter does not independently
	// vary, single-leaf Redaction.* fields, or fields that would trigger
	// relocation / re-materialization in processNativeSessionWithMeta.
	// ThinkingLevelRaw and ThinkingLevelHistory are never filled at session
	// level by the adapter, and commit detection filters an injected analyzer's
	// commits by the session window and the user's email, so these leaves are
	// mutated here as well.
	default:
		// This runs on a pipeline parser goroutine: t.Fatal there would end
		// the worker before it reports its batch and hang the run.
		return nil, func(meta *schema.UnifiedMetadata) {
			if err := setInventoryMutation(row, string(meta.SessionID), meta); err != nil {
				t.Error(err)
				return
			}
			if row.Field != "MetadataHash" {
				meta.MetadataHash = schema.ComputeMetadataHash(meta)
			}
		}
	}
}

func applyOpenCodeOracleCaseMutation(
	t *testing.T,
	c openCodeOracleCase,
	fork openCodeOracleFork,
	git *openCodeOracleGitResolver,
) (gitAnalyzer ingest.GitDiffAnalyzer, postExtract func(*schema.UnifiedMetadata), postExtractSeam func()) {
	t.Helper()
	if c.inventory != nil {
		ga, pe := applyOpenCodeInventoryMutation(t, *c.inventory, fork.nativeDBPath, git)
		return ga, pe, nil
	}
	if c.DeleteParentRowsBeforeRefresh != "" {
		execNativeOpenCodeSQL(t, fork.nativeDBPath, `DELETE FROM session_message WHERE session_id = ?1`, c.DeleteParentRowsBeforeRefresh)
	}
	switch c.Change {
	case oracleChangeRecordStaleStats:
		postExtract = func(meta *schema.UnifiedMetadata) {
			in, conv := int64(902), int64(903)
			meta.Stats.TurnCount = 901
			meta.Stats.InputSubmissionCount = &in
			meta.Stats.ConversationTurnCount = &conv
			meta.MetadataHash = schema.ComputeMetadataHash(meta)
		}
	case oracleChangeNone, "":
	default:
		t.Fatalf("unknown oracle change %q", c.Change)
	}
	switch c.PostExtractMutation {
	case "":
	case "append-message":
		postExtractSeam = func() {
			execNativeOpenCodeSQL(t, fork.nativeDBPath,
				`INSERT INTO session_message (id, session_id, type, time_created, time_updated, data, seq) VALUES ('msg_appended_after_extract', ?1, 'user', 3000, 3000, '{"id":"msg_appended_after_extract","type":"user","text":"appended after extract","time":{"created":3000}}', 2)`,
				openCodeOracleOrdinaryID)
		}
	case "remove-db":
		postExtractSeam = func() {
			if err := os.Remove(fork.nativeDBPath); err != nil && !os.IsNotExist(err) {
				t.Error(err)
			}
		}
	case "append-incomplete-message":
		postExtractSeam = func() {
			execNativeOpenCodeSQL(t, fork.nativeDBPath,
				`INSERT INTO session_message (id, session_id, type, time_created, time_updated, data, seq) VALUES ('msg_incomplete_after_extract', ?1, 'assistant', 3000, 3000, '{"id":"msg_incomplete_after_extract","type":"assistant","agent":"build","model":{"id":"synthetic-model","providerID":"synthetic-provider"},"content":[{"type":"tool","id":"call_inc_stream","name":"read","state":{"status":"streaming","input":"partial"},"time":{"created":3000}}],"time":{"created":3000}}', 2)`,
				openCodeOracleOrdinaryID)
		}
	default:
		t.Fatalf("unknown postExtractMutation %q", c.PostExtractMutation)
	}
	return nil, postExtract, postExtractSeam
}

func runOpenCodeOraclePipeline(
	t *testing.T,
	db managedRefreshPipelineStore,
	nativeDBPath string,
	managedDir string,
	git ingest.GitResolver,
	gitAnalyzer ingest.GitDiffAnalyzer,
	disableSkips bool,
	beforeBuild func(),
	halves *halfRecorder,
	mutateMeta func(*schema.UnifiedMetadata),
	recordedMu *sync.Mutex,
	recorded map[ingest.SessionID]schema.UnifiedMetadata,
) *ingest.PipelineResult {
	t.Helper()
	fs := &ingest.OSFileSystem{}
	cfg := makePipelineConfig(managedDir)
	if validator, ok := db.(ingest.ManagedGenerationValidator); ok {
		cfg.Validator = validator
	}
	cfg.Sources = map[ingest.Harness]ingest.SourceConfig{
		ingest.HarnessOpenCode: {Paths: []ingest.ResolvedPath{ingest.ResolvedPath(filepath.Dir(nativeDBPath))}, Enabled: true},
	}
	cfg.StalenessThreshold = 0
	cfg.DisableRefreshSkips = disableSkips
	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessOpenCode: func(f ingest.FileSystem, g ingest.GitResolver, s salt.Salt) ingest.SourceAdapter {
			adapter, err := ingest.NewOpenCodeAdapterWithCandidateProbe(
				f, g, s, "latest",
				mountedCurrentEnvironment{"OPENCODE_DB": nativeDBPath},
				fs, ingest.OpenOpenCodeSQLiteSource, ingest.DefaultOpenCodeSQLiteSourceOptions(),
			)
			if err != nil {
				t.Fatalf("NewOpenCodeAdapterWithCandidateProbe: %v", err)
			}
			return adapter
		},
	}
	indexers := ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})
	indexers[ingest.HarnessOpenCode] = &seamOpenCodeIndexer{
		OpenCodeIndexer: ingest.NewOpenCodeIndexer(fs),
		before:          beforeBuild,
		halves:          halves,
		mutateMeta:      mutateMeta,
		recordedMu:      recordedMu,
		recorded:        recorded,
		mutated:         map[*schema.UnifiedMetadata]bool{},
	}
	opts := []ingest.PipelineOption{
		ingest.WithStore(db),
		ingest.WithMetricsStore(db),
		ingest.WithSalt(db.InstallationSalt()),
		ingest.WithIndexers(indexers),
		ingest.WithLogger(db),
		ingest.WithIndexLogger(db),
		ingest.WithHarvesterVersions(openCodeMemoVersions(5)),
	}
	if gitAnalyzer != nil {
		opts = append(opts, ingest.WithGitDiffAnalyzer(gitAnalyzer))
	}
	pipeline, err := newTestPipeline(fs, git, adapters, cfg, opts...)
	if err != nil {
		t.Fatalf("build OpenCode oracle pipeline: %v", err)
	}
	result, err := pipeline.Run(t.Context())
	if err != nil {
		t.Fatalf("run OpenCode oracle pipeline: %v", err)
	}
	return result
}

func openCodeOracleGenerationStats(t *testing.T, db *store.Store) map[string]string {
	t.Helper()
	return oracleStats(t, db)
}

func runOpenCodeOracleCase(t *testing.T, c openCodeOracleCase) oracleObservation {
	t.Helper()
	corpus := openCodeOracleCorpus(c.Corpus)
	seed := buildOpenCodeGoldenSeed(t, t.TempDir(), corpus)

	// Fork V: validator + settle/reference enabled.
	vFork := openOpenCodeOracleFork(t, seed)
	vGit := &openCodeOracleGitResolver{
		remote:   openCodeOracleDefaultRemote,
		branch:   openCodeOracleDefaultBranch,
		tracking: openCodeOracleDefaultTracking,
	}
	vAnalyzer, vMutate, vSeam := applyOpenCodeOracleCaseMutation(t, c, vFork, vGit)
	validating := newHalfValidatingStore(vFork.db)
	if c.Change == oracleChangeRecordStaleStats {
		validating.staleRecordedStats = map[ingest.SessionID]bool{
			ingest.SessionID(openCodeOracleOrdinaryID): true,
		}
	}
	halves := newHalfRecorder()
	var vRecordedMu sync.Mutex
	vRecorded := map[ingest.SessionID]schema.UnifiedMetadata{}
	var vOnce sync.Once
	vBefore := func() {
		if vSeam != nil {
			vOnce.Do(vSeam)
		}
	}
	prove := &openCodeOracleProveSeam{
		halfValidatingStore: validating,
		before:              vBefore,
		mutateMeta:          vMutate,
	}
	runOpenCodeOraclePipeline(t, prove, vFork.nativeDBPath, vFork.managedDir, vGit, vAnalyzer, false, vBefore, halves, vMutate, &vRecordedMu, vRecorded)
	validating.assertWiring(t)

	// Non-vacuity assertion at the validator/builder seam for every inventory case.
	if c.inventory != nil {
		observedMeta, ok := vRecorded[ingest.SessionID(openCodeOracleOrdinaryID)]
		if !ok {
			t.Fatalf("inventory row %s: no RecordedMetadata observed at validator/builder seam", c.inventory.Field)
		}
		baseMeta := seed.baselineMeta[openCodeOracleOrdinaryID]
		beforeLeaf, err := metadataLeaf(&baseMeta, c.inventory.Field)
		if err != nil {
			t.Fatal(err)
		}
		afterLeaf, err := metadataLeaf(&observedMeta, c.inventory.Field)
		if err != nil {
			t.Fatal(err)
		}
		if reflect.DeepEqual(beforeLeaf.Interface(), afterLeaf.Interface()) {
			t.Fatalf("inventory row %s: non-vacuity failed at validator/builder seam (before == after == %v)", c.inventory.Field, beforeLeaf.Interface())
		}
	}

	// Fork P: plain parse (DisableRefreshSkips = true).
	pFork := openOpenCodeOracleFork(t, buildOpenCodeGoldenSeed(t, t.TempDir(), corpus))
	pGit := &openCodeOracleGitResolver{
		remote:   openCodeOracleDefaultRemote,
		branch:   openCodeOracleDefaultBranch,
		tracking: openCodeOracleDefaultTracking,
	}
	pAnalyzer, pMutate, pSeam := applyOpenCodeOracleCaseMutation(t, c, pFork, pGit)
	var pRecordedMu sync.Mutex
	pRecorded := map[ingest.SessionID]schema.UnifiedMetadata{}
	var pOnce sync.Once
	pBefore := func() {
		if pSeam != nil {
			pOnce.Do(pSeam)
		}
	}
	parsed := runOpenCodeOraclePipeline(t, pFork.db, pFork.nativeDBPath, pFork.managedDir, pGit, pAnalyzer, true, pBefore, nil, pMutate, &pRecordedMu, pRecorded)

	observed := oracleObservation{}
	sessions := map[string]bool{}
	for sid := range vFork.before {
		sessions[sid] = true
	}
	for sid := range pFork.before {
		sessions[sid] = true
	}
	for sid := range oracleActiveGenerations(t, pFork.db) {
		sessions[sid] = true
	}
	vStats, pStats := oracleStats(t, vFork.db), oracleStats(t, pFork.db)
	vGenStats, pGenStats := openCodeOracleGenerationStats(t, vFork.db), openCodeOracleGenerationStats(t, pFork.db)
	for sid := range sessions {
		pSeed := oracleSeed{db: pFork.db, before: pFork.before}
		o := oracleSessionObservation{
			oracleExpectation: oracleExpectation{
				Validator: oracleValidatorOutcome(validating, halves, sid),
				Parse:     oracleParseOutcome(t, pSeed, parsed, sid),
			},
			statsDiffer: vStats[sid] != pStats[sid] || vGenStats[sid] != pGenStats[sid],
		}
		if result, asked := validating.outcome(ingest.SessionID(sid)); asked {
			o.Mismatch = result.Mismatch
		}
		observed[sid] = o
	}
	return observed
}

func openCodeOracleCases(t *testing.T, document openCodeOracleDocument) []openCodeOracleCase {
	t.Helper()
	var cases []openCodeOracleCase
	for _, row := range loadMetadataInventory(t) {
		cases = append(cases, openCodeOracleCase{Name: "inventory/" + row.Field, Corpus: "ordinary", inventory: &row})
	}
	return append(cases, document.Cases...)
}

func openCodeOracleViolations(c openCodeOracleCase, observed oracleObservation) []string {
	return oracleViolations(oracleCase{Name: c.Name, MayBeNotAsked: c.MayBeNotAsked}, observed)
}

// TestOpenCodeValidatorAgreesWithTheParseSkip is the validator oracle on real
// OpenCode stores: for every metadata_field_inventory.yaml leaf and every
// named fork/boundary case, proven implies the parse would skip, and a parse
// skip with a supported validator implies proven, with stats parity.
// contextChangesFixturePath is the store's context-change fixture, relative to
// the module root.
const contextChangesFixturePath = "internal/store/testdata/context_changes.yaml"

func TestOpenCodeValidatorAgreesWithTheParseSkip(t *testing.T) {
	document := loadOpenCodeOracleDocument(t)
	contextChangesBytes, err := os.ReadFile(filepath.Join(moduleRootFromIngest, contextChangesFixturePath))
	if err != nil {
		t.Fatal(err)
	}
	var changes struct {
		Harnesses     []string `yaml:"harnesses"`
		RequiredNames []string `yaml:"requiredNames"`
		Changes       []struct {
			Name   string            `yaml:"name"`
			Field  string            `yaml:"field"`
			Before string            `yaml:"before"`
			After  string            `yaml:"after"`
			Routes map[string]string `yaml:"routes"`
		} `yaml:"changes"`
	}
	if err := testutil.DecodeFixtureYAML(contextChangesBytes, &changes); err != nil {
		t.Fatal(err)
	}
	inventory := loadMetadataInventory(t)
	_ = inventory
	inventoryByJSON := map[string]bool{}
	var jsonFields []string
	var collectJSON func(reflect.Type, string)
	collectJSON = func(typ reflect.Type, prefix string) {
		for i := range typ.NumField() {
			f := typ.Field(i)
			if !f.IsExported() {
				continue
			}
			// ast-grep-ignore: no-bare-format-literals
			tag := strings.Split(f.Tag.Get("json"), ",")[0]
			if tag == "" || tag == "-" {
				tag = f.Name
			}
			kind := f.Type
			if kind.Kind() == reflect.Pointer && kind.Elem().Kind() == reflect.Struct {
				kind = kind.Elem()
			}
			if kind.Kind() == reflect.Struct {
				collectJSON(kind, prefix+tag+".")
				continue
			}
			jsonFields = append(jsonFields, prefix+tag)
			inventoryByJSON[prefix+tag] = true
		}
	}
	collectJSON(reflect.TypeOf(schema.UnifiedMetadata{}), "")
	for _, ch := range changes.Changes {
		if !inventoryByJSON[ch.Field] {
			t.Fatalf("context_changes.yaml field %q is not in metadata_field_inventory.yaml (%v)", ch.Field, jsonFields)
		}
	}

	for _, c := range openCodeOracleCases(t, document) {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			observed := runOpenCodeOracleCase(t, c)
			t.Logf("observed %v", observed)
			if violations := openCodeOracleViolations(c, observed); len(violations) != 0 {
				t.Fatalf("validator and parse skip disagree: %v (observed %+v)", violations, observed)
			}
			for sid, want := range c.Expect {
				got, ok := observed[sid]
				if !ok || got.oracleExpectation != want {
					t.Fatalf("session %s: observed %+v (present %t), want %+v", sid, got.oracleExpectation, ok, want)
				}
			}
		})
	}
}

// TestOpenCodeMetadataHalfMutationsFailTheOracle applies each metadata-half
// mutation and requires the oracle to catch it via either oracleViolations or
// a per-row Expect mismatch.
func TestOpenCodeMetadataHalfMutationsFailTheOracle(t *testing.T) {
	document := loadOpenCodeOracleDocument(t)
	for _, mutation := range loadOpenCodeMetadataHalfMutations(t) {
		t.Run(mutation.Name, func(t *testing.T) {
			ingest.SetOpenCodeMetadataHalfMutationForTest(t, openCodeHalfMutationHook(t, mutation.Name))
			var caught []string
			for _, c := range document.Cases {
				observed := runOpenCodeOracleCase(t, c)
				t.Logf("%s observed %v", c.Name, observed)
				if violations := openCodeOracleViolations(c, observed); len(violations) != 0 {
					caught = append(caught, c.Name+": "+strings.Join(violations, "; "))
					continue
				}
				for sid, want := range c.Expect {
					got, ok := observed[sid]
					if !ok || got.oracleExpectation != want {
						caught = append(caught, fmt.Sprintf("%s (%s: got %+v, want %+v)", c.Name, sid, got.oracleExpectation, want))
						break
					}
				}
			}
			if len(caught) == 0 {
				t.Fatal("the mutation passed the oracle on every case")
			}
			t.Logf("caught: %v", caught)
		})
	}
}
