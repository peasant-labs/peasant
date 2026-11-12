package ingest_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"maps"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testkit/fsdecorator"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/pair_install_identity.yaml
var pairInstallIdentityYAML []byte

//go:embed testdata/pair_install_identity.manifest.yaml
var pairInstallIdentityManifestYAML []byte

type pairInstallFiles struct {
	Transcript       string            `yaml:"transcript"`
	MetadataFor      string            `yaml:"metadataFor"`
	MetadataRevision int               `yaml:"metadataRevision"`
	Debug            map[string]string `yaml:"debug"`
	Marker           string            `yaml:"marker"`
}

type pairInstallExpect struct {
	TranscriptReplaced bool     `yaml:"transcriptReplaced"`
	MetadataReplaced   bool     `yaml:"metadataReplaced"`
	Writes             int      `yaml:"writes"`
	Renames            int      `yaml:"renames"`
	WALCommits         int      `yaml:"walCommits"`
	ProofKept          bool     `yaml:"proofKept"`
	Untouched          []string `yaml:"untouched"`
	Replaced           []string `yaml:"replaced"`
	Absent             []string `yaml:"absent"`
}

type pairInstallCase struct {
	Name      string            `yaml:"name"`
	Mode      string            `yaml:"mode"`
	Installed pairInstallFiles  `yaml:"installed"`
	Candidate pairInstallFiles  `yaml:"candidate"`
	Expect    pairInstallExpect `yaml:"expect"`
}

type pairInstallDocument struct {
	Transcripts map[string]string `yaml:"transcripts"`
	Cases       []pairInstallCase `yaml:"cases"`
}

func LoadPairInstallIdentityFixtures(t *testing.T) pairInstallDocument {
	t.Helper()
	var document pairInstallDocument
	if err := testutil.DecodeFixtureYAML(pairInstallIdentityYAML, &document); err != nil {
		t.Fatal(err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(pairInstallIdentityManifestYAML, "pair install identity")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(document.Cases))
	for i, c := range document.Cases {
		names[i] = c.Name
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "pair install identity"); err != nil {
		t.Fatal(err)
	}
	return document
}

// pairInstallProof is the settled index input proof every install case seeds.
const pairInstallProof = "abcdef1234abcdef1234abcdef1234abcdef1234abcdef1234abcdef12345679"

type pairInstallHarness struct {
	t          *testing.T
	document   pairInstallDocument
	session    ingest.DiscoveredSession
	sessionDir string
	metadata   string
}

func newPairInstallHarness(t *testing.T, document pairInstallDocument) *pairInstallHarness {
	t.Helper()
	session := makeDiscoveredSession(t, testSessionID, fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID), time.Now().Add(-2*time.Hour))
	return &pairInstallHarness{
		t: t, document: document, session: session,
		sessionDir: ingest.SessionDir(testOutputDir, testutil.TestHostSlug, testSessionID, ""),
		metadata:   ingest.SessionMetadataPath(testOutputDir, testutil.TestHostSlug, testSessionID, ""),
	}
}

func (h *pairInstallHarness) transcriptName() string { return testSessionID + "--transcript.jsonl" }

// rel resolves a fixture file label to its path relative to the session
// directory. "marker" is the private source-capture marker.
func (h *pairInstallHarness) rel(label string) string {
	if label == "marker" {
		return ingest.FileCaptureEvidenceName(h.session.SessionID)
	}
	return filepath.FromSlash(label)
}

