package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
)

const mountedOpenCodeRemote = "git@github.com:acme/tool.git"

type mountedOpenCodeSessionTime struct {
	Created int64 `json:"created"`
	Updated int64 `json:"updated"`
}

type mountedOpenCodeSessionPayload struct {
	ID        string                     `json:"id"`
	Slug      string                     `json:"slug"`
	Version   string                     `json:"version"`
	ProjectID string                     `json:"projectID"`
	Directory string                     `json:"directory"`
	ParentID  string                     `json:"parentID"`
	Title     string                     `json:"title"`
	Time      mountedOpenCodeSessionTime `json:"time"`
}

type mountedOpenCodeWorld struct {
	root      string
	cloneA    string
	cloneB    string
	sessionA  string
	sessionB  string
	projectA  string
	projectB  string
	outputDir string
}

func newMountedOpenCodeWorld(t *testing.T) mountedOpenCodeWorld {
	t.Helper()
	base := t.TempDir()
	world := mountedOpenCodeWorld{
		root:      filepath.Join(base, defaults.HarnessOpenCode.String()),
		cloneA:    filepath.Join(base, "team-a", "tool"),
		cloneB:    filepath.Join(base, "team-b", "tool"),
		sessionA:  "ses_111111111111111111111111",
		sessionB:  "ses_222222222222222222222222",
		projectA:  "project-a",
		projectB:  "project-b",
		outputDir: filepath.Join(base, "output"),
	}
	for _, directory := range []string{world.cloneA, world.cloneB, world.outputDir} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatalf("create mounted OpenCode directory %q: %v", directory, err)
		}
	}
	writeMountedOpenCodeSession(t, world.root, world.projectA, world.sessionA, world.cloneA, "clone A session")
	writeMountedOpenCodeSession(t, world.root, world.projectB, world.sessionB, world.cloneB, "clone B session")
	return world
}

func writeMountedOpenCodeSession(t *testing.T, root, projectID, sessionID, directory, title string) {
	t.Helper()
	payload := mountedOpenCodeSessionPayload{
		ID:        sessionID,
		Slug:      "test",
		Version:   "1.1.53",
		ProjectID: projectID,
		Directory: directory,
		Title:     title,
		Time: mountedOpenCodeSessionTime{
			Created: 1770000000000,
			Updated: 1770000100000,
		},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode mounted OpenCode session %s: %v", sessionID, err)
	}
	path := filepath.Join(root, defaults.OpenCodeDirStorage.String(), defaults.OpenCodeDirSession.String(), projectID, sessionID+defaults.ExtJSON.String())
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create mounted OpenCode session directory for %s: %v", sessionID, err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write mounted OpenCode session %s: %v", sessionID, err)
	}
}

func mountedOpenCodeConfig(t *testing.T, root string) *config.Config {
	t.Helper()
	cfg := config.BaseConfig()
	for harness := range ingest.DefaultAdapterRegistry {
		source, ok := cfg.Sources.Provider(harness)
		if !ok {
			t.Fatalf("registered harness %q has no source configuration", harness)
		}
		source.Enabled = false
		source.Paths = nil
	}
	source, _ := cfg.Sources.Provider(defaults.HarnessOpenCode)
	source.Enabled = true
	source.Paths = []string{root}
	return cfg
}

type mountedOpenCodeGitResolver struct {
	*testutil.StubGitResolver
	allowed map[string]bool
}

func newMountedOpenCodeGitResolver(directories ...string) *mountedOpenCodeGitResolver {
	allowed := make(map[string]bool, len(directories))
	for _, directory := range directories {
		allowed[filepath.Clean(directory)] = true
	}
	return &mountedOpenCodeGitResolver{
		StubGitResolver: &testutil.StubGitResolver{
			Remote:             mountedOpenCodeRemote,
			BranchName:         "main",
			TrackingBranchName: "origin/main",
			Email:              "test@example.invalid",
		},
		allowed: allowed,
	}
}

