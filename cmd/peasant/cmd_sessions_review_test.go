package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/sessions_review_cases.yaml
var sessionsReviewCasesYAML []byte

//go:embed testdata/sessions_review_cases.manifest.yaml
var sessionsReviewCasesManifestYAML []byte

type reviewBodyEntry struct {
	Role       string `yaml:"role"`
	Text       string `yaml:"text"`
	Output     string `yaml:"output"`
	PadOutput  int    `yaml:"padOutput"`
	ToolCallID string `yaml:"toolCallId"`
	ToolInput  string `yaml:"toolInput"`
}

type reviewSeed struct {
	ID      string `yaml:"id"`
	Body    string `yaml:"body"`
	Capture string `yaml:"capture"`
	Parent  string `yaml:"parent"`
}

type reviewRun struct {
	Args            []string            `yaml:"args"`
	Stdin           string              `yaml:"stdin"`
	GitEmail        string              `yaml:"gitEmail"`
	WantExit        int                 `yaml:"wantExit"`
	WantStatus      map[string]string   `yaml:"wantStatus"`
	WantKinds       map[string][]string `yaml:"wantKinds"`
	WantReason      map[string]string   `yaml:"wantReason"`
	WantErr         []string            `yaml:"wantErr"`
	WantWarnings    []string            `yaml:"wantWarnings"`
	WantStderrEmpty bool                `yaml:"wantStderrEmpty"`
	WantUnchanged   bool                `yaml:"wantUnchanged"`
	GoldenFile      string              `yaml:"goldenFile"`
}

type reviewCase struct {
	Name  string       `yaml:"name"`
	Seeds []reviewSeed `yaml:"seeds"`
	Runs  []reviewRun  `yaml:"runs"`
}

type reviewFixture struct {
	Canaries []string                     `yaml:"canaries"`
	Bodies   map[string][]reviewBodyEntry `yaml:"bodies"`
	Cases    []reviewCase                 `yaml:"cases"`
}

