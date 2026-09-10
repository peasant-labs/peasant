package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/harvest_metadata_diagnostics.yaml
var harvestMetadataDiagnosticsYAML []byte

// An incompatible metadata artifact is a refusal for ONE session: the harvest
// stays non-fatal, the last-good index and every artifact are left alone, and the
// user is told what to upgrade. Two surfaces have to carry that, and they are
// tested apart because the command deliberately runs them apart:
//
//   - the --json surface, where the interactive renderer is OFF and the
//     diagnostics are part of the document;
//   - the interactive surface, where the renderer owns the terminal and the
//     warning has to survive the render.
//
// The earlier single test asked for both at once: it passed --json and then
// required the interactive run's log suppression, a state the command cannot be
// in: --json turns the renderer off, and the command suppresses structured logs
// only while the renderer owns the terminal.
//
// That suppression is the command's own, at the renderer.IsTTY() branch in
// cmd_harvest.go, and it is what keeps a slog line out of the rendered frames.
// Each test asserts it from its own side: the terminal test requires the default
// logger to stay silent for the whole interactive run, and the document test
// requires the same logs to flow when the renderer is off. Neither test discards
// the logger, because a discarded logger cannot tell whether the command
// suppressed anything.

// harvestDiagnosticsFixtures is the corpus both surfaces drive.
type harvestDiagnosticsFixtures struct {
	RequiredNames    []string                 `yaml:"requiredNames"`
	ExpectedRootKeys []string                 `yaml:"expectedRootKeys"`
	SessionID        ingest.SessionID         `yaml:"sessionID"`
	Transcript       string                   `yaml:"transcript"`
	Cases            []harvestDiagnosticsCase `yaml:"cases"`
}

type harvestDiagnosticsCase struct {
	Name                string `yaml:"name"`
	SchemaVersion       int    `yaml:"schemaVersion"`
	StoredSchemaVersion int    `yaml:"storedSchemaVersion"`
	Force               bool   `yaml:"force"`
	Reindex             bool   `yaml:"reindex"`
	WantWarning         bool   `yaml:"wantWarning"`
	// WantIndexLog marks the cases that actually index something: the document
	// carries an index log only for work that happened, so a refusal must NOT
	// grow one.
	WantIndexLog bool `yaml:"wantIndexLog"`
	// WantStructuredLog says whether this run emits a structured WARN at all.
	// The document surface suppresses nothing, so this is a statement about the
	// run, and it is declared per case rather than inferred from what happens to
	// appear: inferring it would pass however the command behaved.
	WantStructuredLog bool `yaml:"wantStructuredLog"`
}