func (r *mountedOpenCodeGitResolver) checkDirectory(operation, directory string) error {
	if r.allowed[filepath.Clean(directory)] {
		return nil
	}
	return fmt.Errorf("mounted OpenCode %s received directory %q outside the session clone set; the adapter must supply the session working directory", operation, directory)
}

func (r *mountedOpenCodeGitResolver) RemoteURL(_ context.Context, directory string) (string, error) {
	if err := r.checkDirectory("remote lookup", directory); err != nil {
		return "", err
	}
	return mountedOpenCodeRemote, nil
}

func (r *mountedOpenCodeGitResolver) Branch(_ context.Context, directory string) (string, error) {
	if err := r.checkDirectory("branch lookup", directory); err != nil {
		return "", err
	}
	return "main", nil
}

func (r *mountedOpenCodeGitResolver) Worktree(_ context.Context, directory string) (string, error) {
	if err := r.checkDirectory("worktree lookup", directory); err != nil {
		return "", err
	}
	return filepath.Clean(directory), nil
}

func TestOpenCodeProjectDirectoriesReachKickstartListings(t *testing.T) {
	world := newMountedOpenCodeWorld(t)
	git := newMountedOpenCodeGitResolver(world.cloneA, world.cloneB)
	inventory, listings := ftueDiscoverWith(
		t.Context(), mountedOpenCodeConfig(t, world.root), &ingest.OSFileSystem{}, git, nil, nil,
	)
	if got := inventory[defaults.HarnessOpenCode].SessionCount; got != 2 {
		t.Fatalf("OpenCode inventory count = %d, want 2 sessions from distinct clones", got)
	}
	if len(listings) != 2 {
		t.Fatalf("kickstart listed %d OpenCode sessions, want 2", len(listings))
	}

	byID := make(map[string]string, len(listings))
	projectDirectories := make(map[string]bool, len(listings))
	for _, listing := range listings {
		if listing.GitRemote != mountedOpenCodeRemote {
			t.Errorf("session %s remote = %q, want shared remote %q", listing.SessionID, listing.GitRemote, mountedOpenCodeRemote)
		}
		if listing.ProjectName != "tool" {
			t.Errorf("session %s project name = %q, want shared display name %q", listing.SessionID, listing.ProjectName, "tool")
		}
		byID[listing.SessionID] = listing.WorkingDir
		projectDirectories[listing.WorkingDir] = true
	}
	if got := byID[world.sessionA]; got != world.cloneA {
		t.Errorf("clone A listing working directory = %q, want %q", got, world.cloneA)
	}
	if got := byID[world.sessionB]; got != world.cloneB {
		t.Errorf("clone B listing working directory = %q, want %q", got, world.cloneB)
	}
	if len(projectDirectories) != 2 {
		t.Fatalf("kickstart retained %d project directories, want both same-remote clones; got %v", len(projectDirectories), projectDirectories)
	}
}

