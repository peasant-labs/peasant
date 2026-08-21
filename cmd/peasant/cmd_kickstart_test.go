package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/schema"
	"github.com/spf13/cobra"
)

func TestKickstartDiscoveryMountsCurrentOnlyOpenCodeSession(t *testing.T) {
	materialized := testfixture.MaterializeByName(t, "semantic-parity-current")
	t.Setenv("OPENCODE_DB", materialized.Path)
	cfg := config.BaseConfig()
	cfg.Sources.OpenCode.Enabled = true
	cfg.Sources.OpenCode.Paths = []string{filepath.Dir(materialized.Path)}
	cfg.Sources.ClaudeCode.Paths = nil
	cfg.Sources.Codex.Paths = nil
	cfg.Sources.Cursor.Paths = nil
	cfg.Sources.Strike.Paths = nil

	inventory, sessions := ftueDiscoverWith(t.Context(), cfg, &ingest.OSFileSystem{}, testutil.NoGitResolver(), nil, nil, nil)
	discovery := inventory[defaults.HarnessOpenCode]
	if discovery.SessionCount != 1 || discovery.State == ftue.DiscoveryFailed {
		t.Fatalf("kickstart current-only inventory = %+v, want one available OpenCode session", discovery)
	}
	if len(sessions) != 1 || sessions[0].Harness != defaults.HarnessOpenCode.String() || sessions[0].SessionID != testutil.TestOpenCodeSesID {
		t.Fatalf("kickstart current-only sessions = %+v, want the mounted synthetic OpenCode session", sessions)
	}
}

