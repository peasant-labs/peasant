package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/opencode_native_cli.yaml
var nativeCLIYAML []byte

type nativeCLICase struct {
	Name            string `yaml:"name"`
	SourceFixture   string `yaml:"source_fixture"`
	SessionID       string `yaml:"session_id"`
	UserText        string `yaml:"user_text"`
	AssistantText   string `yaml:"assistant_text"`
	UpdatedUserText string `yaml:"updated_user_text"`
	Version         string `yaml:"version"`
	Model           string `yaml:"model"`
	InputTokens     int    `yaml:"input_tokens"`
	OutputTokens    int    `yaml:"output_tokens"`
	Mutation        string `yaml:"mutation"`
}

func loadNativeCLIFixtures(t *testing.T) []nativeCLICase {
	t.Helper()
	var fixture struct {
		RequiredCases []string        `yaml:"required_cases"`
		Cases         []nativeCLICase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(nativeCLIYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("load native CLI fixtures: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("load native CLI fixtures: expected exactly one YAML document, got %v", err)
	}
	names := make(map[string]bool)
	for _, c := range fixture.Cases {
		if c.Name == "" || names[c.Name] || c.SourceFixture == "" || c.SessionID == "" || c.UserText == "" || c.AssistantText == "" || c.UpdatedUserText == c.UserText || c.UpdatedUserText == "" || c.Version == "" || c.Model == "" || c.Mutation == "" || c.InputTokens <= 0 || c.OutputTokens <= 0 {
			t.Fatalf("load native CLI fixtures: duplicate or incomplete case %q", c.Name)
		}
		names[c.Name] = true
	}
	if len(fixture.RequiredCases) == 0 {
		t.Fatal("load native CLI fixtures: required-name manifest is empty")
	}
	for _, name := range fixture.RequiredCases {
		if !names[name] {
			t.Fatalf("load native CLI fixtures: required case %q is missing", name)
		}
	}
	return fixture.Cases
}

// TestOpenCodeNativeCLI exercises the shipped harvest command against sources
// created by testfixture. Neither the OpenCode executable nor a user's source
// database is involved. The child process receives only test-owned directories.
func TestOpenCodeNativeCLI(t *testing.T) {
	if testing.Short() {
		t.Skip("built-binary regression is excluded by -short")
	}
	cases := loadNativeCLIFixtures(t)
	bin := filepath.Join(t.TempDir(), "peasant")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, "go", "build", "-race", "-mod=readonly", "-o", bin, "./cmd/peasant")
	build.Dir = filepath.Join("..", "..")
	// The enclosing Go test has already resolved the module graph. A missing
	// build-only dependency is an actionable setup failure, never a network fetch.
	build.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build race-instrumented Peasant before native CLI regression: %v\nResolve the repository's build dependencies in the development shell and rerun.\n%s", err, output)
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			source := testfixture.MaterializeByName(t, c.SourceFixture)
			project := testfixture.PrepareNativeCLI(t, source)
			run := newNativeCLIRun(t, bin, source, project)
			first := run.harvest(t, source)
			if first.New != 1 || first.Updated != 0 || first.Unchanged != 0 || first.Indexed != 1 {
				t.Fatalf("initial harvest summary = %+v, want one new indexed session", first)
			}
			before := run.assertStored(t, c, project, c.UserText)
			repeat := run.harvest(t, source)
			if repeat.New != 0 || repeat.Updated != 0 || repeat.Unchanged != 1 || repeat.Indexed != 0 {
				t.Fatalf("repeat harvest summary = %+v, want one unchanged session without reindexing", repeat)
			}
			after := run.assertStored(t, c, project, c.UserText)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("unchanged harvest modified persisted entries or metadata")
			}
			testfixture.ApplyNativeMutationByName(t, source, c.Mutation)
			updated := run.harvest(t, source)
			if updated.New != 0 || updated.Updated != 1 || updated.Unchanged != 0 || updated.Indexed != 1 {
				t.Fatalf("changed harvest summary = %+v, want one updated indexed session", updated)
			}
			changed := run.assertStored(t, c, project, c.UpdatedUserText)
			settled := run.harvest(t, source)
			if settled.New != 0 || settled.Updated != 0 || settled.Unchanged != 1 || settled.Indexed != 0 {
				t.Fatalf("harvest after update summary = %+v, want one unchanged session without reindexing", settled)
			}
			if !reflect.DeepEqual(changed, run.assertStored(t, c, project, c.UpdatedUserText)) {
				t.Fatal("harvest after settled update modified persisted entries or metadata")
			}
		})
	}
}

type nativeCLIRun struct {
	bin, root, configPath, dataDir, outputDir string
	env                                       []string
}