// files builds the bytes of one fixture pair. The metadata records the
// checksum of metadataFor, so a pair whose metadataFor differs is torn.
func (h *pairInstallHarness) files(spec pairInstallFiles) ingest.PairInstallFiles {
	h.t.Helper()
	transcript, ok := h.document.Transcripts[spec.Transcript]
	if !ok {
		h.t.Fatalf("fixture names unknown transcript %q", spec.Transcript)
	}
	hashed := transcript
	if spec.MetadataFor != "" {
		if hashed, ok = h.document.Transcripts[spec.MetadataFor]; !ok {
			h.t.Fatalf("fixture names unknown transcript %q", spec.MetadataFor)
		}
	}
	meta := makeMinimalMeta(h.t, testSessionID)
	// Fixed, so two builds of one variant are byte-identical.
	ingested := int64(1708300900000)
	meta.Timestamp.Ingested = &ingested
	meta.ContentHash = schema.ComputeTranscriptHash([]byte(hashed))
	revision := spec.MetadataRevision
	meta.AdapterVersion = &revision
	encoded, err := json.Marshal(meta)
	if err != nil {
		h.t.Fatal(err)
	}
	files := ingest.PairInstallFiles{
		TranscriptName: h.transcriptName(), Transcript: []byte(transcript),
		MetadataName: filepath.Base(h.metadata), Metadata: encoded,
		SourceCapture: []byte(spec.Marker),
	}
	if len(spec.Debug) > 0 {
		files.Debug = make(map[string][]byte, len(spec.Debug))
		for name, body := range spec.Debug {
			files.Debug[name] = []byte(body)
		}
	}
	return files
}

func (h *pairInstallHarness) seed(fs ingest.FileSystem, files ingest.PairInstallFiles) {
	h.t.Helper()
	write := func(rel string, data []byte) {
		if err := fs.WriteFile(filepath.Join(h.sessionDir, rel), data, 0o600); err != nil {
			h.t.Fatal(err)
		}
	}
	write(files.TranscriptName, files.Transcript)
	for name, data := range files.Debug {
		write(filepath.Join(defaults.DirDebug.String(), name), data)
	}
	if len(files.SourceCapture) > 0 {
		write(h.rel("marker"), files.SourceCapture)
	}
	write(files.MetadataName, files.Metadata)
}

func (h *pairInstallHarness) pipeline(fs ingest.FileSystem, options ...ingest.PipelineOption) *ingest.Pipeline {
	h.t.Helper()
	adapters := map[ingest.Harness]ingest.AdapterFactory{ingest.HarnessClaudeCode: makeStubAdapter(nil, nil)}
	pipeline, err := newTestPipeline(fs, testutil.DefaultGitResolver(), adapters, makePipelineConfig(testOutputDir), options...)
	if err != nil {
		h.t.Fatal(err)
	}
	return pipeline
}

func pairInstallSQL(t *testing.T, db *store.Store, query string, args ...any) (text string, isNull bool) {
	t.Helper()
	conn, err := db.Pool().Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Pool().Put(conn)
	isNull = true
	if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{Args: args, ResultFunc: func(stmt *sqlite.Stmt) error {
		isNull = stmt.ColumnType(0) == sqlite.TypeNull
		text = stmt.ColumnText(0)
		return nil
	}}); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return text, isNull
}

func pairInstallWAL(t *testing.T, dbPath string) int {
	t.Helper()
	frames, err := storetest.CountWALCommitFrames(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	return frames
}

func TestPairInstallIdentity(t *testing.T) {
	t.Parallel()
	document := LoadPairInstallIdentityFixtures(t)
	for _, c := range document.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			switch c.Mode {
			case "install":
				runPairInstallCase(t, document, c)
			case "gated":
				runGatedPairInstallCase(t, document, c)
			case "harvest":
				runMetadataOnlyHarvestCase(t)
			default:
				t.Fatalf("unknown pair install mode %q", c.Mode)
			}
		})
	}
}