func loadReviewFixture(t *testing.T) reviewFixture {
	t.Helper()
	var fixture reviewFixture
	decoder := yaml.NewDecoder(bytes.NewReader(sessionsReviewCasesYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode sessions_review_cases.yaml: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(sessionsReviewCasesManifestYAML, "sessions review")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(fixture.Cases))
	for _, c := range fixture.Cases {
		names = append(names, c.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "sessions review"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func reviewEntries(id string, body []reviewBodyEntry) []schema.SessionEntry {
	entries := make([]schema.SessionEntry, len(body))
	for i, e := range body {
		entry := schema.SessionEntry{
			SessionID: schema.SessionID(id), EntryIndex: i, Harness: schema.HarnessClaudeCode,
			EntryType: schema.EntryTypeText, Role: schema.Role(e.Role),
		}
		if e.Text != "" {
			text := e.Text
			entry.ContentPreview = &text
		}
		if e.Output != "" {
			output := strings.Repeat("filler line\n", e.PadOutput/12) + e.Output
			entry.ToolOutput = &output
		}
		if e.ToolCallID != "" {
			callID := e.ToolCallID
			entry.ToolCallID = &callID
		}
		if e.ToolInput != "" {
			input := e.ToolInput
			entry.ToolInput = &input
		}
		entries[i] = entry
	}
	return entries
}

// reviewWorld is one computer's store and config.
type reviewWorld struct {
	dir, cfgPath string
	ownedRoot    string
}

func newReviewWorld(t *testing.T, fixture reviewFixture, seeds []reviewSeed) reviewWorld {
	t.Helper()
	dir := t.TempDir()
	w := reviewWorld{dir: dir, ownedRoot: filepath.Join(dir, "peasant-sync")}
	w.cfgPath = writeCfg(t, dir, "review.yaml", "version: 1\noutput:\n  basePath: "+w.ownedRoot+"\n")
	if len(seeds) == 0 {
		return w
	}
	if err := os.MkdirAll(w.ownedRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	dbPath := string(defaults.ResolveDBFilePathWith(dir))
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := openPreparedStore(t, dbPath, mustGenerationStoreOptions(t, w.ownedRoot)...)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for i, seed := range seeds {
		seedReviewSession(t, db, seed, fixture.Bodies[seed.Body], int64(i))
	}
	return w
}

func seedReviewSession(t *testing.T, db *store.Store, seed reviewSeed, body []reviewBodyEntry, order int64) {
	t.Helper()
	ctx := t.Context()
	entries := reviewEntries(seed.ID, body)
	entry := makeCmdStoreEntry(t, seed.ID, "github.com-acme-tools", "", "main", 1700000000000+order*60000, "/synthetic/repo")
	if seed.Parent != "" {
		parent := schema.SessionID(seed.Parent)
		entry.Metadata.ParentUUID = &parent
	}
	switch seed.Capture {
	case "", "full":
		testutil.SeedReadyPublication(t, db, entry.Metadata, entries)
	case "preview":
		if err := db.InsertSessions(ctx, []ingest.StoreEntry{entry}); err != nil {
			t.Fatal(err)
		}
		if err := db.IndexSessionEntries(ctx, entry.Metadata.SessionID, entries); err != nil {
			t.Fatal(err)
		}
	case "native":
		seedNativeReviewSession(t, db, entry.Metadata.SessionID, *entries[0].ContentPreview)
	default:
		t.Fatalf("unknown capture %q", seed.Capture)
	}
}

// seedNativeReviewSession activates one managed generation, the shape a native
// harness stores: its entries live in the generation tables and nothing is in
// the session_entries mirror.
func seedNativeReviewSession(t *testing.T, db *store.Store, id schema.SessionID, text string) {
	t.Helper()
	entry := makeCmdStoreEntry(t, string(id), "github.com-acme-tools", "", "main", 1700000000000, "/synthetic/repo")
	if err := db.InsertSessions(t.Context(), []ingest.StoreEntry{entry}); err != nil {
		t.Fatal(err)
	}
	candidate, blobs := reclaimCmdGeneration(t, id, "gen-review")
	entries := candidate.Generation.Main.Entries
	entries[0].ContentPreview = &text
	for ref := range blobs {
		blobs[ref] = []byte(text)
	}
	activation := store.GenerationActivation{
		Generation: fillReclaimCmdCandidate(candidate, blobs), Blobs: blobs, IndexerVersion: 1, IndexedAtMs: 1,
		ContentCapture: ingest.SessionContentCaptureWrite{Status: ingest.ContentCaptureComplete, SourceAuthority: ingest.ContentSourceNewIngest, CaptureFormat: ingest.ContentCaptureFormatFull},
	}
	if _, err := db.ActivateGeneration(t.Context(), activation); err != nil {
		t.Fatalf("activate native generation: %v", err)
	}
}

func (w reviewWorld) review(t *testing.T, run reviewRun) (runOutcome, defaults.ExitCode) {
	t.Helper()
	root := newTestRoot()
	sessions := &cobra.Command{Use: "sessions"}
	sessions.AddCommand(buildSessionsReviewCommandWith(sessionsReviewDeps{
		gitEmail: func(context.Context) string { return run.GitEmail },
	}))
	root.AddCommand(sessions)
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetIn(strings.NewReader(run.Stdin))
	root.SetArgs(append([]string{"--data-dir", w.dir, "--config-dir", w.dir, "--state-dir", w.dir, "--config", w.cfgPath, "sessions", "review"}, run.Args...))
	err := root.Execute()
	exit := defaults.ExitOK
	if err != nil {
		exit = exitCodeFor(err)
	}
	return runOutcome{stdout: stdout.String(), stderr: stderr.String(), err: err}, exit
}

// directoryDigest hashes every file under dir but the SQLite coordination files
// a read-only open may create: -shm, and a -wal that stays empty.
func directoryDigest(t *testing.T, dir string) map[string]string {
	t.Helper()
	digest := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasSuffix(path, "-shm") {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.HasSuffix(path, "-wal") && len(raw) == 0 {
			return nil
		}
		digest[filepath.Base(path)] = fmt.Sprintf("%x", sha256.Sum256(raw))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func TestSessionsReview(t *testing.T) {
	t.Parallel()
	fixture := loadReviewFixture(t)
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			w := newReviewWorld(t, fixture, tc.Seeds)
			for _, run := range tc.Runs {
				before := directoryDigest(t, w.dir)
				out, exit := w.review(t, run)
				context := fmt.Sprintf("args %v\nerr: %v\nstdout:\n%s\nstderr:\n%s", run.Args, out.err, out.stdout, out.stderr)
				if exit.Int() != run.WantExit {
					t.Fatalf("exit = %d, want %d\n%s", exit, run.WantExit, context)
				}
				for _, want := range run.WantErr {
					if out.err == nil || !strings.Contains(out.err.Error(), want) {
						t.Errorf("error misses %q\n%s", want, context)
					}
				}
				if run.WantStderrEmpty && out.stderr != "" {
					t.Errorf("stderr must be empty\n%s", context)
				}
				if run.WantUnchanged {
					after := directoryDigest(t, w.dir)
					if fmt.Sprint(before) != fmt.Sprint(after) {
						t.Errorf("the run changed files:\nbefore %v\nafter  %v", before, after)
					}
				}
				for _, canary := range fixture.Canaries {
					if strings.Contains(out.stdout+out.stderr, canary) {
						t.Errorf("output holds the matched value %q\n%s", canary, context)
					}
				}
				if run.GoldenFile != "" {
					assertReviewGolden(t, run.GoldenFile, out.stdout)
				}
				if slicesContainJSON(run.Args) && out.stdout != "" {
					assertReviewDocument(t, run, out.stdout, context)
				}
			}
		})
	}
}

func slicesContainJSON(args []string) bool {
	for _, arg := range args {
		if arg == "--"+defaults.JSONFlagName {
			return true
		}
	}
	return false
}

func assertReviewGolden(t *testing.T, name, got string) {
	t.Helper()
	want, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	normalized := strings.ReplaceAll(got, redact.Version(), "RULE_SET_VERSION")
	if normalized != string(want) {
		t.Errorf("%s differs:\ngot:\n%s\nwant:\n%s", name, normalized, want)
	}
}

func assertReviewDocument(t *testing.T, run reviewRun, stdout, context string) {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(stdout))
	var document sessionsReviewDocument
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("stdout is not one document: %v\n%s", err, context)
	}
	if decoder.More() {
		t.Fatalf("stdout holds more than one document\n%s", context)
	}
	if !strings.Contains(stdout, `"warnings": [`) {
		t.Errorf("warnings must be an array, never null or missing\n%s", context)
	}
	rows := map[string]sessionReviewRow{}
	for _, row := range document.Sessions {
		rows[row.SessionID] = row
		if row.Findings == nil {
			t.Errorf("findings of %s is null\n%s", row.SessionID, context)
		}
	}
	for id, want := range run.WantStatus {
		if got := string(rows[id].Status); got != want {
			t.Errorf("status of %s = %q, want %q\n%s", id, got, want, context)
		}
	}
	for id, want := range run.WantKinds {
		var got []string
		for _, finding := range rows[id].Findings {
			got = append(got, string(finding.Kind))
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("kinds of %s = %v, want %v\n%s", id, got, want, context)
		}
	}
	for id, want := range run.WantReason {
		if !strings.Contains(rows[id].Reason, want) {
			t.Errorf("reason of %s = %q, want %q\n%s", id, rows[id].Reason, want, context)
		}
	}
	var warned []string
	for _, warning := range document.Warnings {
		warned = append(warned, warning.SessionID)
	}
	if strings.Join(warned, ",") != strings.Join(run.WantWarnings, ",") {
		t.Errorf("warnings = %v, want %v\n%s", warned, run.WantWarnings, context)
	}
}

// TestBuildRedactorParity proves both commands build the same redactor from
// the same flags and config (case redactor_parity).
func TestBuildRedactorParity(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfgPath := writeCfg(t, dir, "parity.yaml", "version: 1\nredaction:\n  level: minimal\n  custom_patterns:\n    - id: parity\n      pattern: PARITY-[0-9]+\n      category: secrets\n      replacement: <PARITY>\n")
	build := func(refusal redactionRefusalText, load func(string) (*config.Config, error)) (builtRedactor, string) {
		t.Helper()
		root := newTestRoot()
		var built builtRedactor
		var buildErr error
		var stderr bytes.Buffer
		sub := &cobra.Command{Use: "probe", RunE: func(cmd *cobra.Command, _ []string) error {
			built, buildErr = buildRedactor(cmd, "", refusal, load)
			return nil
		}}
		root.AddCommand(sub)
		root.SetErr(&stderr)
		root.SetArgs([]string{"--config", cfgPath, "--config-dir", dir, "--data-dir", dir, "--state-dir", dir, "probe"})
		if err := root.Execute(); err != nil {
			t.Fatal(err)
		}
		if buildErr != nil {
			t.Fatal(buildErr)
		}
		return built, stderr.String()
	}
	redactBuilt, redactNotes := build(redactOperationRefusal, loadConfig)
	reviewBuilt, reviewNotes := build(sessionsReviewRefusal, func(path string) (*config.Config, error) {
		return config.Parse(mustReadFile(t, path))
	})
	if redactBuilt.Level != reviewBuilt.Level || redactBuilt.Level != redact.RedactionLevel("standard") {
		t.Errorf("levels = %q and %q, want the raised standard", redactBuilt.Level, reviewBuilt.Level)
	}
	if redactNotes != reviewNotes || redactNotes == "" {
		t.Errorf("disclosures differ or are missing: %q and %q", redactNotes, reviewNotes)
	}
	const sample = "code PARITY-1234 and mail someone@example.org at /home/alice/work"
	if a, b := redactBuilt.Redactor.RedactText(sample), reviewBuilt.Redactor.RedactText(sample); a != b || strings.Contains(a, "PARITY-1234") {
		t.Errorf("redacted texts differ or keep the custom pattern: %q and %q", a, b)
	}
}