func newNativeCLIRun(t *testing.T, bin string, source testfixture.MaterializedSource, project string) nativeCLIRun {
	t.Helper()
	root := t.TempDir()
	run := nativeCLIRun{bin: bin, root: root, configPath: filepath.Join(root, "config", "peasant", "config.yaml"), dataDir: filepath.Join(root, "data"), outputDir: filepath.Join(root, "output")}
	run.env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CEILING_DIRECTORIES=" + root + string(os.PathListSeparator) + filepath.Dir(project), "GORACE=halt_on_error=1 atexit_sleep_ms=0"}
	// Keep the real child process within the same resource bounds as package
	// tests; the isolated environment does not inherit TestMain's overrides.
	run.env = append(run.env, ingest.EnvArenaSizeBytes+"=67108864", store.EnvPoolSize+"=1")
	for key, name := range map[string]string{string(defaults.EnvXDGConfigHome): "config", string(defaults.EnvXDGDataHome): "data", string(defaults.EnvXDGStateHome): "state", "XDG_CACHE_HOME": "cache", "XDG_RUNTIME_DIR": "runtime", "TMPDIR": "tmp"} {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		run.env = append(run.env, key+"="+dir)
	}
	cfg := &config.Config{
		Version:   defaults.ConfigVersion,
		User:      config.UserConfig{Email: "fixture@example.test"},
		Redaction: config.RedactionConfig{Level: redact.Standard},
		Sources:   config.SourcesConfig{OpenCode: config.SourceProviderConfig{Enabled: true, Paths: []string{filepath.Dir(source.Path)}}},
		Output:    config.OutputConfig{BasePath: run.outputDir, StalenessThresholdSec: 1},
		Selection: config.SelectionConfig{Mode: config.SelectionModeAll},
	}
	if err := config.SaveAtomic(run.configPath, cfg); err != nil {
		t.Fatalf("write isolated harvest config: %v", err)
	}
	return run
}

func (run nativeCLIRun) harvest(t *testing.T, source testfixture.MaterializedSource) ingest.PipelineSummary {
	t.Helper()
	// Capture after each intentional fixture update, and compare even if the
	// production command fails, including database and WAL/SHM presence.
	before := testfixture.SnapshotSource(t, source)
	defer testfixture.AssertUnchanged(t, source, before)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, run.bin, "--config", run.configPath, "--data-dir", run.dataDir, "--state-dir", filepath.Join(run.root, "state"), "harvest", "--source-provider", string(defaults.HarnessOpenCode), "--source-path", filepath.Dir(source.Path), "--output", run.outputDir, "--json")
	cmd.Dir, cmd.Env = run.root, run.env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("isolated Peasant harvest failed: %v\nstdout: %s\nstderr: %s", err, &stdout, &stderr)
	}
	var result struct {
		Summary  ingest.PipelineSummary `json:"summary"`
		Sessions []struct {
			Error string `json:"error"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode harvest result: %v\nstdout: %s\nstderr: %s", err, &stdout, &stderr)
	}
	if result.Summary.Errors != 0 || result.Summary.StoreError != nil {
		t.Fatalf("harvest reported errors: %s\nstderr: %s", &stdout, &stderr)
	}
	for _, session := range result.Sessions {
		if session.Error != "" {
			t.Fatalf("harvest session failed: %s", session.Error)
		}
	}
	if result.Summary.New+result.Summary.Updated+result.Summary.Unchanged == 0 {
		t.Fatalf("harvest discovered no fixture session: %s\nstderr: %s", &stdout, &stderr)
	}
	return result.Summary
}

type nativeCLIStored struct {
	Entries  []schema.SessionEntry
	Metadata ingest.UnifiedMetadata
}

func (run nativeCLIRun) assertStored(t *testing.T, c nativeCLICase, project, userText string) nativeCLIStored {
	t.Helper()
	db, err := store.Open(string(defaults.ResolveDBFilePathWith(run.dataDir)))
	if err != nil {
		t.Fatalf("open test-owned harvest store: %v", err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ids, err := db.AllSessionIDs(ctx)
	if err != nil || !reflect.DeepEqual(ids, []string{c.SessionID}) {
		t.Fatalf("persisted session IDs = %v, error = %v; want only %s", ids, err, c.SessionID)
	}
	row, err := db.SessionByID(ctx, c.SessionID)
	if err != nil || row == nil {
		t.Fatalf("read persisted session: row=%v error=%v", row, err)
	}
	if row.ModelHarness != string(ingest.HarnessOpenCode) || row.ModelID != c.Model || row.IndexedAt == nil {
		t.Fatalf("persisted session lost harness, model, or successful index evidence: %+v", row)
	}
	entries, err := db.ListEntries(ctx, ingest.SessionID(c.SessionID))
	if err != nil {
		t.Fatalf("read persisted entries: %v", err)
	}
	userCount, assistantCount := 0, 0
	for _, entry := range entries {
		if entry.ContentPreview == nil {
			continue
		}
		if entry.Role == schema.RoleUser && *entry.ContentPreview == userText {
			userCount++
		}
		if entry.Role == schema.RoleAssistant && *entry.ContentPreview == c.AssistantText {
			assistantCount++
		}
		if userText != c.UserText && *entry.ContentPreview == c.UserText {
			t.Fatal("reindex retained stale user content")
		}
	}
	if userCount != 1 || assistantCount != 1 {
		t.Fatalf("stored native text occurrences: user=%d assistant=%d; entries=%+v", userCount, assistantCount, entries)
	}
	metadataPath := ingest.SessionMetadataPath(run.outputDir, row.HostSlug, c.SessionID, "")
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatalf("read persisted metadata: %v", err)
	}
	var metadata ingest.UnifiedMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		t.Fatalf("decode persisted metadata: %v", err)
	}
	if metadata.CWD != project || metadata.Version != c.Version || metadata.Stats.TokensIn != c.InputTokens || metadata.Stats.TokensOut != c.OutputTokens {
		t.Fatalf("native session metadata did not survive production harvest: %+v", metadata)
	}
	return nativeCLIStored{Entries: entries, Metadata: metadata}
}