// TestBuildKickstartCommandMountsProjectFirstScope exercises the RETAINED legacy
// FTUE wizard's project-first scope behavior. The default kickstart command now
// mounts the settings.Flow rebuild (covered by the internal/tui/kickstart program
// smokes); the legacy wizard remains shipping, deprecation-candidate code, so this
// drives it through the retained runLegacyFTUEWizard entry point rather than
// cmd.Execute.
func TestBuildKickstartCommandMountsProjectFirstScope(t *testing.T) {
	sessions := []ftue.SessionListing{
		{Harness: defaults.HarnessClaudeCode.String(), ProjectName: "tool", GitRemote: "git@github.com:acme/tool.git", WorkingDir: "/work/acme/tool", SessionID: "11111111-1111-1111-1111-111111111111"},
		{Harness: defaults.HarnessOpenCode.String(), ProjectName: "tool", GitRemote: "https://github.com/acme/tool.git", WorkingDir: "/work/acme/tool", SessionID: "22222222-2222-2222-2222-222222222222"},
	}
	inventory := ftue.ProviderInventory{
		defaults.HarnessClaudeCode: {SessionCount: 1, Enabled: true},
		defaults.HarnessOpenCode:   {SessionCount: 1, Enabled: true},
	}
	var projectFrame, scopeFrame, filteredScopeFrame, nextFrame, destinationFrame, consentFrame string
	deps := kickstartCommandDeps{
		discover: func(context.Context, string, string, *discoverySpinner) (ftue.ProviderInventory, []ftue.SessionListing) {
			return inventory, sessions
		},
		getwd: func() (string, error) { return "/work/acme/tool", nil },
		run: func(model ftue.WizardModel) error {
			updated, _ := model.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
			model = updated.(ftue.WizardModel)
			updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyDown})
			model = updated.(ftue.WizardModel)
			updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			model = updated.(ftue.WizardModel)
			projectFrame = model.View().Content
			if strings.Contains(projectFrame, "[ ] tool") {
				updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeySpace})
				model = updated.(ftue.WizardModel)
			}
			updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			model = updated.(ftue.WizardModel)
			scopeFrame = model.View().Content
			updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyTab})
			model = updated.(ftue.WizardModel)
			updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyDown})
			model = updated.(ftue.WizardModel)
			updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeySpace})
			model = updated.(ftue.WizardModel)
			filteredScopeFrame = model.View().Content
			updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			model = updated.(ftue.WizardModel)
			nextFrame = model.View().Content
			for range 4 {
				updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
				model = updated.(ftue.WizardModel)
			}
			destinationFrame = model.View().Content
			updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			model = updated.(ftue.WizardModel)
			consentFrame = model.View().Content
			return nil
		},
	}
	// The legacy wizard is retained as a deprecation candidate; with no flow
	// runner injected, the command falls back to it, so cmd.Execute drives the
	// legacy project-first path exactly as before.
	cmd := buildKickstartCommand(deps)
	cmd.SetContext(t.Context())
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute mounted kickstart: %v", err)
	}
	_ = inventory
	if strings.Contains(projectFrame, "Select Providers") {
		t.Fatalf("first scope decision remained harness-first:\n%s", projectFrame)
	}
	if !strings.Contains(projectFrame, "Select Projects") || strings.Count(projectFrame, "tool (2 sessions, 2 harnesses)") != 1 {
		t.Fatalf("mounted project-first frame did not aggregate the cross-harness project once:\n%s", projectFrame)
	}
	if !strings.Contains(scopeFrame, "Narrow Project Scope") || strings.Count(scopeFrame, "tool") != 1 {
		t.Fatalf("mounted narrow page is not project-rooted:\n%s", scopeFrame)
	}
	if strings.Contains(scopeFrame, "Claude Code (1 sessions)") || strings.Contains(scopeFrame, "OpenCode (1 sessions)") {
		t.Fatalf("mounted narrow page restored a harness grouping:\n%s", scopeFrame)
	}
	if strings.Count(scopeFrame, "Harnesses") != 1 || strings.Count(scopeFrame, "Claude Code (1)") != 1 || strings.Count(scopeFrame, "OpenCode (1)") != 1 {
		t.Fatalf("scope page did not render the one global harness control on the right:\n%s", scopeFrame)
	}
	if !strings.Contains(filteredScopeFrame, "[ ] OpenCode (1)") {
		t.Fatalf("right-column harness toggle did not update independently:\n%s", filteredScopeFrame)
	}
	if strings.Contains(nextFrame, "Select Providers") || !strings.Contains(nextFrame, "Auto-Ingest New Branches") {
		t.Fatalf("scope confirmation did not proceed directly to auto-ingest:\n%s", nextFrame)
	}
	if !strings.Contains(destinationFrame, "Choose Destination") || !strings.Contains(destinationFrame, "You skipped login") || !strings.Contains(destinationFrame, "Keep local only") {
		t.Fatalf("mounted command did not preserve login-or-skip local destination:\n%s", destinationFrame)
	}
	if !strings.Contains(consentFrame, "Final Consent") || !strings.Contains(consentFrame, "Destination:  local") || !strings.Contains(consentFrame, "Redaction:    Standard") {
		t.Fatalf("mounted command did not reach truthful final consent:\n%s", consentFrame)
	}
}

var _ = (*cobra.Command)(nil) // keep cobra import for TestKickstartFtueAlias

// TestKickstartFtueAlias verifies "ftue" is an alias of "kickstart", not a separate command (5vg).
func TestKickstartFtueAlias(t *testing.T) {
	t.Parallel()
	rootCmd := buildTestRootCmd()

	// "kickstart" should exist as a direct subcommand.
	var kickstart *cobra.Command
	for _, c := range rootCmd.Commands() {
		if c.Name() == "kickstart" {
			kickstart = c
			break
		}
	}
	if kickstart == nil {
		t.Fatal("kickstart command not found")
	}

	// "ftue" should be an alias, not a separate command.
	if !containsString(kickstart.Aliases, "ftue") {
		t.Errorf("kickstart should have 'ftue' alias; aliases: %v", kickstart.Aliases)
	}

	// There should NOT be a separate "ftue" command at the root level.
	for _, c := range rootCmd.Commands() {
		if c.Name() == "ftue" {
			t.Error("'ftue' should be an alias of kickstart, not a separate command")
		}
	}
}