func TestKickstartReuseFallsBackToRecordedOpenCodeDirectories(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, defaults.HarnessOpenCode.String())
	worktree := filepath.Join(base, "recorded-worktree", "tool")
	canonical := filepath.Join(base, "recorded-canonical", "tool")
	for _, directory := range []string{worktree, canonical} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatalf("create recorded OpenCode directory %q: %v", directory, err)
		}
	}
	const worktreeSession = "ses_333333333333333333333333"
	const canonicalSession = "ses_444444444444444444444444"
	writeMountedOpenCodeSession(t, root, "recorded-worktree", worktreeSession, "", "recorded worktree")
	writeMountedOpenCodeSession(t, root, "recorded-canonical", canonicalSession, "", "recorded canonical cwd")

	ingestedMs := time.Now().Add(time.Hour).UnixMilli()
	known := knownSessionIndex{
		worktreeSession: {
			GitRemote:     mountedOpenCodeRemote,
			Branch:        "main",
			GitWorktree:   worktree,
			CanonicalCwd:  canonical,
			IngestedMs:    ingestedMs,
			SchemaVersion: ingest.CurrentSchemaVersion,
		},
		canonicalSession: {
			GitRemote:     mountedOpenCodeRemote,
			Branch:        "main",
			CanonicalCwd:  canonical,
			IngestedMs:    ingestedMs,
			SchemaVersion: ingest.CurrentSchemaVersion,
		},
	}
	_, listings := ftueDiscoverWith(
		t.Context(), mountedOpenCodeConfig(t, root), &ingest.OSFileSystem{}, testutil.NoGitResolver(), known, nil,
	)
	if len(listings) != 2 {
		t.Fatalf("kickstart listed %d reusable OpenCode sessions, want 2", len(listings))
	}
	byID := make(map[string]string, len(listings))
	for _, listing := range listings {
		byID[listing.SessionID] = listing.WorkingDir
	}
	if got := byID[worktreeSession]; got != worktree {
		t.Errorf("stored worktree fallback = %q, want %q before canonical cwd %q", got, worktree, canonical)
	}
	if got := byID[canonicalSession]; got != canonical {
		t.Errorf("stored canonical cwd fallback = %q, want %q when worktree is absent", got, canonical)
	}
}

func TestOpenCodeProjectDirectoriesReachHarvestCohortPreparation(t *testing.T) {
	world := newMountedOpenCodeWorld(t)
	sourceBefore := snapshotMountedOpenCodeJSON(t, world.root)
	git := newMountedOpenCodeGitResolver(world.cloneA, world.cloneB)
	cfg := mountedOpenCodeConfig(t, world.root)
	cfg.Selection = config.SelectionConfig{
		Mode: config.SelectionModeSelected,
		Harnesses: map[string]config.SelectionHarnessConfig{
			defaults.HarnessOpenCode.String(): {
				Projects: []config.ProjectSelection{{
					GitRemote:  mountedOpenCodeRemote,
					ClonePaths: []string{world.cloneA},
					Branches:   []string{"main"},
				}},
			},
		},
	}
	filter, _ := buildSelectionFilterWithResolver(cfg, git, ingest.NewPhysicalPathResolver())
	outputDir, err := ingest.NewResolvedPath(world.outputDir)
	if err != nil {
		t.Fatalf("resolve harvest output directory: %v", err)
	}
	sourceRoot, err := ingest.NewResolvedPath(world.root)
	if err != nil {
		t.Fatalf("resolve mounted OpenCode source root: %v", err)
	}
	pipeline, err := ingest.NewPipeline(
		&ingest.OSFileSystem{},
		git,
		map[ingest.Harness]ingest.AdapterFactory{
			defaults.HarnessOpenCode: ingest.DefaultAdapterRegistry[defaults.HarnessOpenCode],
		},
		ingest.PipelineConfig{
			Sources: map[ingest.Harness]ingest.SourceConfig{
				defaults.HarnessOpenCode: {
					Enabled: true,
					Paths:   []ingest.ResolvedPath{sourceRoot},
				},
			},
			OutputDir:            outputDir,
			Force:                true,
			Parallelism:          1,
			PrepareSessionFilter: filter.Prepare,
			SessionFilter:        filter.Match,
		},
	)
	if err != nil {
		t.Fatalf("construct mounted OpenCode harvest pipeline: %v", err)
	}
	result, err := pipeline.Run(t.Context())
	if err != nil {
		t.Fatalf("run mounted OpenCode harvest pipeline: %v", err)
	}
	if len(result.Sessions) != 2 {
		t.Fatalf("harvest reported %d OpenCode sessions, want complete cohort of 2", len(result.Sessions))
	}
	byID := make(map[ingest.SessionID]ingest.SessionResult, len(result.Sessions))
	for _, session := range result.Sessions {
		byID[session.SessionID] = session
	}
	selected := byID[ingest.SessionID(world.sessionA)]
	if selected.Error != nil || selected.OutputPath == "" || selected.Status != ingest.DiffNew {
		t.Errorf("selected clone result = status %v output %q error %v, want ingested new session", selected.Status, selected.OutputPath, selected.Error)
	}
	excluded := byID[ingest.SessionID(world.sessionB)]
	if excluded.Error != nil || excluded.OutputPath != "" || excluded.Status != ingest.DiffUnchanged {
		t.Errorf("unselected sibling result = status %v output %q error %v, want filtered unchanged result", excluded.Status, excluded.OutputPath, excluded.Error)
	}
	if sourceAfter := snapshotMountedOpenCodeJSON(t, world.root); !reflect.DeepEqual(sourceAfter, sourceBefore) {
		t.Errorf("mounted legacy OpenCode JSON source bytes changed during harvest: before=%v after=%v", sourceBefore, sourceAfter)
	}
}