func loadHarvestDiagnosticsFixtures(t *testing.T) harvestDiagnosticsFixtures {
	t.Helper()
	var fixtures harvestDiagnosticsFixtures
	decoder := yaml.NewDecoder(bytes.NewReader(harvestMetadataDiagnosticsYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatal(err)
	}
	return fixtures
}

// harvestDiagnosticsWorld is one arranged session: a stored index, its managed
// artifact at the fixture's schema version, a retained native source, and a
// snapshot of every file so a refusal can be proved to have changed nothing.
type harvestDiagnosticsWorld struct {
	db         *store.Store
	dir        string
	output     string
	source     string
	config     string
	files      map[string][]byte
	before     harvestIndexSnapshot
	session    ingest.SessionID
	transcript string
}

func arrangeHarvestDiagnostics(t *testing.T, fixtures harvestDiagnosticsFixtures, fixture harvestDiagnosticsCase) harvestDiagnosticsWorld {
	t.Helper()
	dir := t.TempDir()
	output := filepath.Join(dir, "managed")
	dbPath := defaults.ResolveDBFilePathWith(dir).String()
	if err := os.MkdirAll(filepath.Dir(dbPath), 0700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	files := make(map[string][]byte)
	seedHarvestIndexSession(t, db, output, harvestIndexSessionFixture{Name: fixture.Name, ID: fixtures.SessionID, Harness: ingest.HarnessClaudeCode, Age: "24h", Transcript: fixtures.Transcript}, files)
	metaPath := filepath.Join(output, testutil.TestHostSlug, string(fixtures.SessionID), string(fixtures.SessionID)+defaults.MetadataSuffix)
	var meta ingest.UnifiedMetadata
	if err := json.Unmarshal(files[metaPath], &meta); err != nil {
		t.Fatal(err)
	}
	meta.SchemaVersion = fixture.SchemaVersion
	meta.MetadataHash = schema.ComputeMetadataHash(&meta)
	files[metaPath], err = json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metaPath, files[metaPath], 0600); err != nil {
		t.Fatal(err)
	}
	if fixture.SchemaVersion <= ingest.CurrentSchemaVersion {
		// Settle the compatible artifact change before recording the
		// no-op baseline or injecting a future stored-schema refusal.
		reconcileHarvestIndexMetadata(t, db, output, fixtures.SessionID, metaPath, files)
	}
	if fixture.StoredSchemaVersion > 0 {
		conn, err := db.Pool().Take(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		err = sqlitex.ExecuteTransient(conn, "UPDATE sessions SET schema_version = ? WHERE session_id = ?", &sqlitex.ExecOptions{Args: []any{fixture.StoredSchemaVersion, string(fixtures.SessionID)}})
		db.Pool().Put(conn)
		if err != nil {
			t.Fatal(err)
		}
	}
	before := readHarvestIndexSnapshot(t, db, fixtures.SessionID)
	source := filepath.Join(dir, "native")
	nativePath := filepath.Join(source, "-synthetic-project", string(fixtures.SessionID)+".jsonl")
	if err := os.MkdirAll(filepath.Dir(nativePath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nativePath, []byte(fixtures.Transcript), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.UnixMilli(1700000000000)
	if err := os.Chtimes(nativePath, old, old); err != nil {
		t.Fatal(err)
	}
	files[nativePath] = []byte(fixtures.Transcript)
	return harvestDiagnosticsWorld{db: db, dir: dir, output: output, source: source, config: writeTestConfigFile(t, dir), files: files, before: before, session: fixtures.SessionID, transcript: fixtures.Transcript}
}

// args builds the mounted harvest invocation for one case. json selects the
// document surface, which is also what turns the interactive renderer off.
func (w harvestDiagnosticsWorld) args(fixture harvestDiagnosticsCase, asJSON bool) []string {
	args := []string{"--config", w.config, "--data-dir", w.dir, "--config-dir", w.dir, "--state-dir", w.dir, "harvest"}
	if fixture.Reindex {
		args = append(args, "index")
	}
	args = append(args, "--source-harness=claude-code", "--output="+w.output)
	if asJSON {
		args = append(args, "--json")
	}
	if !fixture.Reindex {
		args = append(args, "--source-path="+w.source)
	}
	if fixture.Force {
		args = append(args, "--force")
	}
	return args
}

// diagnosticsSurface names the two output surfaces a harvest can end on. They
// differ in what "said nothing" can mean: a document surface writes no bytes at
// all, while an interactive one has already written the frames of its render.
type diagnosticsSurface int

const (
	diagnosticsSurfaceDocument diagnosticsSurface = iota
	diagnosticsSurfaceInteractive
)

// assertWarning holds the refusal's user-facing half: the session, the version it
// needs, and the upgrade that fixes it, or no such claim for compatible input.
func (w harvestDiagnosticsWorld) assertWarning(t *testing.T, fixture harvestDiagnosticsCase, shown string, surface diagnosticsSurface) {
	t.Helper()
	if fixture.WantWarning {
		for _, want := range []string{string(w.session), "999", "upgrade Peasant"} {
			if !strings.Contains(shown, want) {
				t.Fatalf("the post-render warning must name %q: %q", want, shown)
			}
		}
		return
	}
	if surface == diagnosticsSurfaceDocument {
		if strings.TrimSpace(shown) != "" {
			t.Fatalf("compatible input produced a false warning: %q", shown)
		}
		return
	}
	// The render's own control sequences are not a warning. What must be absent
	// is the refusal itself, in any of the words it is made of.
	//
	// The "warning:" prefix is live, not decoration: the harvest prints every
	// diagnostic as "warning: <location>: <message>" on the command's error
	// stream, which a refusal case on this same surface is observed to produce.
	for _, forbidden := range []string{"upgrade Peasant", "warning:", "999"} {
		if strings.Contains(shown, forbidden) {
			t.Fatalf("compatible input produced a false warning naming %q: %q", forbidden, shown)
		}
	}
}

// assertOutcome holds what the run owed this session afterwards.
//
// For a refusal that is the whole of the product's promise, in its own words:
// this session's artifact and index were not changed. Every seeded file is
// compared byte for byte, the retained native source included.
//
// A COMPATIBLE artifact is not a no-op and is not asserted as one. This corpus's
// native source sits in a different project from the seeded host slug, so a
// compatible run re-ingests and republishes the artifact under the session's
// current identity: a real, correct move. What it still owes is the transcript,
// which no re-publication may alter, and a retained source it must never touch.
func (w harvestDiagnosticsWorld) assertOutcome(t *testing.T, fixture harvestDiagnosticsCase) {
	t.Helper()
	if fixture.WantWarning {
		after := readHarvestIndexSnapshot(t, w.db, w.session)
		if after.IndexerVersion != w.before.IndexerVersion || after.IndexedAt != w.before.IndexedAt || !reflect.DeepEqual(after.Entries, w.before.Entries) {
			t.Fatal("the refusal changed the last-good index")
		}
		for path, beforeBytes := range w.files {
			afterBytes, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(afterBytes, beforeBytes) {
				t.Fatalf("artifact or source %s changed: %v", path, err)
			}
		}
		return
	}
	sourceBytes, err := os.ReadFile(filepath.Join(w.source, "-synthetic-project", string(w.session)+".jsonl"))
	if err != nil || !bytes.Equal(sourceBytes, []byte(w.transcript)) {
		t.Fatalf("the run changed the retained native source: %v", err)
	}
	transcript, err := w.publishedTranscript()
	if err != nil {
		t.Fatalf("the compatible run left no published transcript for this session: %v", err)
	}
	if !bytes.Equal(transcript, []byte(w.transcript)) {
		t.Fatalf("re-publishing the artifact changed the transcript text:\n%s", transcript)
	}
}

// publishedTranscript finds this session's published transcript wherever the run
// placed it, because the artifact's directory is its project identity and a
// compatible re-ingest may legitimately change that.
func (w harvestDiagnosticsWorld) publishedTranscript() ([]byte, error) {
	var found []byte
	err := filepath.WalkDir(w.output, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || !strings.HasSuffix(path, string(w.session)+"--transcript.jsonl") {
			return walkErr
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		found = data
		return nil
	})
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, fmt.Errorf("no transcript named %s--transcript.jsonl exists under %s", w.session, w.output)
	}
	return found, nil
}

// TestHarvestMetadataDiagnosticsJSON drives the document surface: the refusal is
// non-fatal, the JSON keys and the non-error summary are unchanged, the warning
// is actionable, and nothing stored or retained moved.
func TestHarvestMetadataDiagnosticsJSON(t *testing.T) {
	fixtures := loadHarvestDiagnosticsFixtures(t)
	names := make(map[string]bool)
	for _, fixture := range fixtures.Cases {
		if fixture.Name == "" || names[fixture.Name] {
			t.Fatalf("invalid fixture name %q", fixture.Name)
		}
		names[fixture.Name] = true
		t.Run(fixture.Name, func(t *testing.T) {
			world := arrangeHarvestDiagnostics(t, fixtures, fixture)
			oldLogger := slog.Default()
			var logged bytes.Buffer
			slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
			defer slog.SetDefault(oldLogger)
			root := buildRootCommand()
			var stdout, stderr bytes.Buffer
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			root.SetArgs(world.args(fixture, true))
			if err := root.Execute(); err != nil {
				t.Fatalf("a metadata refusal must stay non-fatal: %v; stderr=%s", err, &stderr)
			}
			// The document surface turns the renderer off, so nothing owns the
			// terminal and structured logs are NOT suppressed. A refusal's own log
			// line is the evidence: suppressing it here would hide a refusal from
			// every non-interactive consumer, which is what this surface is for.
			if got := strings.Contains(logged.String(), "level=WARN"); got != fixture.WantStructuredLog {
				t.Fatalf("structured WARN present=%v, want %v on the document surface; this surface suppresses nothing, so the value is whatever the run emits: %q", got, fixture.WantStructuredLog, logged.String())
			}
			world.assertWarning(t, fixture, stderr.String(), diagnosticsSurfaceDocument)
			var result map[string]json.RawMessage
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatalf("JSON stdout changed: %v; %s", err, &stdout)
			}
			wantKeys := slices.Clone(fixtures.ExpectedRootKeys)
			if fixture.WantIndexLog {
				wantKeys = append(wantKeys, "indexLog")
			}
			slices.Sort(wantKeys)
			if keys := slices.Sorted(maps.Keys(result)); !slices.Equal(keys, wantKeys) {
				t.Fatalf("JSON root keys=%v want=%v", keys, wantKeys)
			}
			var summary ingest.PipelineSummary
			if err := json.Unmarshal(result["summary"], &summary); err != nil || summary.Errors != 0 {
				t.Fatalf("the refusal changed the non-fatal summary: %+v %v", summary, err)
			}
			world.assertOutcome(t, fixture)
		})
	}
	if err := testutil.RequireFixtureNames("harvest metadata diagnostics", "case", fixtures.RequiredNames, names); err != nil {
		t.Fatal(err)
	}
}

// TestHarvestMetadataDiagnosticsTTY drives the interactive surface through the
// mounted progress terminal: the renderer draws, and the refusal's warning still
// reaches the user after the render rather than being erased by it.
//
// The renderer decides whether to draw from the command's OWN error stream, so
// that stream is the pseudo-terminal here; a buffer would have disabled it and
// left this testing the pipe path twice. Everything the user would see is read
// back off the terminal, which is why the warning and the frames are asserted in
// the same stream.
//
// It changes process-global stderr and the default logger, so it stays
// non-parallel. It never asserts global log suppression: the command does not
// suppress logs, and requiring it to would be asking for a state it is not in.
func TestHarvestMetadataDiagnosticsTTY(t *testing.T) {
	fixtures := loadHarvestDiagnosticsFixtures(t)
	for _, fixture := range fixtures.Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			world := arrangeHarvestDiagnostics(t, fixtures, fixture)
			master, terminal := openTestTerminal(t)
			var mu sync.Mutex
			var shown bytes.Buffer
			drained := make(chan struct{})
			go func() {
				defer close(drained)
				buf := make([]byte, 4096)
				for {
					n, err := master.Read(buf)
					if n > 0 {
						mu.Lock()
						shown.Write(buf[:n])
						mu.Unlock()
					}
					if err != nil {
						return
					}
				}
			}()
			oldStderr, oldLogger := os.Stderr, slog.Default()
			os.Stderr = terminal
			// A buffer, never io.Discard. Every structured log this scenario emits
			// is emitted while the renderer owns the terminal, so the command's own
			// suppression is exactly what decides whether this buffer stays empty.
			// Discarding here would answer that question for the command and leave
			// its suppression untested.
			var logged bytes.Buffer
			slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
			root := buildRootCommand()
			var stdout bytes.Buffer
			root.SetOut(&stdout)
			root.SetErr(terminal)
			root.SetArgs(world.args(fixture, false))
			err := root.Execute()
			os.Stderr, _ = oldStderr, 0
			slog.SetDefault(oldLogger)
			_ = terminal.Close()
			<-drained
			mu.Lock()
			visible := shown.String()
			mu.Unlock()
			if err != nil {
				t.Fatalf("a metadata refusal must stay non-fatal: %v; terminal=%q", err, visible)
			}
			// Frame control sequences: proof the interactive renderer actually ran,
			// so "the warning survived the render" is a claim about a render.
			if !strings.Contains(visible, "\x1b[") {
				t.Fatalf("the interactive run drew no terminal control sequences, so this case cannot show a warning surviving a render: %q", visible)
			}
			// The command suppressed structured logs for the whole render. Deleting
			// its renderer.IsTTY() branch in cmd_harvest.go sends this scenario's
			// warnings here instead, and this assertion is what reports it.
			if logged.Len() != 0 {
				t.Fatalf("the interactive run let structured logs through while the renderer owned the terminal: %q", logged.String())
			}
			// And they did not reach the frames by another route either.
			for _, leaked := range []string{"level=WARN", "level=ERROR", "level=INFO", "msg="} {
				if strings.Contains(visible, leaked) {
					t.Fatalf("a structured log line reached the rendered frames (%q): %q", leaked, visible)
				}
			}
			world.assertWarning(t, fixture, visible, diagnosticsSurfaceInteractive)
			world.assertOutcome(t, fixture)
		})
	}
}