// TestRemoveIfExists_RemovesExistingFile verifies that removeIfExists deletes a file.
func TestRemoveIfExists_RemovesExistingFile(t *testing.T) {
	t.Parallel()
	path := t.TempDir() + "/config.yaml"
	if err := os.WriteFile(path, []byte("version: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := removeIfExists(path); err != nil {
		t.Fatalf("removeIfExists() error: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("removeIfExists should have deleted the file")
	}
}

// TestRemoveIfExists_MissingFileIsNonFatal verifies removeIfExists is a no-op when the
// file does not exist.
func TestRemoveIfExists_MissingFileIsNonFatal(t *testing.T) {
	t.Parallel()
	if err := removeIfExists(t.TempDir() + "/nonexistent.yaml"); err != nil {
		t.Errorf("removeIfExists on missing file should not error, got: %v", err)
	}
}

// TestFtueDiscover_EmptyOnMissingConfig verifies that ftueDiscover returns a
// configured provider inventory without panicking for a first-time user.
func TestFtueDiscover_EmptyOnMissingConfig(t *testing.T) {
	t.Parallel()
	// HOME/XDG are isolated to a throwaway dir by TestMain, so discovery finds no
	// local transcript store or db. This exercises the empty case deterministically.
	inventory, sessions := ftueDiscover(t.Context(), t.TempDir()+"/nonexistent/config.yaml", t.TempDir()+"/nonexistent/peasant.db", nil)
	if inventory == nil {
		t.Error("provider inventory should never be nil")
	}
	defaultConfig := config.BaseConfig()
	for harness := range ingest.DefaultAdapterRegistry {
		discovery, ok := inventory[harness]
		if !ok {
			t.Errorf("registered harness %q is missing from Kickstart inventory", harness)
			continue
		}
		configured, _ := defaultConfig.Sources.Provider(harness)
		if discovery.Enabled != configured.Enabled {
			t.Errorf("registered harness %q inventory enabled = %v, want configured value %v", harness, discovery.Enabled, configured.Enabled)
		}
	}
	// Sessions may be nil or empty for a fresh install with no transcripts.
	_ = sessions
}

func TestFtueDiscover_FindsStrikeBeforeOptIn(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	fixtureName := "20260728T123456.123456789Z-AAAAAAAAAAAAAAAAAAAAAAAAAA.jsonl"
	fixture, err := os.ReadFile(filepath.Join("testdata", "strike", fixtureName))
	if err != nil {
		t.Fatalf("read committed Strike fixture: %v", err)
	}
	strikeDir := filepath.Join(home, ".strike", "sessions")
	if err := os.MkdirAll(strikeDir, 0o700); err != nil {
		t.Fatalf("create default Strike session directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(strikeDir, fixtureName), fixture, 0o600); err != nil {
		t.Fatalf("write isolated Strike fixture: %v", err)
	}

	inventory, sessions := ftueDiscover(t.Context(), filepath.Join(home, "missing-config.yaml"), filepath.Join(home, "missing-peasant.db"), nil)
	strike := inventory[defaults.HarnessStrike]
	if strike.SessionCount != 1 {
		t.Fatalf("Kickstart Strike count = %d, want 1 available session before opt-in", strike.SessionCount)
	}
	if strike.Enabled {
		t.Fatal("Kickstart inventory enabled default-disabled Strike before user selection")
	}
	if len(sessions) != 1 || sessions[0].Harness != defaults.HarnessStrike.String() {
		t.Fatalf("Kickstart sessions = %+v, want the available Strike fixture", sessions)
	}
}

// TestFtueSources_AllProviders returns all configured sources when all providers are selected.
func TestFtueSources_AllProviders(t *testing.T) {
	t.Parallel()
	cfg := config.BaseConfig()
	cfg.Sources.ClaudeCode.Enabled = true
	cfg.Sources.ClaudeCode.Paths = []string{t.TempDir()}
	cfg.Sources.OpenCode.Enabled = true
	cfg.Sources.OpenCode.Paths = []string{t.TempDir()}

	answers := ftue.WizardAnswers{
		WantImport: true,
		ProviderSelections: []ftue.ProviderSelection{
			{Harness: string(defaults.HarnessClaudeCode), ImportAll: true},
			{Harness: string(defaults.HarnessOpenCode), ImportAll: true},
		},
	}

	sources := ftueSources(cfg, answers)
	if _, ok := sources[defaults.HarnessClaudeCode]; !ok {
		t.Error("expected Claude source when selected")
	}
	if _, ok := sources[defaults.HarnessOpenCode]; !ok {
		t.Error("expected OpenCode source when selected")
	}
}

// TestFtueSources_FilteredProvider filters to only the selected providers.
func TestFtueSources_FilteredProvider(t *testing.T) {
	t.Parallel()
	cfg := config.BaseConfig()
	cfg.Sources.ClaudeCode.Enabled = true
	cfg.Sources.ClaudeCode.Paths = []string{t.TempDir()}
	cfg.Sources.OpenCode.Enabled = true
	cfg.Sources.OpenCode.Paths = []string{t.TempDir()}

	answers := ftue.WizardAnswers{
		WantImport: true,
		ProviderSelections: []ftue.ProviderSelection{
			{Harness: string(defaults.HarnessClaudeCode), ImportAll: false},
		},
	}

	sources := ftueSources(cfg, answers)
	if _, ok := sources[defaults.HarnessClaudeCode]; !ok {
		t.Error("expected Claude source in filtered selection")
	}
	if _, ok := sources[defaults.HarnessOpenCode]; ok {
		t.Error("OpenCode should not be included when not selected")
	}
}

// TestFtueSources_EmptySelection returns all sources when no provider selections exist.
func TestFtueSources_EmptySelection(t *testing.T) {
	t.Parallel()
	cfg := config.BaseConfig()
	cfg.Sources.ClaudeCode.Enabled = true
	cfg.Sources.ClaudeCode.Paths = []string{t.TempDir()}

	answers := ftue.WizardAnswers{
		WantImport:         true,
		ProviderSelections: nil,
	}

	sources := ftueSources(cfg, answers)
	// With no selections, ftueSources returns all sources (fallback).
	if _, ok := sources[defaults.HarnessClaudeCode]; !ok {
		t.Error("expected Claude source when no selections specified (fallback to all)")
	}
}

// TestResolveConfiguredSource_DisabledProvider preserves paths for inventory.
func TestResolveConfiguredSource_DisabledProvider(t *testing.T) {
	t.Parallel()
	cfg := config.BaseConfig()
	cfg.Sources.ClaudeCode.Enabled = false

	src, _, ok := resolveConfiguredSource(cfg, defaults.HarnessClaudeCode)
	if !ok {
		t.Fatal("configured Claude source was not found")
	}
	if src.Enabled {
		t.Error("expected disabled SourceConfig for disabled Claude provider")
	}
	if len(src.Paths) == 0 {
		t.Error("disabled provider paths were dropped before Kickstart inventory")
	}
}

// TestResolveConfiguredSource_ValidPaths resolves valid paths into SourceConfig.
func TestResolveConfiguredSource_ValidPaths(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := config.BaseConfig()
	cfg.Sources.ClaudeCode.Enabled = true
	cfg.Sources.ClaudeCode.Paths = []string{dir}

	src, issues, ok := resolveConfiguredSource(cfg, defaults.HarnessClaudeCode)
	if !ok {
		t.Fatal("configured Claude source was not found")
	}
	if len(issues) != 0 {
		t.Fatalf("unexpected path issues: %+v", issues)
	}
	if !src.Enabled {
		t.Error("expected enabled SourceConfig")
	}
	if len(src.Paths) != 1 {
		t.Errorf("expected 1 path, got %d", len(src.Paths))
	}
	if string(src.Paths[0]) != dir {
		t.Errorf("path = %q, want %q", string(src.Paths[0]), dir)
	}
}

// TestResolveConfiguredSource_InvalidPathsReported skips and reports invalid paths.
func TestResolveConfiguredSource_InvalidPathsReported(t *testing.T) {
	t.Parallel()
	cfg := config.BaseConfig()
	cfg.Sources.ClaudeCode.Enabled = true
	cfg.Sources.ClaudeCode.Paths = []string{"relative/path/not/allowed"}

	src, issues, ok := resolveConfiguredSource(cfg, defaults.HarnessClaudeCode)
	if !ok {
		t.Fatal("configured Claude source was not found")
	}
	// Enabled is still true but paths list is empty since invalid paths are skipped.
	if len(src.Paths) != 0 {
		t.Errorf("expected 0 resolved paths for relative input, got %d", len(src.Paths))
	}
	if len(issues) != 1 || issues[0].path != "relative/path/not/allowed" {
		t.Errorf("path issues = %+v, want the rejected configured path", issues)
	}
}

// TestResolveConfiguredSource_UnknownProvider rejects an unconfigured harness.
func TestResolveConfiguredSource_UnknownProvider(t *testing.T) {
	t.Parallel()
	cfg := config.BaseConfig()
	_, _, ok := resolveConfiguredSource(cfg, ingest.Harness("unknown"))
	if ok {
		t.Error("unknown provider unexpectedly resolved a source config")
	}
}

func TestResolveConfiguredSource_CoversDefaultAdapterRegistry(t *testing.T) {
	t.Parallel()
	cfg := config.BaseConfig()
	for harness := range ingest.DefaultAdapterRegistry {
		source, _, ok := resolveConfiguredSource(cfg, harness)
		if !ok {
			t.Errorf("registered harness %q has no source configuration", harness)
			continue
		}
		configured, _ := cfg.Sources.Provider(harness)
		if source.Enabled != configured.Enabled {
			t.Errorf("registered harness %q enabled = %v, want configured value %v", harness, source.Enabled, configured.Enabled)
		}
	}
}

// ---------------------------------------------------------------------------
// filterRootSessions — subagent exclusion
// ---------------------------------------------------------------------------

func TestFilterRootSessions(t *testing.T) {
	t.Parallel()
	parentID := ingest.SessionID("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	tests := []struct {
		name      string
		sessions  []ingest.DiscoveredSession
		wantCount int
	}{
		{
			name:      "nil input",
			sessions:  nil,
			wantCount: 0,
		},
		{
			name: "all roots",
			sessions: []ingest.DiscoveredSession{
				{SessionID: ingest.SessionID("11111111-1111-1111-1111-111111111111")},
				{SessionID: ingest.SessionID("22222222-2222-2222-2222-222222222222")},
			},
			wantCount: 2,
		},
		{
			name: "mixed roots and subagents",
			sessions: []ingest.DiscoveredSession{
				{SessionID: ingest.SessionID("11111111-1111-1111-1111-111111111111")},
				{SessionID: ingest.SessionID("33333333-3333-3333-3333-333333333333"), ParentUUID: &parentID},
				{SessionID: ingest.SessionID("22222222-2222-2222-2222-222222222222")},
			},
			wantCount: 2,
		},
		{
			name: "all subagents",
			sessions: []ingest.DiscoveredSession{
				{SessionID: ingest.SessionID("33333333-3333-3333-3333-333333333333"), ParentUUID: &parentID},
			},
			wantCount: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			roots := filterRootSessions(tt.sessions)
			if len(roots) != tt.wantCount {
				t.Errorf("filterRootSessions() returned %d roots, want %d", len(roots), tt.wantCount)
			}
			// Every returned session must have nil ParentUUID.
			for _, r := range roots {
				if r.ParentUUID != nil {
					t.Errorf("root session %s has non-nil ParentUUID", r.SessionID)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// aggregateProviderCounts — per-provider new/updated aggregation
// ---------------------------------------------------------------------------

func TestAggregateProviderCounts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		sessions []ingest.SessionResult
		want     []ftue.ProviderIngestCount
	}{
		{
			name:     "empty",
			sessions: nil,
			want:     nil,
		},
		{
			name: "mixed new and updated",
			sessions: []ingest.SessionResult{
				{Harness: defaults.HarnessClaudeCode, Status: ingest.DiffNew},
				{Harness: defaults.HarnessClaudeCode, Status: ingest.DiffUpdated},
				{Harness: defaults.HarnessOpenCode, Status: ingest.DiffNew},
			},
			want: []ftue.ProviderIngestCount{
				{Harness: schema.HarnessDisplayName(defaults.HarnessClaudeCode), New: 1, Updated: 1},
				{Harness: schema.HarnessDisplayName(defaults.HarnessOpenCode), New: 1},
			},
		},
		{
			name: "errored sessions skipped",
			sessions: []ingest.SessionResult{
				{Harness: defaults.HarnessClaudeCode, Status: ingest.DiffNew},
				{Harness: defaults.HarnessClaudeCode, Status: ingest.DiffNew, Error: fmt.Errorf("fail")},
			},
			want: []ftue.ProviderIngestCount{
				{Harness: schema.HarnessDisplayName(defaults.HarnessClaudeCode), New: 1},
			},
		},
		{
			name: "unchanged sessions excluded from output",
			sessions: []ingest.SessionResult{
				{Harness: defaults.HarnessClaudeCode, Status: ingest.DiffUnchanged},
			},
			want: nil,
		},
		{
			name: "single provider multiple new",
			sessions: []ingest.SessionResult{
				{Harness: defaults.HarnessClaudeCode, Status: ingest.DiffNew},
				{Harness: defaults.HarnessClaudeCode, Status: ingest.DiffNew},
				{Harness: defaults.HarnessClaudeCode, Status: ingest.DiffNew},
			},
			want: []ftue.ProviderIngestCount{
				{Harness: schema.HarnessDisplayName(defaults.HarnessClaudeCode), New: 3},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := aggregateProviderCounts(tt.sessions)
			if len(got) != len(tt.want) {
				t.Fatalf("got %d counts, want %d", len(got), len(tt.want))
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("count[%d] = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestClaudeSlugToProjectName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		slug string
		want string
	}{
		{
			name: "empty string",
			slug: "",
			want: "",
		},
		{
			name: "known dir prefix Users",
			slug: "-Users-alice-GitHub-my-repo",
			want: "my-repo",
		},
		{
			name: "known dir prefix home",
			slug: "-home-user-dev-project",
			want: "project",
		},
		{
			name: "no known dir returns original slug",
			slug: "-foo-bar-baz",
			want: "-foo-bar-baz",
		},
		{
			name: "multiple known dirs uses last",
			slug: "-home-user-dev-project-src-main",
			want: "main",
		},
		{
			name: "known dir is last segment returns original slug",
			slug: "-Users-alice-home",
			want: "-Users-alice-home",
		},
		{
			name: "remainder after known dir has dashes",
			slug: "-home-user-code-my-cool-app",
			want: "my-cool-app",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := claudeSlugToProjectName(tt.slug)
			if got != tt.want {
				t.Errorf("claudeSlugToProjectName(%q) = %q, want %q", tt.slug, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// buildChildMap — parent → child session ID mapping
// ---------------------------------------------------------------------------

func TestBuildChildMap(t *testing.T) {
	t.Parallel()
	parentA := ingest.SessionID("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	parentB := ingest.SessionID("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")

	tests := []struct {
		name     string
		sessions []ingest.DiscoveredSession
		wantMap  map[string][]string
	}{
		{
			name:     "nil input",
			sessions: nil,
			wantMap:  map[string][]string{},
		},
		{
			name: "all roots no children",
			sessions: []ingest.DiscoveredSession{
				{SessionID: parentA},
				{SessionID: parentB},
			},
			wantMap: map[string][]string{},
		},
		{
			name: "one parent two children",
			sessions: []ingest.DiscoveredSession{
				{SessionID: parentA},
				{SessionID: ingest.SessionID("cccccccc-cccc-cccc-cccc-cccccccccccc"), ParentUUID: &parentA},
				{SessionID: ingest.SessionID("dddddddd-dddd-dddd-dddd-dddddddddddd"), ParentUUID: &parentA},
			},
			wantMap: map[string][]string{
				string(parentA): {
					"cccccccc-cccc-cccc-cccc-cccccccccccc",
					"dddddddd-dddd-dddd-dddd-dddddddddddd",
				},
			},
		},
		{
			name: "two parents each with one child",
			sessions: []ingest.DiscoveredSession{
				{SessionID: parentA},
				{SessionID: parentB},
				{SessionID: ingest.SessionID("cccccccc-cccc-cccc-cccc-cccccccccccc"), ParentUUID: &parentA},
				{SessionID: ingest.SessionID("dddddddd-dddd-dddd-dddd-dddddddddddd"), ParentUUID: &parentB},
			},
			wantMap: map[string][]string{
				string(parentA): {"cccccccc-cccc-cccc-cccc-cccccccccccc"},
				string(parentB): {"dddddddd-dddd-dddd-dddd-dddddddddddd"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildChildMap(tt.sessions)
			if len(got) != len(tt.wantMap) {
				t.Fatalf("buildChildMap() returned %d entries, want %d", len(got), len(tt.wantMap))
			}
			for parentID, wantChildren := range tt.wantMap {
				gotChildren := got[parentID]
				if len(gotChildren) != len(wantChildren) {
					t.Errorf("parent %s: got %d children, want %d", parentID, len(gotChildren), len(wantChildren))
					continue
				}
				for i := range wantChildren {
					if gotChildren[i] != wantChildren[i] {
						t.Errorf("parent %s child[%d] = %q, want %q", parentID, i, gotChildren[i], wantChildren[i])
					}
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// expandAllowedSessionIDs — subagent inclusion in allowed set
// ---------------------------------------------------------------------------

func TestExpandAllowedSessionIDs(t *testing.T) {
	t.Parallel()
	parentID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	childID1 := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	childID2 := "cccccccc-cccc-cccc-cccc-cccccccccccc"
	noChildID := "dddddddd-dddd-dddd-dddd-dddddddddddd"

	tests := []struct {
		name     string
		selected []ftue.SessionListing
		wantNil  bool
		wantIDs  []string // expected IDs in the map
	}{
		{
			name:     "nil selected returns nil (allow all)",
			selected: nil,
			wantNil:  true,
		},
		{
			name:     "empty selected returns nil (allow all)",
			selected: []ftue.SessionListing{},
			wantNil:  true,
		},
		{
			name: "parent with children includes children",
			selected: []ftue.SessionListing{
				{
					SessionID:   parentID,
					SubagentIDs: []string{childID1, childID2},
				},
			},
			wantIDs: []string{parentID, childID1, childID2},
		},
		{
			name: "session without children only includes itself",
			selected: []ftue.SessionListing{
				{
					SessionID:   noChildID,
					SubagentIDs: nil,
				},
			},
			wantIDs: []string{noChildID},
		},
		{
			name: "mixed parents and childless sessions",
			selected: []ftue.SessionListing{
				{
					SessionID:   parentID,
					SubagentIDs: []string{childID1},
				},
				{
					SessionID:   noChildID,
					SubagentIDs: nil,
				},
			},
			wantIDs: []string{parentID, childID1, noChildID},
		},
		{
			name: "deselected parent excludes its children",
			// Only noChildID is selected; parentID is NOT in the list.
			selected: []ftue.SessionListing{
				{
					SessionID:   noChildID,
					SubagentIDs: nil,
				},
			},
			wantIDs: []string{noChildID},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := expandAllowedSessionIDs(tt.selected)
			if tt.wantNil {
				if got != nil {
					t.Fatalf("expandAllowedSessionIDs() = %v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatal("expandAllowedSessionIDs() = nil, want non-nil map")
			}
			if len(got) != len(tt.wantIDs) {
				t.Fatalf("expandAllowedSessionIDs() has %d entries, want %d", len(got), len(tt.wantIDs))
			}
			for _, id := range tt.wantIDs {
				sid, err := ingest.NewSessionID(id)
				if err != nil {
					t.Fatalf("invalid test session ID %q: %v", id, err)
				}
				if !got[sid] {
					t.Errorf("expected session ID %q in allowed set", id)
				}
			}
		})
	}
}