func TestOpenCodeSQLiteEvidenceCannotEnterMountedProductionIngest(t *testing.T) {
	t.Parallel()
	materialized := testfixture.MaterializeByName(t, "current-session-message")
	sourceRoot := filepath.Dir(materialized.Path)
	prober, err := ingest.NewOpenCodeCandidateProber(&ingest.OSFileSystem{}, ingest.OpenOpenCodeSQLiteSource, ingest.DefaultOpenCodeSQLiteSourceOptions())
	if err != nil {
		t.Fatalf("construct mounted OpenCode candidate prober: %v", err)
	}
	probes := prober.Probe(t.Context(), []ingest.OpenCodeCandidate{{
		Path:       materialized.Path,
		Kind:       ingest.OpenCodeSourceSQLite,
		Provenance: ingest.OpenCodeCandidateChannel,
	}})
	if len(probes) != 1 || probes[0].Capability != ingest.OpenCodeCapabilityCurrent || probes[0].Support != ingest.OpenCodeSupportSupported {
		t.Fatalf("mounted SQLite precondition did not produce supported current evidence: %+v", probes)
	}

	git := testutil.NoGitResolver()
	inventory, listings := ftueDiscoverWith(t.Context(), mountedOpenCodeConfig(t, sourceRoot), &ingest.OSFileSystem{}, git, nil, nil)
	if got := inventory[defaults.HarnessOpenCode].SessionCount; got != 0 || len(listings) != 0 {
		t.Fatalf("kickstart exposed SQLite-only evidence: inventory=%d listings=%d", got, len(listings))
	}

	commandRoot := t.TempDir()
	outputRoot := filepath.Join(commandRoot, "managed")
	output, err := executeHarvestCmd(t, commandRoot, []string{
		"--source-provider=" + defaults.HarnessOpenCode.String(),
		"--source-path=" + sourceRoot,
		"--output=" + outputRoot,
		"--force",
	})
	if err != nil {
		t.Fatalf("run mounted harvest CLI against SQLite-only evidence: %v\n%s", err, output)
	}
	managedEntries, err := os.ReadDir(outputRoot)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("inspect mounted harvest output: %v", err)
	}
	if len(managedEntries) != 0 {
		t.Fatalf("SQLite-only evidence created %d managed output artifacts", len(managedEntries))
	}

	databasePath := defaults.ResolveDBFilePathWith(commandRoot).String()
	localStore, err := store.Open(databasePath, store.WithPoolSize(1))
	if err != nil {
		t.Fatalf("open mounted harvest store: %v", err)
	}
	defer func() {
		if err := localStore.Close(); err != nil {
			t.Errorf("close mounted harvest store: %v", err)
		}
	}()
	rows, err := localStore.ListSessionsFiltered(t.Context(), store.SessionListFilter{})
	if err != nil {
		t.Fatalf("list mounted harvest store rows: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("SQLite-only evidence created %d local session rows", len(rows))
	}
}

func snapshotMountedOpenCodeJSON(t testing.TB, root string) map[string]string {
	t.Helper()
	snapshot := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(path) != defaults.ExtJSON.String() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		snapshot[relative] = string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot mounted OpenCode JSON source: %v", err)
	}
	return snapshot
}
