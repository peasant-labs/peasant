// Package config parses and validates YAML configuration for the peasant ingest pipeline.
//
// If no config file exists, Load returns sensible defaults rather than creating a
// file automatically. Users should run `peasant kickstart` to generate an initial config.
//
// CLI note: --source-path replaces (not appends to) config paths. --force and
// --include-active are separate runtime flags handled in the CLI layer (S10); they
// are not stored in this config struct.
package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

// SaveAtomic validates and atomically replaces the configuration at path.
// The temporary file is created beside the destination so rename cannot cross
// filesystem boundaries. Callers must pass the exact path selected by the user.
func SaveAtomic(path string, cfg *Config) error {
	if path == "" {
		return fmt.Errorf("config save: destination path is empty while preparing an atomic configuration replacement; no file was changed; pass the resolved --config or --config-dir path and retry")
	}
	if cfg == nil {
		return fmt.Errorf("config save: configuration is nil while preparing %q; no file was changed; load or construct a valid configuration and retry", path)
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("config save: marshal configuration for %q before atomic replacement: %w; no file was changed; correct the unsupported configuration value and retry", path, err)
	}
	if _, err := Parse(data); err != nil {
		return fmt.Errorf("config save: validate serialized configuration for %q before atomic replacement: %w; no file was changed; correct the reported field and retry", path, err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, defaults.PublicDirPerm); err != nil {
		return fmt.Errorf("config save: create destination directory %q before writing %q: %w; no file was changed; fix directory ownership or permissions and retry", dir, path, err)
	}
	tmp, err := os.CreateTemp(dir, ".config-*.yaml.tmp")
	if err != nil {
		return fmt.Errorf("config save: create temporary file beside %q during atomic replacement: %w; no file was changed; fix directory ownership or free space and retry", path, err)
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		_ = tmp.Close()
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()

	if err := tmp.Chmod(defaults.PublicFilePerm); err != nil {
		return fmt.Errorf("config save: set permissions on temporary file %q for %q: %w; the destination was not changed; fix filesystem permission support and retry", tmpPath, path, err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("config save: write temporary file %q for %q: %w; the destination was not changed; free disk space or repair the filesystem and retry", tmpPath, path, err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("config save: sync temporary file %q for %q before replacement: %w; the destination was not changed; repair the filesystem and retry", tmpPath, path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("config save: close temporary file %q for %q before replacement: %w; the destination was not changed; repair the filesystem and retry", tmpPath, path, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("config save: atomically replace %q from temporary file %q: %w; the previous destination remains available; fix destination permissions and retry", path, tmpPath, err)
	}
	committed = true

	if parent, err := os.Open(dir); err == nil {
		if syncErr := parent.Sync(); syncErr != nil {
			_ = parent.Close()
			return fmt.Errorf("config save: sync destination directory %q after replacing %q: %w; the new file is present but crash durability is not confirmed; verify the file and retry the save", dir, path, syncErr)
		}
		if closeErr := parent.Close(); closeErr != nil {
			return fmt.Errorf("config save: close destination directory %q after replacing %q: %w; the new file is present; verify it before continuing", dir, path, closeErr)
		}
	}
	return nil
}

// SelectionMatcher compiles the persisted selection through the canonical
// matcher used by ingest, push, discovery lists, and prune.
func (c *Config) SelectionMatcher() ingest.SelectionMatcher {
	return CompileSelectionMatcher(c.Selection)
}

// CompileSelectionMatcher compiles a selection without requiring a full Config.
func CompileSelectionMatcher(selection SelectionConfig) ingest.SelectionMatcher {
	b := ingest.NewSelectionMatcherBuilder()
	for harness, selected := range selection.Harnesses {
		b.AddHarness(harness)
		for _, project := range selected.Projects {
			b.AddProjectWithClonePaths(harness, project.GitRemote, project.Name, project.ClonePaths, project.Branches...)
		}
		for _, sessionID := range selected.Sessions {
			b.AddSession(harness, sessionID)
		}
		if selection.Mode == SelectionModeSelected {
			for _, excluded := range selected.Exclusions.Branches {
				b.AddBranchExclusion(harness, excluded.ClonePath, excluded.Branches...)
			}
			for _, sessionID := range selected.Exclusions.Sessions {
				b.AddSessionExclusion(harness, sessionID)
			}
		}
	}
	return b.Build()
}

// MockDataStoreOptions holds the parsed components and sections to mock.
type MockDataStoreOptions struct {
	Components map[defaults.MockComponent]bool
	Sections   map[defaults.MockSection]bool
	// Disable is set when the caller explicitly passed "none", meaning mock
	// mode should be turned off entirely regardless of config.yaml.
	Disable bool
}

func (o *MockDataStoreOptions) HasComponent(c defaults.MockComponent) bool {
	if o == nil {
		return false
	}
	return o.Components[c]
}

func (o *MockDataStoreOptions) HasSection(s defaults.MockSection) bool {
	if o == nil {
		return false
	}
	return o.Sections[s]
}

func (o *MockDataStoreOptions) String() string {
	if o == nil {
		return ""
	}
	var parts []string
	for c := range o.Components {
		parts = append(parts, string(c))
	}
	for s := range o.Sections {
		parts = append(parts, string(s))
	}
	return strings.Join(parts, ",")
}

// ApplyMockOverrides updates the mock config based on CLI flags.
// CLI flags REPLACE configured sections for the component.
// Passing opts.Disable = true (from --mock-data-store=none) turns off mock mode entirely.
func (cfg *Config) ApplyMockOverrides(opts *MockDataStoreOptions, component defaults.MockComponent) {
	if opts == nil {
		return
	}
	// Explicit disable: --mock-data-store=none turns off mock mode regardless of config.yaml.
	if opts.Disable {
		cfg.Sources.Mock.Enabled = false
		return
	}
	// Any non-disable flag enables mock mode globally.
	cfg.Sources.Mock.Enabled = true

	// Case 1: Component explicitly mentioned (e.g., --mock-data-store=web).
	// This means ALL sections for this component are mocked.
	if opts.HasComponent(component) {
		switch component {
		case defaults.MockComponents.Web:
			cfg.Sources.Mock.Web = nil
		case defaults.MockComponents.TUI:
			cfg.Sources.Mock.TUI = nil
		case defaults.MockComponents.API:
			cfg.Sources.Mock.API = nil
		}
		return
	}

	// Case 2: Specific sections mentioned (e.g., --mock-data-store=dashboard,sessions).
	// These REPLACE the configured sections for THIS component.
	if len(opts.Sections) > 0 {
		sections := make([]defaults.MockSection, 0, len(opts.Sections))
		for s := range opts.Sections {
			sections = append(sections, s)
		}
		switch component {
		case defaults.MockComponents.Web:
			cfg.Sources.Mock.Web = sections
		case defaults.MockComponents.TUI:
			cfg.Sources.Mock.TUI = sections
		case defaults.MockComponents.API:
			cfg.Sources.Mock.API = sections
		}
	}
}

// PushMethod controls which sessions peasant push uploads.
type PushMethod string

const (
	// PushMethodAll uploads all unpushed sessions.
	PushMethodAll PushMethod = "all"
	// PushMethodBySource uploads sessions filtered to configured source providers.
	PushMethodBySource PushMethod = "by-source"
	// PushMethodIndividual requires interactive session selection (not yet implemented).
	PushMethodIndividual PushMethod = "individual"
)

// String implements fmt.Stringer.
func (m PushMethod) String() string { return string(m) }

// IsValid reports whether the PushMethod is a known value.
func (m PushMethod) IsValid() bool {
	switch m {
	case PushMethodAll, PushMethodBySource, PushMethodIndividual:
		return true
	}
	return false
}

// Visibility controls the default visibility for pushed transcripts.
// Type alias for schema.Visibility — single source of truth in the
// github.com/peasant-labs/schema module.
type Visibility = schema.Visibility

const (
	// VisibilityPublic makes pushed transcripts visible to anyone.
	VisibilityPublic = schema.VisibilityPublic
	// VisibilityPrivate makes pushed transcripts visible only to the owner.
	VisibilityPrivate = schema.VisibilityPrivate
	// VisibilityGroup makes pushed transcripts visible to group members.
	VisibilityGroup = schema.VisibilityGroup
	// VisibilityShared is a deprecated alias for VisibilityGroup.
	// Deprecated: Use VisibilityGroup instead.
	VisibilityShared = schema.VisibilityGroup
)

// License is the default content license applied to pushed transcripts.
// Type alias for schema.License — single source of truth in the
// github.com/peasant-labs/schema module.
type License = schema.License

const (
	// LicenseCC0 dedicates the transcript to the public domain (CC0-1.0).
	LicenseCC0 = schema.LicenseCC0
	// LicenseCCBY licenses under Creative Commons Attribution (CC-BY-4.0).
	LicenseCCBY = schema.LicenseCCBY
	// LicenseCCBYSA licenses under CC Attribution-ShareAlike (CC-BY-SA-4.0).
	LicenseCCBYSA = schema.LicenseCCBYSA
)

// PushFieldVisibility controls which RAW-identity metadata fields are included in
// either part of a multipart publication. Fields set to false are omitted
// (zero-valued) from both the authoritative request and transcript content. All
// such fields default to false (user must opt-in to share).
//
// NOTE: project.hash is NOT gated here. It is a per-installation salted
// HMAC-SHA256 (non-correlatable across users) and is ALWAYS sent, so the village
// can group a single user's sessions by project without recovering any
// plaintext. The deprecated `projectHash` key is parsed (ProjectHashLegacy) only
// to emit a one-line deprecation note; it never gates anything.
type PushFieldVisibility struct {
	// GitRemote controls whether git remote URL is included. Default: false — leaks git identity.
	GitRemote bool `yaml:"gitRemote"`
	// GitBranch controls whether branch name is included. Default: false — may contain feature descriptions.
	GitBranch bool `yaml:"gitBranch"`
	// ProjectPath controls whether local project path is included. Default: false — leaks directory structure.
	ProjectPath bool `yaml:"projectPath"`
	// HostSlug controls whether host slug is included. Default: false — for tracked projects, IS the git remote in slug form.
	HostSlug bool `yaml:"hostSlug"`
	// ProjectName controls whether the project name is included. Default: false — may leak
	// project/directory names that reveal local directory structure or work context.
	ProjectName bool `yaml:"projectName"`
	// ProjectHashLegacy is a PARSE-ONLY presence flag for the deprecated
	// `push.fields.projectHash` key. nil = key absent. It NEVER gates the hash
	// (project.hash is always sent); it exists solely so the push path can emit a
	// one-line stderr deprecation note when a stale config still carries the key.
	// yaml.Unmarshal is non-strict, so an absent key leaves this nil.
	ProjectHashLegacy *bool `yaml:"projectHash"`
}

// HasDeprecatedProjectHashKey reports whether the loaded config carried the
// deprecated `push.fields.projectHash` key. Used by the push path to emit a
// one-line deprecation note. It never affects what is sent.
func (f PushFieldVisibility) HasDeprecatedProjectHashKey() bool {
	return f.ProjectHashLegacy != nil
}

// DefaultPushFieldVisibility returns safe defaults where all raw-identity fields are omitted.
func DefaultPushFieldVisibility() PushFieldVisibility {
	return PushFieldVisibility{
		GitRemote:   false,
		GitBranch:   false,
		ProjectPath: false,
		HostSlug:    false,
		ProjectName: false,
	}
}

// PushConfig controls the peasant push command behavior.
type PushConfig struct {
	// Method controls which sessions are selected for upload.
	Method PushMethod `yaml:"method"`
	// Sources lists the provider names used when Method is PushMethodBySource.
	Sources []string `yaml:"sources,omitempty"`
	// Visibility is the default visibility for pushed transcripts.
	Visibility Visibility `yaml:"visibility"`
	// License is the default content license applied to all pushed transcripts
	// (chosen during kickstart). Empty ⇒ no license is sent ⇒ the village stores
	// NULL. Overridable per-run with the --license flag.
	License License `yaml:"license,omitempty"`
	// Fields controls which metadata fields are included in push payloads.
	Fields PushFieldVisibility `yaml:"fields"`
	// Concurrency is the default number of parallel uploads (and the HTTP
	// connection-pool size). 0 (or omitted) means "use the CPU-derived default",
	// max(1, NumCPU/2). The --concurrency CLI flag overrides this value.
	Concurrency int `yaml:"concurrency,omitempty"`
}

// DaemonConfig controls the background daemon behavior.
type DaemonConfig struct {
	// ProjectMode is "opt-in" or "opt-out" for project-level tracking.
	ProjectMode string `yaml:"projectMode"`
}

// Theme selects which terminal color mode the TUI renders in. Its two values
// share their string representation with internal/tui/theme.Mode
// ("dark"/"light"); theme.ModeFromConfig converts a validated Theme's string
// into a theme.Mode at TUI mount.
type Theme string

const (
	// ThemeDark selects dark terminal colors. This is the default.
	ThemeDark Theme = "dark"
	// ThemeLight selects light terminal colors.
	ThemeLight Theme = "light"
)

// String implements fmt.Stringer.
func (t Theme) String() string { return string(t) }

// IsValid reports whether the Theme is a known value.
func (t Theme) IsValid() bool {
	switch t {
	case ThemeDark, ThemeLight:
		return true
	}
	return false
}

// DisplayConfig controls terminal rendering preferences for the TUI.
type DisplayConfig struct {
	// Theme selects dark or light terminal colors. Empty is accepted by
	// validate() the same way Selection.Mode's empty is - BaseConfig already
	// sets ThemeDark, so an explicit config.yaml only needs to name this key
	// to override it, never to satisfy it.
	Theme Theme `yaml:"theme"`
}

// SelectionMode controls how the ingest pipeline selects sessions.
type SelectionMode string

const (
	// SelectionModeAll ingests everything from enabled providers (no filter).
	SelectionModeAll SelectionMode = "all"
	// SelectionModeSelected ingests only projects/branches/sessions in the allowlist.
	SelectionModeSelected SelectionMode = "selected"
)

// String implements fmt.Stringer.
func (m SelectionMode) String() string { return string(m) }

// IsValid reports whether the SelectionMode is a known value.
func (m SelectionMode) IsValid() bool {
	switch m {
	case SelectionModeAll, SelectionModeSelected:
		return true
	}
	return false
}

// ProjectSelection identifies a project by git remote (or project name fallback)
// and optionally restricts ingestion to specific branches.
type ProjectSelection struct {
	// GitRemote is the primary project identifier (e.g., "git@github.com:user/repo.git").
	GitRemote string `yaml:"gitRemote,omitempty"`
	// Name is a fallback identifier when no git remote is available (e.g., local project name).
	// Name-only matching uses the folder basename. ClonePaths supplies exact
	// physical identity when differently located projects share that basename.
	Name string `yaml:"name,omitempty"`
	// ClonePaths lists the resolved absolute physical paths for the local clones
	// selected by this entry. One Git remote can have more than one clone path.
	// Branches applies to every clone path in the entry.
	ClonePaths []string `yaml:"clonePaths,omitempty"`
	// Branches restricts ingestion to specific branches within this project.
	// Empty means all branches are included.
	Branches []string `yaml:"branches,omitempty"`
}

// BranchExclusion identifies branches denied for one exact physical clone.
type BranchExclusion struct {
	// ClonePath is the resolved absolute physical path for the local clone.
	ClonePath string `yaml:"clonePath"`
	// Branches lists exact branch names denied for this clone.
	Branches []string `yaml:"branches"`
}

// SelectionExclusions holds exact denials for one harness.
type SelectionExclusions struct {
	// Branches denies branches only for their exact physical clone path.
	Branches []BranchExclusion `yaml:"branches,omitempty"`
	// Sessions denies exact session IDs for this harness.
	Sessions []string `yaml:"sessions,omitempty"`
}

// SelectionHarnessConfig holds positive selection and exact exclusions for one harness.
type SelectionHarnessConfig struct {
	// Projects lists the allowed projects (by git remote or name).
	Projects []ProjectSelection `yaml:"projects,omitempty"`
	// Sessions lists explicitly allowed session IDs (for session-level picks
	// that don't fit into project grouping).
	Sessions []string `yaml:"sessions,omitempty"`
	// Exclusions lists exact branch and session denials applied after positive
	// selection. Exclusions are valid only in selected mode.
	Exclusions SelectionExclusions `yaml:"exclusions,omitempty"`
}

// SelectionConfig controls which projects, branches, and sessions are ingested.
// Persisted from kickstart wizard selections and used by peasant ingest to filter.
type SelectionConfig struct {
	// Mode controls whether the selection filter is active.
	// "all" means ingest everything; "selected" means apply the allowlist.
	Mode SelectionMode `yaml:"mode"`
	// AutoIngestNewBranches controls whether new branches in fully-selected
	// projects are automatically included in subsequent ingestions.
	AutoIngestNewBranches bool `yaml:"autoIngestNewBranches"`
	// Harnesses maps harness name to its selection allowlist.
	Harnesses map[string]SelectionHarnessConfig `yaml:"harnesses,omitempty"`

	// DeprecatedProviders captures the legacy "selection.providers:" key (renamed to
	// "harnesses" in the bestiary harness migration) so validate() can reject it with
	// remediation rather than silently ignoring it.
	// TEMPORARY (bestiary migration): remove this field and its validate() check at the
	// private->public launch, once no pre-rename installs remain — same lifecycle as
	// Sources.DeprecatedClaude and defaults.LegacyHarness*.
	DeprecatedProviders map[string]SelectionHarnessConfig `yaml:"providers,omitempty"`
}

// Config holds the complete configuration for the peasant ingest pipeline.
type Config struct {
	Version   int             `yaml:"version"`
	User      UserConfig      `yaml:"user"`
	Redaction RedactionConfig `yaml:"redaction"`
	Sources   SourcesConfig   `yaml:"sources"`
	Output    OutputConfig    `yaml:"output"`
	Village   VillageConfig   `yaml:"village"`
	Daemon    DaemonConfig    `yaml:"daemon"`
	Push      PushConfig      `yaml:"push"`
	Selection SelectionConfig `yaml:"selection"`
	Display   DisplayConfig   `yaml:"display"`

	// ClaudeRetentionDays is the Claude Code cleanupPeriodDays value the
	// onboarding flow lets the user choose. It is NOT a peasant setting: it is
	// written to ~/.claude/settings.json by the retention writer, never to
	// config.yaml. The yaml:"-" tag keeps it out of the persisted file while
	// still letting the settings flow carry the choice through its draft to the
	// commit, after which the program hands it to the existing retention writer.
	ClaudeRetentionDays int `yaml:"-"`
}

// VillageConfig holds the village server connection settings.
type VillageConfig struct {
	URL       string `yaml:"url"`
	Connected bool   `yaml:"connected"`
}

// UserConfig identifies the user running the ingest pipeline.
type UserConfig struct {
	Email string `yaml:"email"`
}

// PatternCategory classifies what kind of sensitive data a custom pattern targets.
// The valid values mirror redact.Category but are defined here for config-layer clarity
// and to allow YAML unmarshalling without pulling redact types into YAML tags.
type PatternCategory string

const (
	// CategorySecrets targets API keys, tokens, credentials, and other secret values.
	CategorySecrets PatternCategory = "secrets"
	// CategoryPII targets personally identifiable information (email, phone, SSN, etc.).
	CategoryPII PatternCategory = "pii"
	// CategoryPaths targets filesystem paths that may expose usernames.
	CategoryPaths PatternCategory = "paths"
	// CategoryProject targets project identity such as repository aliases.
	CategoryProject PatternCategory = "project"
)

// String implements fmt.Stringer.
func (p PatternCategory) String() string { return string(p) }

// CustomPattern is a user-defined regex redaction rule that can be specified in config.yaml.
// Each pattern is compiled and validated at config load time (fail-closed).
type CustomPattern struct {
	ID          string          `yaml:"id"`
	Category    PatternCategory `yaml:"category"`
	Pattern     string          `yaml:"pattern"`
	Replacement string          `yaml:"replacement"`
}

// RedactionConfig controls how sensitive data is redacted from transcripts.
type RedactionConfig struct {
	Level          redact.RedactionLevel `yaml:"level"`
	CustomPatterns []CustomPattern       `yaml:"custom_patterns,omitempty"`
}

// SourcesConfig defines which conversation harnesses are enabled and where their
// data lives on disk. YAML keys use canonical bestiary.Harness identifiers.
type SourcesConfig struct {
	ClaudeCode SourceProviderConfig `yaml:"claude-code"`
	OpenCode   SourceProviderConfig `yaml:"opencode"`
	Codex      SourceProviderConfig `yaml:"codex"`
	Cursor     SourceProviderConfig `yaml:"cursor"`
	Strike     SourceProviderConfig `yaml:"strike"`

	// deprecatedClaude captures the legacy "claude:" key for error reporting.
	// Parsing errors out if this is set; the field exists only so the YAML
	// decoder will populate it (rather than silently rejecting the key).
	DeprecatedClaude SourceProviderConfig `yaml:"claude"`

	Mock MockConfig `yaml:"mock"`
}

// Provider returns the mutable source configuration for an ingestion harness.
func (s *SourcesConfig) Provider(harness defaults.Harness) (*SourceProviderConfig, bool) {
	switch harness {
	case defaults.HarnessClaudeCode:
		return &s.ClaudeCode, true
	case defaults.HarnessOpenCode:
		return &s.OpenCode, true
	case defaults.HarnessCodex:
		return &s.Codex, true
	case defaults.HarnessCursor:
		return &s.Cursor, true
	case defaults.HarnessStrike:
		return &s.Strike, true
	default:
		return nil, false
	}
}

// MockConfig controls mock data provider behavior for development/testing.
// Each component field lists sections that should use mock data.
// Example YAML:
//
//	mock:
//	  enabled: true
//	  web: [dashboard, sessions, trends]
//	  tui: [sessions]
//	  api: [sessions]
type MockConfig struct {
	Enabled defaults.MockEnabled   `yaml:"enabled"`
	Web     []defaults.MockSection `yaml:"web,omitempty"`
	TUI     []defaults.MockSection `yaml:"tui,omitempty"`
	API     []defaults.MockSection `yaml:"api,omitempty"`
}

// SourceProviderConfig holds the toggle and raw disk paths for one provider.
// Paths are raw (may contain tildes); callers must expand them via
// ingest.NewResolvedPath before use.
type SourceProviderConfig struct {
	Enabled bool     `yaml:"enabled"`
	Paths   []string `yaml:"paths"`
}

// OutputConfig controls where ingested data is written and when existing output
// is considered stale.
type OutputConfig struct {
	// BasePath is the raw output directory path (may contain tildes).
	BasePath string `yaml:"basePath"`
	// StalenessThresholdSec is the number of seconds after which output is
	// considered stale and eligible for re-ingestion. Defaults to 60.
	StalenessThresholdSec int `yaml:"stalenessThresholdSec"`
}

// BaseConfig returns a Config populated with default values from the defaults
// package. Used by LoadDefaults, Parse, and ftue.Save to eliminate duplication.
func BaseConfig() *Config {
	return &Config{
		Version:   defaults.ConfigVersion,
		Redaction: RedactionConfig{Level: redact.Standard},
		Sources: SourcesConfig{
			ClaudeCode: SourceProviderConfig{
				Enabled: true,
				Paths:   []string{defaults.DefaultClaudePath.String()},
			},
			OpenCode: SourceProviderConfig{
				Enabled: true,
				Paths:   []string{defaults.DefaultOpenCodePath.String()},
			},
			Codex: SourceProviderConfig{
				Enabled: true,
				Paths:   []string{defaults.DefaultCodexPath.String()},
			},
			Cursor: SourceProviderConfig{
				Enabled: true,
				Paths:   []string{defaults.DefaultCursorPath.String()},
			},
			Strike: SourceProviderConfig{
				Enabled: false,
				Paths:   []string{defaults.DefaultStrikePath.String()},
			},
			Mock: MockConfig{
				Enabled: defaults.DefaultMockEnabled,
				Web:     defaults.DefaultMockWebSections,
				TUI:     defaults.DefaultMockTUISections,
				API:     defaults.DefaultMockAPISections,
			},
		},
		Output: OutputConfig{
			BasePath:              defaults.ResolveOutputBasePath().String(),
			StalenessThresholdSec: defaults.ConfigStalenessThresholdSec,
		},
		Village: VillageConfig{
			URL: defaults.DefaultVillageURL.String(),
		},
		Daemon: DaemonConfig{
			ProjectMode: "opt-in",
		},
		Push: PushConfig{
			Method:     PushMethodAll,
			Visibility: VisibilityPrivate,
			Fields:     DefaultPushFieldVisibility(),
		},
		Selection: SelectionConfig{
			Mode:                  SelectionModeAll,
			AutoIngestNewBranches: true,
		},
		Display: DisplayConfig{
			Theme: ThemeDark,
		},
	}
}

// Load reads the YAML config file at path, validates it, and returns the result.
//
// If path is empty or the file does not exist, Load returns LoadDefaults without
// an error. If path does not exist but a sibling config.toml exists, Load
// migrates the TOML to YAML automatically before reading.
//
// Users who want a config file should run `peasant kickstart`.
func Load(path string, fs ingest.FileSystem, git ingest.GitResolver) (*Config, error) {
	if path == "" {
		return LoadDefaults(context.Background(), git), nil
	}

	data, err := fs.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Check for a config.toml sibling and migrate if present.
			// Use fs.Stat (not os.Stat) so this works with MemFS in tests.
			ext := filepath.Ext(path)
			tomlPath := strings.TrimSuffix(path, ext) + ".toml"
			if _, statErr := fs.Stat(tomlPath); statErr == nil {
				if migErr := MigrateTOML(tomlPath, path); migErr != nil {
					return nil, fmt.Errorf("config: migrate TOML: %w", migErr)
				}
				// Re-read the now-created YAML file.
				data, err = fs.ReadFile(path)
				if err != nil {
					return nil, fmt.Errorf("config: read migrated %q: %w", path, err)
				}
				return Parse(data)
			}
			return LoadDefaults(context.Background(), git), nil
		}
		return nil, fmt.Errorf("config: read %q: %w", path, err)
	}

	return Parse(data)
}

// LoadDefaults returns a Config populated with sensible production defaults.
// The user.email field is auto-detected from git config --global user.email when
// git is non-nil; if the lookup fails the field is left empty.
func LoadDefaults(ctx context.Context, git ingest.GitResolver) *Config {
	cfg := BaseConfig()

	email := ""
	if git != nil {
		if e, err := git.UserEmail(ctx); err == nil {
			email = e
		}
	}
	cfg.User = UserConfig{Email: email}

	return cfg
}

// Parse decodes raw YAML bytes into a Config, applies missing-field defaults, and
// validates the result. Returns an error if any field fails validation.
func Parse(data []byte) (*Config, error) {
	cfg := BaseConfig()

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("config: parse YAML: %w", err)
	}

	if err := validate(cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}

// CustomPatternsToUserPatterns converts []CustomPattern from config into []redact.UserPattern
// suitable for passing to redact.NewRedactor. Each pattern is validated:
//   - ID must be non-empty
//   - Category must be a known redact.Category value (cast + IsValid check)
//   - Pattern must be a valid regular expression
//
// Returns an error on the first invalid pattern (fail-closed). Safe to call from cmd/peasant.
func CustomPatternsToUserPatterns(patterns []CustomPattern) ([]redact.UserPattern, error) {
	if len(patterns) == 0 {
		return nil, nil
	}
	out := make([]redact.UserPattern, 0, len(patterns))
	for _, p := range patterns {
		if p.ID == "" {
			return nil, fmt.Errorf("config: custom pattern has empty id")
		}
		cat := redact.Category(p.Category)
		if !cat.IsValid() {
			return nil, fmt.Errorf("config: custom pattern %q: unknown category %q", p.ID, p.Category)
		}
		if _, err := regexp.Compile(p.Pattern); err != nil {
			return nil, fmt.Errorf("config: custom pattern %q: invalid regex: %w", p.ID, err)
		}
		out = append(out, redact.UserPattern{
			ID:          p.ID,
			Category:    cat,
			Pattern:     p.Pattern,
			Replacement: p.Replacement,
		})
	}
	return out, nil
}

// validate checks all semantic constraints on a parsed Config.
func validate(cfg *Config) error {
	if cfg.Version != defaults.ConfigVersion {
		return fmt.Errorf("config: unsupported config version %d (only version %d is supported)", cfg.Version, defaults.ConfigVersion)
	}

	// Parsing and OFFERING are separate steps on purpose. An unrecognised value is
	// a typo and fails here. A recognised value this version no longer offers -
	// today, minimal - still parses, because an existing configuration carrying it
	// must keep working: ResolveRedactionPolicy raises it and discloses the change.
	// Refusing it here would dead-end its owner at load time, before any surface
	// could explain the remedy.
	if !cfg.Redaction.Level.IsValid() {
		return fmt.Errorf("config: unknown redaction level %q (the level this version offers is %s)",
			cfg.Redaction.Level, RedactionLevelMenu())
	}

	if cfg.Output.StalenessThresholdSec < 0 {
		return fmt.Errorf("config: stalenessThresholdSec must be >= 0, got %d", cfg.Output.StalenessThresholdSec)
	}

	// The legacy "claude:" key is no longer accepted. If the user's config still
	// uses it, error out with clear remediation rather than silently mapping it.
	if cfg.Sources.DeprecatedClaude.Enabled || len(cfg.Sources.DeprecatedClaude.Paths) > 0 {
		return fmt.Errorf(
			"config: sources.claude is no longer accepted (the harness was renamed to %q).\n"+
				"  What this means: peasant will not ingest any Claude Code sessions until the config is updated.\n"+
				"  How to fix: in your config.yaml, rename the YAML key from `claude:` to `claude-code:`.\n"+
				"  Example:\n"+
				"    sources:\n"+
				"      claude-code:    # was 'claude'\n"+
				"        enabled: true\n"+
				"        paths: [~/.claude/projects]",
			defaults.HarnessClaudeCode,
		)
	}

	if cfg.Sources.ClaudeCode.Enabled && len(cfg.Sources.ClaudeCode.Paths) == 0 {
		return fmt.Errorf("config: enabled source %q must have at least one path", defaults.HarnessClaudeCode)
	}

	if cfg.Sources.OpenCode.Enabled && len(cfg.Sources.OpenCode.Paths) == 0 {
		return fmt.Errorf("config: enabled source %q must have at least one path", defaults.HarnessOpenCode)
	}

	if cfg.Sources.Codex.Enabled && len(cfg.Sources.Codex.Paths) == 0 {
		return fmt.Errorf("config: enabled source %q must have at least one path", defaults.HarnessCodex)
	}

	if cfg.Sources.Cursor.Enabled && len(cfg.Sources.Cursor.Paths) == 0 {
		return fmt.Errorf("config: enabled source %q must have at least one path", defaults.HarnessCursor)
	}

	if cfg.Sources.Strike.Enabled && len(cfg.Sources.Strike.Paths) == 0 {
		return fmt.Errorf("config: enabled source %q must have at least one path", defaults.HarnessStrike)
	}

	// Validate mock section values when mock is enabled.
	if cfg.Sources.Mock.Enabled {
		for _, s := range cfg.Sources.Mock.Web {
			if !defaults.IsValidMockSection(s) {
				return fmt.Errorf("config: unknown mock.web section %q (valid: %v)", s, defaults.AllMockSections)
			}
		}
		for _, s := range cfg.Sources.Mock.TUI {
			if !defaults.IsValidMockSection(s) {
				return fmt.Errorf("config: unknown mock.tui section %q (valid: %v)", s, defaults.AllMockSections)
			}
		}
		for _, s := range cfg.Sources.Mock.API {
			if !defaults.IsValidMockSection(s) {
				return fmt.Errorf("config: unknown mock.api section %q (valid: %v)", s, defaults.AllMockSections)
			}
		}
	}

	// Validate custom patterns at load time (fail-closed).
	// Check for duplicate pattern IDs.
	seen := make(map[string]bool, len(cfg.Redaction.CustomPatterns))
	for _, p := range cfg.Redaction.CustomPatterns {
		if seen[p.ID] {
			return fmt.Errorf("config: duplicate custom pattern id %q", p.ID)
		}
		seen[p.ID] = true
	}
	if _, err := CustomPatternsToUserPatterns(cfg.Redaction.CustomPatterns); err != nil {
		return err
	}

	// Validate push config.
	if cfg.Push.Method != "" && !cfg.Push.Method.IsValid() {
		return fmt.Errorf("config: unknown push.method %q (valid: all, by-source, individual)", cfg.Push.Method)
	}
	// "shared" is no longer accepted — require users to update their config explicitly.
	if cfg.Push.Visibility == "shared" {
		return fmt.Errorf("config: push.visibility %q is no longer valid; use %q instead", "shared", "group")
	}
	if cfg.Push.Visibility != "" && !cfg.Push.Visibility.IsValid() {
		return fmt.Errorf("config: unknown push.visibility %q (valid: public, private, group)", cfg.Push.Visibility)
	}
	if cfg.Push.License != "" && !cfg.Push.License.IsValid() {
		return fmt.Errorf("config: unknown push.license %q (valid: %s)", cfg.Push.License, schema.LicenseMenu())
	}
	if cfg.Push.Method == PushMethodBySource && len(cfg.Push.Sources) == 0 {
		return fmt.Errorf("config: push.method is %q but push.sources is empty", PushMethodBySource)
	}
	if cfg.Village.Connected && cfg.Village.URL == "" {
		return fmt.Errorf("config: village.connected is true but village.url is empty")
	}

	// Validate selection config.
	if cfg.Selection.Mode != "" && !cfg.Selection.Mode.IsValid() {
		return fmt.Errorf("config: unknown selection.mode %q (valid: all, selected)", cfg.Selection.Mode)
	}
	// The legacy "selection.providers:" key was renamed to "selection.harnesses:" in
	// the bestiary harness migration. Reject it with remediation rather than silently
	// ignoring it (consistent with the sources.claude rejection above).
	// TEMPORARY (bestiary migration): remove this check at the private->public launch.
	if len(cfg.Selection.DeprecatedProviders) > 0 {
		return fmt.Errorf(
			"config: selection.providers is no longer accepted (renamed to selection.harnesses).\n" +
				"  What this means: peasant will not apply your selection allowlist until the config is updated.\n" +
				"  How to fix: in your config.yaml, rename the YAML key `providers:` under `selection:` to `harnesses:`.\n" +
				"  Example:\n" +
				"    selection:\n" +
				"      harnesses:    # was 'providers'\n" +
				"        claude-code:\n" +
				"          projects: [...]")
	}
	if err := validateSelectionExclusions(cfg.Selection); err != nil {
		return err
	}

	// Validate display config.
	if cfg.Display.Theme != "" && !cfg.Display.Theme.IsValid() {
		return fmt.Errorf("config: unknown display.theme %q (valid: dark, light)", cfg.Display.Theme)
	}
	return nil
}

func validateSelectionExclusions(selection SelectionConfig) error {
	harnessNames := make([]string, 0, len(selection.Harnesses))
	for harness := range selection.Harnesses {
		harnessNames = append(harnessNames, harness)
	}
	sort.Strings(harnessNames)

	for _, harness := range harnessNames {
		exclusions := selection.Harnesses[harness].Exclusions
		if len(exclusions.Branches) == 0 && len(exclusions.Sessions) == 0 {
			continue
		}
		harnessPath := fmt.Sprintf("selection.harnesses[%q]", harness)
		if selection.Mode != SelectionModeSelected {
			return newSelectionExclusionValidationError(
				harnessPath+".exclusions",
				"exact exclusions cannot be used",
				fmt.Sprintf("selection.mode is %q instead of %q", selection.Mode, SelectionModeSelected),
				"set selection.mode to selected or remove the exclusions",
				nil,
			)
		}
		if strings.TrimSpace(harness) == "" {
			return newSelectionExclusionValidationError(
				harnessPath,
				"the harness name is empty",
				"an exact exclusion needs a harness identity",
				"move the exclusions under a supported non-empty harness key",
				nil,
			)
		}
		if !ingest.Harness(harness).IsKnown() {
			return newSelectionExclusionValidationError(
				harnessPath,
				fmt.Sprintf("harness %q is not supported", harness),
				"an exact exclusion can only match a known ingestion harness",
				"use a supported harness key or remove the exclusions",
				nil,
			)
		}

		seenSessions := make(map[string]int, len(exclusions.Sessions))
		for index, rawSessionID := range exclusions.Sessions {
			fieldPath := fmt.Sprintf("%s.exclusions.sessions[%d]", harnessPath, index)
			if rawSessionID == "" {
				return newSelectionExclusionValidationError(
					fieldPath,
					"the session ID is empty",
					"empty text cannot identify one stored session",
					"set a valid session UUID or remove this entry",
					nil,
				)
			}
			if strings.TrimSpace(rawSessionID) != rawSessionID {
				return newSelectionExclusionValidationError(
					fieldPath,
					fmt.Sprintf("session ID %q is not normalized", rawSessionID),
					"leading or trailing whitespace prevents an exact session match",
					"remove the surrounding whitespace and retry",
					nil,
				)
			}
			if _, err := ingest.NewSessionID(rawSessionID); err != nil {
				return newSelectionExclusionValidationError(
					fieldPath,
					fmt.Sprintf("session ID %q is malformed", rawSessionID),
					"the value is not a valid session identifier",
					"copy the exact session UUID from Peasant or remove this entry",
					err,
				)
			}
			if firstIndex, duplicate := seenSessions[rawSessionID]; duplicate {
				return newSelectionExclusionValidationError(
					fieldPath,
					fmt.Sprintf("the session ID duplicates sessions[%d]", firstIndex),
					"one exact session denial must appear only once per harness",
					"remove the duplicate session entry",
					nil,
				)
			}
			seenSessions[rawSessionID] = index
		}

		seenClonePaths := make(map[string]int, len(exclusions.Branches))
		for exclusionIndex, exclusion := range exclusions.Branches {
			rowPath := fmt.Sprintf("%s.exclusions.branches[%d]", harnessPath, exclusionIndex)
			clonePathField := rowPath + ".clonePath"
			switch {
			case exclusion.ClonePath == "":
				return newSelectionExclusionValidationError(
					clonePathField,
					"the clone path is empty",
					"a branch denial needs one exact physical clone identity",
					"set the resolved absolute clone path or remove this entry",
					nil,
				)
			case strings.TrimSpace(exclusion.ClonePath) != exclusion.ClonePath:
				return newSelectionExclusionValidationError(
					clonePathField,
					fmt.Sprintf("clone path %q is not normalized", exclusion.ClonePath),
					"leading or trailing whitespace prevents an exact physical path match",
					"remove the surrounding whitespace and use the resolved absolute path",
					nil,
				)
			case !filepath.IsAbs(exclusion.ClonePath):
				return newSelectionExclusionValidationError(
					clonePathField,
					fmt.Sprintf("clone path %q is not absolute", exclusion.ClonePath),
					"a relative path does not provide stable physical clone identity",
					"replace it with the resolved absolute clone path",
					nil,
				)
			case filepath.Clean(exclusion.ClonePath) != exclusion.ClonePath:
				return newSelectionExclusionValidationError(
					clonePathField,
					fmt.Sprintf("clone path %q is not clean", exclusion.ClonePath),
					"dot segments or redundant separators do not provide canonical physical identity",
					fmt.Sprintf("replace it with %q", filepath.Clean(exclusion.ClonePath)),
					nil,
				)
			}
			if firstIndex, duplicate := seenClonePaths[exclusion.ClonePath]; duplicate {
				return newSelectionExclusionValidationError(
					clonePathField,
					fmt.Sprintf("the clone path duplicates branches[%d].clonePath", firstIndex),
					"one exact clone must have one branch exclusion row per harness",
					"merge the branch names into the first row and remove this duplicate",
					nil,
				)
			}
			seenClonePaths[exclusion.ClonePath] = exclusionIndex
			if len(exclusion.Branches) == 0 {
				return newSelectionExclusionValidationError(
					rowPath+".branches",
					"the branch list is empty",
					"the row does not identify any exact branch to deny",
					"add at least one branch name or remove this row",
					nil,
				)
			}
			seenBranches := make(map[string]int, len(exclusion.Branches))
			for branchIndex, branch := range exclusion.Branches {
				fieldPath := fmt.Sprintf("%s.branches[%d]", rowPath, branchIndex)
				normalized := strings.TrimSpace(branch)
				switch {
				case branch == "":
					return newSelectionExclusionValidationError(
						fieldPath,
						"the branch name is empty",
						"empty text cannot identify one Git branch",
						"set the exact case-sensitive branch name or remove this entry",
						nil,
					)
				case normalized != branch:
					return newSelectionExclusionValidationError(
						fieldPath,
						fmt.Sprintf("branch name %q is not normalized", branch),
						"leading or trailing whitespace prevents an exact branch match",
						fmt.Sprintf("replace it with %q", normalized),
						nil,
					)
				}
				if reason := invalidGitBranchReason(branch); reason != "" {
					return newSelectionExclusionValidationError(
						fieldPath,
						fmt.Sprintf("branch name %q is not a valid Git branch name", branch),
						reason,
						"use the exact branch name reported by Git or remove this entry",
						nil,
					)
				}
				if firstIndex, duplicate := seenBranches[branch]; duplicate {
					return newSelectionExclusionValidationError(
						fieldPath,
						fmt.Sprintf("the branch name duplicates branches[%d]", firstIndex),
						"one exact branch denial must appear only once per clone",
						"remove the duplicate branch name",
						nil,
					)
				}
				seenBranches[branch] = branchIndex
			}
		}
	}
	return nil
}

func invalidGitBranchReason(branch string) string {
	switch {
	case branch == "@":
		return "Git reserves the single at-sign ref name"
	case strings.HasPrefix(branch, "-"):
		return "Git branch shorthand cannot start with a hyphen"
	case strings.HasPrefix(branch, "/") || strings.HasSuffix(branch, "/"):
		return "Git branch names cannot start or end with a slash"
	case strings.Contains(branch, "//"):
		return "Git branch names cannot contain consecutive slashes"
	case strings.Contains(branch, ".."):
		return "Git branch names cannot contain two consecutive dots"
	case strings.Contains(branch, "@{"):
		return "Git branch names cannot contain the reflog sequence @{"
	case strings.HasSuffix(branch, "."):
		return "Git branch names cannot end with a dot"
	}
	for _, component := range strings.Split(branch, "/") {
		if strings.HasPrefix(component, ".") {
			return "Git branch path components cannot start with a dot"
		}
		if strings.HasSuffix(strings.ToLower(component), ".lock") {
			return "Git branch path components cannot end with .lock"
		}
	}
	for _, character := range branch {
		if character <= ' ' || character == 0x7f || strings.ContainsRune("~^:?*[\\", character) {
			return fmt.Sprintf("Git branch names cannot contain character %q", character)
		}
	}
	return ""
}

func newSelectionExclusionValidationError(fieldPath, what, why, fix string, cause error) error {
	message := fmt.Sprintf(
		"config: %s: what: %s; why: %s; where: selection validation in internal/config/config.go; when: while loading exact selection exclusions; meaning: Peasant did not load the selection because this denial cannot be applied exactly; fix: %s",
		fieldPath,
		what,
		why,
		fix,
	)
	if cause == nil {
		return errors.New(message)
	}
	return fmt.Errorf("%s: %w", message, cause)
}