// runPairInstallCase installs the candidate over the seeded pair and counts
// the work: file writes and renames on the counting filesystem, and commit
// frames in the store's write-ahead log.
func runPairInstallCase(t *testing.T, document pairInstallDocument, c pairInstallCase) {
	h := newPairInstallHarness(t, document)
	fs := testutil.NewCountingFS(testutil.NewMemFS())
	installed, candidate := h.files(c.Installed), h.files(c.Candidate)
	h.seed(fs, installed)

	dbPath := storetest.CopyGoldenDB(t)
	db, err := store.Open(dbPath, store.WithSkipMigrations(), store.WithWALAutocheckpointDisabled())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// The row a completed harvest of the installed pair left: a pair identity
	// and a settled index input proof. A torn pair has no valid identity of
	// its own, so the row is the one its metadata describes.
	rowFiles := c.Installed
	if rowFiles.MetadataFor != "" {
		rowFiles.Transcript = rowFiles.MetadataFor
	}
	row := h.files(rowFiles)
	artifact, err := ingest.NewManagedArtifact(row.Metadata, row.Transcript)
	if err != nil {
		t.Fatal(err)
	}
	stored := artifact.Metadata
	if err := db.InsertSessions(t.Context(), []ingest.StoreEntry{{Metadata: &stored}}); err != nil {
		t.Fatal(err)
	}
	if results := db.MirrorArtifacts(t.Context(), []ingest.ArtifactMirrorRequest{{Artifact: artifact}}); len(results) != 1 || results[0].Err != nil {
		t.Fatalf("seed stored pair identity: %+v", results)
	}
	pairInstallSQL(t, db, "UPDATE sessions SET indexed_input_hash = ? WHERE session_id = ?", pairInstallProof, testSessionID)

	pipeline := h.pipeline(fs, ingest.WithStore(db), ingest.WithMetricsStore(db))
	fs.ResetCounts()
	walBefore := pairInstallWAL(t, dbPath)
	transcriptReplaced, metadataReplaced, err := pipeline.InstallManagedPair(t.Context(), h.sessionDir, h.session, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if transcriptReplaced != c.Expect.TranscriptReplaced || metadataReplaced != c.Expect.MetadataReplaced {
		t.Errorf("replaced (transcript, metadata) = (%t, %t), want (%t, %t)", transcriptReplaced, metadataReplaced, c.Expect.TranscriptReplaced, c.Expect.MetadataReplaced)
	}
	if got := fs.Total(testutil.FSOpWriteFile); got != c.Expect.Writes {
		t.Errorf("WriteFile total = %d, want %d", got, c.Expect.Writes)
	}
	if got := fs.Total(testutil.FSOpRename); got != c.Expect.Renames {
		t.Errorf("Rename total = %d, want %d", got, c.Expect.Renames)
	}
	if c.Expect.Writes == 0 {
		if got := fs.Total(testutil.FSOpMkdirAll); got != 0 {
			t.Errorf("an install that writes nothing made %d directories, want no temporary directory", got)
		}
	}
	if got := pairInstallWAL(t, dbPath) - walBefore; got != c.Expect.WALCommits {
		t.Errorf("store commits during the install = %d, want %d", got, c.Expect.WALCommits)
	}
	renamed := func(rel string) int { return fs.Count(testutil.FSOpRename, filepath.Join(h.sessionDir, rel)) }
	for rel, replaced := range map[string]bool{candidate.TranscriptName: c.Expect.TranscriptReplaced, candidate.MetadataName: c.Expect.MetadataReplaced} {
		want := 0
		if replaced {
			want = 1
		}
		if got := renamed(rel); got != want {
			t.Errorf("renames onto %s = %d, want %d", rel, got, want)
		}
	}
	for _, label := range c.Expect.Untouched {
		if got := renamed(h.rel(label)); got != 0 {
			t.Errorf("unchanged file %s was renamed over %d times", label, got)
		}
		if _, err := fs.ReadFile(filepath.Join(h.sessionDir, h.rel(label))); err != nil {
			t.Errorf("unchanged file %s is gone: %v", label, err)
		}
	}
	for _, label := range c.Expect.Replaced {
		if got := renamed(h.rel(label)); got != 1 {
			t.Errorf("renames onto changed file %s = %d, want 1", label, got)
		}
	}
	for _, label := range c.Expect.Absent {
		if _, err := fs.ReadFile(filepath.Join(h.sessionDir, h.rel(label))); err == nil {
			t.Errorf("obsolete owned file %s survived the install", label)
		}
	}
	proof, cleared := pairInstallSQL(t, db, "SELECT indexed_input_hash FROM sessions WHERE session_id = ?", testSessionID)
	if c.Expect.ProofKept && (cleared || proof != pairInstallProof) {
		t.Errorf("index input proof = %q (null %t), want it kept", proof, cleared)
	}
	if !c.Expect.ProofKept && !cleared {
		t.Errorf("index input proof = %q, want it cleared before the transcript was replaced", proof)
	}
	// Whatever was kept or replaced, the installed pair is the candidate.
	current, err := ingest.ReadManagedPair(fs, testOutputDir, h.metadata, h.session.SessionID)
	if err != nil {
		t.Fatalf("read installed pair: %v", err)
	}
	if string(current.Transcript) != string(candidate.Transcript) || string(current.MetadataJSON) != string(candidate.Metadata) {
		t.Error("the installed pair is not the candidate pair")
	}
}

// runGatedPairInstallCase holds the metadata rename of a metadata-only install
// and reads the pair while it is held and after it is released. The transcript
// is never touched, so the reader sees the whole old pair, then the whole new
// one.
func runGatedPairInstallCase(t *testing.T, document pairInstallDocument, c pairInstallCase) {
	h := newPairInstallHarness(t, document)
	mem := testutil.NewMemFS()
	gated := testutil.NewGatedFS(mem)
	installed, candidate := h.files(c.Installed), h.files(c.Candidate)
	h.seed(mem, installed)
	pipeline := h.pipeline(gated)
	gated.Arm(fsdecorator.Gate{Op: fsdecorator.OpRename, Path: h.metadata})
	type outcome struct {
		transcript, metadata bool
		err                  error
	}
	done := make(chan outcome, 1)
	go func() {
		var result outcome
		result.transcript, result.metadata, result.err = pipeline.InstallManagedPair(context.Background(), h.sessionDir, h.session, candidate)
		done <- result
	}()
	select {
	case <-gated.Reached():
	case result := <-done:
		t.Fatalf("the install finished without renaming its metadata: %+v", result)
	case <-time.After(5 * time.Second):
		t.Fatal("the install never reached its metadata rename")
	}
	held, err := ingest.ReadManagedPair(mem, testOutputDir, h.metadata, h.session.SessionID)
	if err != nil {
		t.Fatalf("pair read while the metadata rename is held: %v", err)
	}
	if string(held.MetadataJSON) != string(installed.Metadata) || string(held.Transcript) != string(installed.Transcript) {
		t.Error("the pair read during the held rename is not the whole old pair")
	}
	gated.Release()
	result := <-done
	if result.err != nil {
		t.Fatal(result.err)
	}
	if result.transcript != c.Expect.TranscriptReplaced || result.metadata != c.Expect.MetadataReplaced {
		t.Errorf("replaced (transcript, metadata) = (%t, %t), want (%t, %t)", result.transcript, result.metadata, c.Expect.TranscriptReplaced, c.Expect.MetadataReplaced)
	}
	after, err := ingest.ReadManagedPair(mem, testOutputDir, h.metadata, h.session.SessionID)
	if err != nil {
		t.Fatalf("pair read after the install: %v", err)
	}
	if string(after.MetadataJSON) != string(candidate.Metadata) || string(after.Transcript) != string(candidate.Transcript) {
		t.Error("the pair read after the install is not the whole new pair")
	}
}

// runMetadataOnlyHarvestCase harvests one session, then harvests it again
// under a newer adapter revision. The source is unchanged, so the second run
// produces the same transcript bytes beside new metadata. It must keep the
// transcript file and the index input proof, and write no entry row.
func runMetadataOnlyHarvestCase(t *testing.T) {
	fs := testutil.NewCountingFS(testutil.NewMemFS())
	git := testutil.DefaultGitResolver()
	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	modTime := time.Now().Add(-2 * time.Hour)
	setupSourceFile(t, fs.MemFS, sourcePath)
	fs.ModTimes[sourcePath] = modTime
	session := makeDiscoveredSession(t, testSessionID, sourcePath, modTime)
	meta := makeMinimalMeta(t, testSessionID)
	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter([]ingest.DiscoveredSession{session}, map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta}),
	}
	dbPath := storetest.CopyGoldenDB(t)
	db, err := store.Open(dbPath, store.WithSkipMigrations(), store.WithWALAutocheckpointDisabled())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	run := func(versions map[ingest.Harness]ingest.HarvesterVersions) *ingest.PipelineResult {
		t.Helper()
		pipeline, err := newTestPipeline(fs, git, adapters, makePipelineConfig(testOutputDir),
			ingest.WithStore(db), ingest.WithMetricsStore(db), ingest.WithIndexLogger(db),
			ingest.WithHarvesterVersions(versions),
			ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})))
		if err != nil {
			t.Fatal(err)
		}
		result, err := pipeline.Run(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	versions := maps.Clone(ingest.HarvesterVersionRegistry)
	run(versions)
	sessionDir := ingest.SessionDir(testOutputDir, string(meta.HostSlug), testSessionID, "")
	transcriptPath := filepath.Join(sessionDir, testSessionID+"--transcript.jsonl")
	metadataPath := ingest.SessionMetadataPath(testOutputDir, string(meta.HostSlug), testSessionID, "")
	before, err := db.ReadIndexState(t.Context(), session.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if before == nil || before.IndexedInputHash == nil || before.ArtifactHash == nil {
		t.Fatalf("the first harvest did not settle the session: %+v", before)
	}

	// Record every entry row change the second run makes.
	pairInstallSQL(t, db, "CREATE TABLE pair_install_entry_changes(source TEXT NOT NULL)")
	for _, table := range []string{"session_entries", "session_entry_bodies"} {
		for _, event := range []string{"INSERT", "UPDATE", "DELETE"} {
			pairInstallSQL(t, db, fmt.Sprintf("CREATE TRIGGER pair_install_%s_%s AFTER %s ON %s BEGIN INSERT INTO pair_install_entry_changes(source) VALUES('%s'); END",
				table, strings.ToLower(event), event, table, table))
		}
	}
	claude := versions[ingest.HarnessClaudeCode]
	claude.AdapterVersion++
	versions[ingest.HarnessClaudeCode] = claude
	fs.ResetCounts()
	result := run(versions)
	if result.Summary.Updated != 1 {
		t.Fatalf("the adapter bump did not refresh the session: %+v", result.Summary)
	}
	for _, op := range []testutil.FSOp{testutil.FSOpWriteFile, testutil.FSOpRename, testutil.FSOpRemove, testutil.FSOpRemoveAll} {
		if got := fs.Count(op, transcriptPath); got != 0 {
			t.Errorf("%s on the unchanged transcript = %d, want 0 (the installed file is kept)", op, got)
		}
	}
	if got := fs.CountSuffixUnder(testutil.FSOpWriteFile, testOutputDir, "--transcript.jsonl"); got != 0 {
		t.Errorf("transcript files written anywhere under the output = %d, want 0", got)
	}
	if got := fs.Count(testutil.FSOpRename, metadataPath); got != 1 {
		t.Errorf("metadata renames = %d, want 1", got)
	}
	after, err := db.ReadIndexState(t.Context(), session.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if after == nil || after.IndexedInputHash == nil || *after.IndexedInputHash != *before.IndexedInputHash {
		t.Errorf("index input proof after the metadata-only refresh = %v, want %q kept", after.IndexedInputHash, *before.IndexedInputHash)
	}
	if after.ArtifactHash == nil || *after.ArtifactHash == *before.ArtifactHash {
		t.Error("the mirror did not record the new pair identity")
	}
	if after.PublicationCaptureRevision > 0 && !after.PublicationBound {
		t.Errorf("the stored index was left unbound from metadata capture revision %d", after.PublicationCaptureRevision)
	}
	if changes, _ := pairInstallSQL(t, db, "SELECT COUNT(*) FROM pair_install_entry_changes"); changes != "0" {
		t.Errorf("entry rows changed by the metadata-only refresh = %s, want 0", changes)
	}
	// The run after it finds nothing to do.
	fs.ResetCounts()
	settled := run(versions)
	if settled.Summary.Updated != 0 || settled.Summary.New != 0 {
		t.Errorf("the session did not settle after the refresh: %+v", settled.Summary)
	}
	if got := fs.Total(testutil.FSOpWriteFile) + fs.Total(testutil.FSOpRename); got != 0 {
		t.Errorf("file writes and renames in the settled run = %d, want 0", got)
	}
}
