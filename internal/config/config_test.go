package config

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/redact"
)

// --- TestParse_ValidConfig ---

func TestParse_ValidConfig(t *testing.T) {
	yaml := `
version: 1
user:
  email: alice@example.com
redaction:
  level: standard
sources:
  claude-code:
    enabled: true
    paths:
      - /home/alice/.claude/projects
      - /home/alice/.claude/extra
  opencode:
    enabled: false
    paths:
      - /home/alice/.opencode
  cursor:
    enabled: false
    paths:
      - /home/alice/.cursor/projects
output:
  basePath: /home/alice/.local/state/peasant
  stalenessThresholdSec: 120
village:
  url: https://village.example.com
  connected: true
daemon:
  projectMode: opt-out
push:
  method: by-source
  sources:
    - claude
    - opencode
  visibility: public
`

	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}

	if cfg.Version != 1 {
		t.Errorf("Version = %d, want 1", cfg.Version)
	}
	if cfg.User.Email != "alice@example.com" {
		t.Errorf("User.Email = %q, want %q", cfg.User.Email, "alice@example.com")
	}
	if cfg.Redaction.Level != redact.Standard {
		t.Errorf("Redaction.Level = %q, want %q", cfg.Redaction.Level, redact.Standard)
	}
	if cfg.Sources.Strike.Enabled {
		t.Error("Sources.Strike.Enabled = true, want disabled-by-default opt-in")
	}
	if len(cfg.Sources.Strike.Paths) != 1 || cfg.Sources.Strike.Paths[0] != defaults.DefaultStrikePath.String() {
		t.Errorf("Sources.Strike.Paths = %v, want [%s]", cfg.Sources.Strike.Paths, defaults.DefaultStrikePath)
	}
	if !cfg.Sources.ClaudeCode.Enabled {
		t.Errorf("Sources.Claude.Enabled = false, want true")
	}
	if len(cfg.Sources.ClaudeCode.Paths) != 2 {
		t.Errorf("Sources.Claude.Paths len = %d, want 2", len(cfg.Sources.ClaudeCode.Paths))
	}
	if cfg.Sources.OpenCode.Enabled {
		t.Errorf("Sources.OpenCode.Enabled = true, want false")
	}
	if cfg.Output.BasePath != "/home/alice/.local/state/peasant" {
		t.Errorf("Output.BasePath = %q, want %q", cfg.Output.BasePath, "/home/alice/.local/state/peasant")
	}
	if cfg.Output.StalenessThresholdSec != 120 {
		t.Errorf("Output.StalenessThresholdSec = %d, want 120", cfg.Output.StalenessThresholdSec)
	}
	// New fields: village.
	if !cfg.Village.Connected {
		t.Errorf("Village.Connected = false, want true")
	}
	if cfg.Village.URL != "https://village.example.com" {
		t.Errorf("Village.URL = %q, want %q", cfg.Village.URL, "https://village.example.com")
	}
	// New fields: daemon.
	if cfg.Daemon.ProjectMode != "opt-out" {
		t.Errorf("Daemon.ProjectMode = %q, want %q", cfg.Daemon.ProjectMode, "opt-out")
	}
	// New fields: push.
	if cfg.Push.Method != PushMethodBySource {
		t.Errorf("Push.Method = %q, want %q", cfg.Push.Method, PushMethodBySource)
	}
	if len(cfg.Push.Sources) != 2 {
		t.Errorf("Push.Sources len = %d, want 2", len(cfg.Push.Sources))
	}
	if cfg.Push.Visibility != VisibilityPublic {
		t.Errorf("Push.Visibility = %q, want %q", cfg.Push.Visibility, VisibilityPublic)
	}
}

// --- TestParse_Defaults ---

func TestParse_Defaults(t *testing.T) {
	// Provide the minimum required fields; everything else should come from defaults.
	yaml := `
version: 1
sources:
  claude-code:
    enabled: true
    paths:
      - ~/.claude/projects
`

	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}

	// Redaction level should default to Standard, since the village requires Standard-level redaction.
	if cfg.Redaction.Level != redact.Standard {
		t.Errorf("Redaction.Level = %q, want %q", cfg.Redaction.Level, redact.Standard)
	}

	// StalenessThresholdSec should default to 60.
	if cfg.Output.StalenessThresholdSec != 60 {
		t.Errorf("Output.StalenessThresholdSec = %d, want 60", cfg.Output.StalenessThresholdSec)
	}

	// BasePath should default to the XDG-resolved output base (honors
	// XDG_DATA_HOME, not a hardcoded ~/.local/share path.
	if want := defaults.ResolveOutputBasePath().String(); cfg.Output.BasePath != want {
		t.Errorf("Output.BasePath = %q, want default %q", cfg.Output.BasePath, want)
	}

	// Push defaults: method=all, visibility=private.
	if cfg.Push.Method != PushMethodAll {
		t.Errorf("Push.Method = %q, want default %q", cfg.Push.Method, PushMethodAll)
	}
	if cfg.Push.Visibility != VisibilityPrivate {
		t.Errorf("Push.Visibility = %q, want default %q", cfg.Push.Visibility, VisibilityPrivate)
	}

	// Daemon default: opt-in.
	if cfg.Daemon.ProjectMode != "opt-in" {
		t.Errorf("Daemon.ProjectMode = %q, want default %q", cfg.Daemon.ProjectMode, "opt-in")
	}
}

// --- TestParse_InvalidRedactionLevel ---

func TestParse_InvalidRedactionLevel(t *testing.T) {
	tests := []struct {
		name  string
		level string
	}{
		{"empty level", ""},
		{"unknown level", "aggressive"},
		{"capitalized", "Minimal"},
		{"numeric", "1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var yaml string
			if tt.level == "" {
				yaml = `
version: 1
redaction:
  level: ""
sources:
  claude-code:
    enabled: true
    paths:
      - ~/.claude/projects
`
			} else {
				yaml = `
version: 1
redaction:
  level: ` + tt.level + `
sources:
  claude-code:
    enabled: true
    paths:
      - ~/.claude/projects
`
			}

			_, err := Parse([]byte(yaml))
			if err == nil {
				t.Fatalf("Parse with level=%q: expected error, got nil", tt.level)
			}
			if !strings.Contains(err.Error(), "unknown redaction level") {
				t.Errorf("error = %q, want to contain %q", err.Error(), "unknown redaction level")
			}
		})
	}
}

// --- TestParse_InvalidVersion ---

func TestParse_InvalidVersion(t *testing.T) {
	tests := []struct {
		name    string
		version int
	}{
		{"version 0", 0},
		{"version 2", 2},
		{"version negative", -1},
		{"version 99", 99},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			yaml := `
version: ` + intToStr(tt.version) + `
redaction:
  level: minimal
sources:
  claude-code:
    enabled: true
    paths:
      - ~/.claude/projects
`

			_, err := Parse([]byte(yaml))
			if err == nil {
				t.Fatalf("Parse with version=%d: expected error, got nil", tt.version)
			}
			if !strings.Contains(err.Error(), "unsupported config version") {
				t.Errorf("error = %q, want to contain %q", err.Error(), "unsupported config version")
			}
		})
	}
}

// --- TestParse_InvalidStaleness ---

func TestParse_InvalidStaleness(t *testing.T) {
	yaml := `
version: 1
redaction:
  level: minimal
sources:
  claude-code:
    enabled: true
    paths:
      - ~/.claude/projects
output:
  stalenessThresholdSec: -1
`

	_, err := Parse([]byte(yaml))
	if err == nil {
		t.Fatal("Parse with negative stalenessThresholdSec: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "stalenessThresholdSec") {
		t.Errorf("error = %q, want to contain %q", err.Error(), "stalenessThresholdSec")
	}
}

// --- TestParse_EnabledSourceNoPaths ---

func TestParse_EnabledSourceNoPaths(t *testing.T) {
	tests := []struct {
		name string
		yaml string
	}{
		{
			name: "claude enabled no paths",
			yaml: `
version: 1
redaction:
  level: minimal
sources:
  claude-code:
    enabled: true
    paths: []
  opencode:
    enabled: false
    paths: []
`,
		},
		{
			name: "opencode enabled no paths",
			yaml: `
version: 1
redaction:
  level: minimal
sources:
  claude-code:
    enabled: false
    paths: []
  opencode:
    enabled: true
    paths: []
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
			if err == nil {
				t.Fatalf("Parse(%q): expected error for enabled source with no paths", tt.name)
			}
			if !strings.Contains(err.Error(), "at least one path") {
				t.Errorf("error = %q, want to contain %q", err.Error(), "at least one path")
			}
		})
	}
}

// --- TestParse_PushMethodValidation ---

func TestParse_PushMethodValidation(t *testing.T) {
	// Unknown method value.
	t.Run("unknown method", func(t *testing.T) {
		yaml := `
version: 1
sources:
  claude-code:
    enabled: true
    paths:
      - ~/.claude/projects
push:
  method: bulk
  sources:
    - claude
`
		_, err := Parse([]byte(yaml))
		if err == nil {
			t.Fatal("Parse with unknown push.method: expected error, got nil")
		}
		if !strings.Contains(err.Error(), "unknown push.method") {
			t.Errorf("error = %q, want to contain %q", err.Error(), "unknown push.method")
		}
	})

	// by-source with no sources.
	t.Run("by-source with empty sources", func(t *testing.T) {
		yaml := `
version: 1
sources:
  claude-code:
    enabled: true
    paths:
      - ~/.claude/projects
push:
  method: by-source
`
		_, err := Parse([]byte(yaml))
		if err == nil {
			t.Fatal("Parse with by-source and no sources: expected error, got nil")
		}
		if !strings.Contains(err.Error(), "push.sources is empty") {
			t.Errorf("error = %q, want to contain %q", err.Error(), "push.sources is empty")
		}
	})
}

// --- TestParse_VisibilityValidation ---

func TestParse_VisibilityValidation(t *testing.T) {
	yaml := `
version: 1
sources:
  claude-code:
    enabled: true
    paths:
      - ~/.claude/projects
push:
  method: all
  visibility: hidden
`

	_, err := Parse([]byte(yaml))
	if err == nil {
		t.Fatal("Parse with unknown visibility: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "unknown push.visibility") {
		t.Errorf("error = %q, want to contain %q", err.Error(), "unknown push.visibility")
	}
}

// --- TestParse_LicenseValidation ---

func TestParse_LicenseValidation(t *testing.T) {
	t.Run("invalid license is rejected", func(t *testing.T) {
		yaml := `
version: 1
sources:
  claude-code:
    enabled: true
    paths:
      - ~/.claude/projects
push:
  method: all
  license: MIT
`
		_, err := Parse([]byte(yaml))
		if err == nil {
			t.Fatal("Parse with unknown license: expected error, got nil")
		}
		if !strings.Contains(err.Error(), "unknown push.license") {
			t.Errorf("error = %q, want to contain %q", err.Error(), "unknown push.license")
		}
	})

	t.Run("valid license parses", func(t *testing.T) {
		yaml := `
version: 1
sources:
  claude-code:
    enabled: true
    paths:
      - ~/.claude/projects
push:
  method: all
  license: CC-BY-4.0
`
		cfg, err := Parse([]byte(yaml))
		if err != nil {
			t.Fatalf("Parse with valid license: %v", err)
		}
		if cfg.Push.License != LicenseCCBY {
			t.Errorf("parsed push.license = %q, want %q", cfg.Push.License, LicenseCCBY)
		}
	})

	t.Run("absent license is valid (no imposed default)", func(t *testing.T) {
		yaml := `
version: 1
sources:
  claude-code:
    enabled: true
    paths:
      - ~/.claude/projects
push:
  method: all
`
		cfg, err := Parse([]byte(yaml))
		if err != nil {
			t.Fatalf("Parse without license: %v", err)
		}
		if cfg.Push.License != "" {
			t.Errorf("absent license should be empty, got %q", cfg.Push.License)
		}
	})
}

// --- TestParse_VillageConnectedRequiresURL ---

func TestParse_VillageConnectedRequiresURL(t *testing.T) {
	yaml := `
version: 1
sources:
  claude-code:
    enabled: true
    paths:
      - ~/.claude/projects
village:
  url: ""
  connected: true
`

	_, err := Parse([]byte(yaml))
	if err == nil {
		t.Fatal("Parse with connected=true and empty URL: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "village.connected is true but village.url is empty") {
		t.Errorf("error = %q, want to contain %q", err.Error(), "village.connected is true but village.url is empty")
	}
}

// --- TestParse_PushBySourceWithSources ---

func TestParse_PushBySourceWithSources(t *testing.T) {
	// by-source with sources specified — should succeed.
	yaml := `
version: 1
sources:
  claude-code:
    enabled: true
    paths:
      - ~/.claude/projects
push:
  method: by-source
  sources:
    - claude
  visibility: private
`

	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse with by-source + sources: unexpected error: %v", err)
	}
	if cfg.Push.Method != PushMethodBySource {
		t.Errorf("Push.Method = %q, want %q", cfg.Push.Method, PushMethodBySource)
	}
	if len(cfg.Push.Sources) != 1 {
		t.Errorf("Push.Sources len = %d, want 1", len(cfg.Push.Sources))
	}
}

// --- TestPushMethod_IsValid ---

func TestPushMethod_IsValid(t *testing.T) {
	valid := []PushMethod{PushMethodAll, PushMethodBySource, PushMethodIndividual}
	for _, m := range valid {
		if !m.IsValid() {
			t.Errorf("PushMethod(%q).IsValid() = false, want true", m)
		}
	}

	invalid := []PushMethod{"", "bulk", "manual", "ALL"}
	for _, m := range invalid {
		if m.IsValid() {
			t.Errorf("PushMethod(%q).IsValid() = true, want false", m)
		}
	}
}

// --- TestVisibility_IsValid ---

func TestVisibility_IsValid(t *testing.T) {
	valid := []Visibility{VisibilityPublic, VisibilityPrivate, VisibilityShared}
	for _, v := range valid {
		if !v.IsValid() {
			t.Errorf("Visibility(%q).IsValid() = false, want true", v)
		}
	}

	invalid := []Visibility{"", "hidden", "unlisted", "PUBLIC"}
	for _, v := range invalid {
		if v.IsValid() {
			t.Errorf("Visibility(%q).IsValid() = true, want false", v)
		}
	}
}

// --- TestVisibility_SharedResolvesToGroup ---

func TestVisibility_SharedResolvesToGroup(t *testing.T) {
	// VisibilityShared is a deprecated alias that resolves to VisibilityGroup.
	// Both Go constants must be equal ("group").
	if VisibilityShared != VisibilityGroup {
		t.Errorf("VisibilityShared = %q, want %q (same as VisibilityGroup)", VisibilityShared, VisibilityGroup)
	}
	// String representation of both must be "group".
	if VisibilityShared.String() != "group" {
		t.Errorf("VisibilityShared.String() = %q, want %q", VisibilityShared.String(), "group")
	}
	if VisibilityGroup.String() != "group" {
		t.Errorf("VisibilityGroup.String() = %q, want %q", VisibilityGroup.String(), "group")
	}
	// IsValid must return true for VisibilityShared (it's just "group" under the hood).
	if !VisibilityShared.IsValid() {
		t.Error("VisibilityShared.IsValid() = false, want true (deprecated alias for group)")
	}
}

// --- TestParse_VisibilityGroupAccepted ---

func TestParse_VisibilityGroupAccepted(t *testing.T) {
	// "group" is accepted directly in YAML config.
	yaml := `
version: 1
sources:
  claude-code:
    enabled: true
    paths:
      - ~/.claude/projects
push:
  method: all
  visibility: group
`

	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse with visibility: group: unexpected error: %v", err)
	}
	if cfg.Push.Visibility != VisibilityGroup {
		t.Errorf("Push.Visibility = %q, want %q", cfg.Push.Visibility, VisibilityGroup)
	}
	// Also verify it equals VisibilityShared (backward compat).
	if cfg.Push.Visibility != VisibilityShared {
		t.Errorf("Push.Visibility = %q, want equal to VisibilityShared (%q)", cfg.Push.Visibility, VisibilityShared)
	}
}

// --- TestParse_VisibilitySharedYAML ---

// TestParse_VisibilitySharedYAML verifies that "shared" in YAML is a hard error.
// Users must update their config.yaml to use "group" explicitly.
func TestParse_VisibilitySharedYAML(t *testing.T) {
	yaml := `
version: 1
sources:
  claude-code:
    enabled: true
    paths:
      - ~/.claude/projects
push:
  method: all
  visibility: shared
`

	_, err := Parse([]byte(yaml))
	if err == nil {
		t.Fatal("Parse with visibility: shared: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "is no longer valid") {
		t.Errorf("error = %q, want to contain %q", err.Error(), "is no longer valid")
	}
	if !strings.Contains(err.Error(), "group") {
		t.Errorf("error = %q, want to contain %q (migration hint)", err.Error(), "group")
	}
}

// --- TestLoadDefaults_AutoDetectsEmail ---

func TestLoadDefaults_AutoDetectsEmail(t *testing.T) {
	git := testutil.DefaultGitResolver()

	cfg := LoadDefaults(context.Background(), git)

	if cfg.User.Email != testutil.TestEmail {
		t.Errorf("User.Email = %q, want %q", cfg.User.Email, testutil.TestEmail)
	}
	if cfg.Version != 1 {
		t.Errorf("Version = %d, want 1", cfg.Version)
	}
	if cfg.Redaction.Level != redact.Standard {
		t.Errorf("Redaction.Level = %q, want %q", cfg.Redaction.Level, redact.Standard)
	}
	if !cfg.Sources.ClaudeCode.Enabled {
		t.Errorf("Sources.Claude.Enabled = false, want true")
	}
	if len(cfg.Sources.ClaudeCode.Paths) == 0 {
		t.Errorf("Sources.Claude.Paths is empty, want at least one default")
	}
	if !cfg.Sources.OpenCode.Enabled {
		t.Errorf("Sources.OpenCode.Enabled = false, want true")
	}
	if cfg.Output.StalenessThresholdSec != 60 {
		t.Errorf("Output.StalenessThresholdSec = %d, want 60", cfg.Output.StalenessThresholdSec)
	}
}

// --- TestLoadDefaults_NoGit ---

func TestLoadDefaults_NoGit(t *testing.T) {
	git := testutil.NoGitResolver()

	cfg := LoadDefaults(context.Background(), git)

	if cfg.User.Email != "" {
		t.Errorf("User.Email = %q, want empty when git fails", cfg.User.Email)
	}
	// Other defaults should still be populated.
	if cfg.Version != 1 {
		t.Errorf("Version = %d, want 1", cfg.Version)
	}
	if cfg.Output.StalenessThresholdSec != 60 {
		t.Errorf("Output.StalenessThresholdSec = %d, want 60", cfg.Output.StalenessThresholdSec)
	}
}

// --- TestLoadDefaults_NilGit ---

func TestLoadDefaults_NilGit(t *testing.T) {
	cfg := LoadDefaults(context.Background(), nil)

	if cfg.User.Email != "" {
		t.Errorf("User.Email = %q, want empty when git is nil", cfg.User.Email)
	}
	if cfg.Version != 1 {
		t.Errorf("Version = %d, want 1", cfg.Version)
	}
}

// --- TestLoad_FileNotFound ---

func TestLoad_FileNotFound(t *testing.T) {
	memfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	// Path that does not exist in the MemFS.
	cfg, err := Load("/nonexistent/config.yaml", memfs, git)
	if err != nil {
		t.Fatalf("Load with missing file: expected nil error, got %v", err)
	}
	if cfg == nil {
		t.Fatal("Load with missing file: expected non-nil Config, got nil")
	}
	// Should have returned defaults.
	if cfg.Version != 1 {
		t.Errorf("Version = %d, want 1", cfg.Version)
	}
	if cfg.User.Email != testutil.TestEmail {
		t.Errorf("User.Email = %q, want %q", cfg.User.Email, testutil.TestEmail)
	}
}

// --- TestLoad_EmptyPath ---

func TestLoad_EmptyPath(t *testing.T) {
	memfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	cfg, err := Load("", memfs, git)
	if err != nil {
		t.Fatalf("Load with empty path: unexpected error: %v", err)
	}
	if cfg == nil {
		t.Fatal("Load with empty path: expected non-nil Config")
	}
	if cfg.Version != 1 {
		t.Errorf("Version = %d, want 1", cfg.Version)
	}
}

// --- TestLoad_ValidFile ---

func TestLoad_ValidFile(t *testing.T) {
	const yamlContent = `
version: 1
user:
  email: bob@example.com
redaction:
  level: maximum
sources:
  claude-code:
    enabled: true
    paths:
      - /data/claude
  opencode:
    enabled: true
    paths:
      - /data/opencode
output:
  basePath: /data/output
  stalenessThresholdSec: 30
`

	memfs := testutil.NewMemFS()
	if err := memfs.WriteFile("/etc/peasant/config.yaml", []byte(yamlContent), 0644); err != nil {
		t.Fatalf("setup WriteFile: %v", err)
	}

	git := testutil.DefaultGitResolver()

	cfg, err := Load("/etc/peasant/config.yaml", memfs, git)
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	if cfg.User.Email != "bob@example.com" {
		t.Errorf("User.Email = %q, want %q", cfg.User.Email, "bob@example.com")
	}
	if cfg.Redaction.Level != redact.Maximum {
		t.Errorf("Redaction.Level = %q, want %q", cfg.Redaction.Level, redact.Maximum)
	}
	if cfg.Output.StalenessThresholdSec != 30 {
		t.Errorf("Output.StalenessThresholdSec = %d, want 30", cfg.Output.StalenessThresholdSec)
	}
	if cfg.Output.BasePath != "/data/output" {
		t.Errorf("Output.BasePath = %q, want %q", cfg.Output.BasePath, "/data/output")
	}
}

// --- TestParse_CustomPatterns_Valid ---

func TestParse_CustomPatterns_Valid(t *testing.T) {
	yaml := `
version: 1
redaction:
  level: minimal
  custom_patterns:
    - id: my-secret
      category: secrets
      pattern: "MYSECRET-[A-Z0-9]+"
      replacement: "<MY_SECRET>"
    - id: my-pii
      category: pii
      pattern: "[0-9]{3}-[0-9]{2}-[0-9]{4}"
      replacement: "<SSN>"
    - id: my-paths
      category: paths
      pattern: "/home/[a-z]+"
      replacement: "/home/<USER>"
sources:
  claude-code:
    enabled: true
    paths:
      - ~/.claude/projects
`

	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse with valid custom_patterns: unexpected error: %v", err)
	}

	if len(cfg.Redaction.CustomPatterns) != 3 {
		t.Fatalf("CustomPatterns len = %d, want 3", len(cfg.Redaction.CustomPatterns))
	}

	pat := cfg.Redaction.CustomPatterns[0]
	if pat.ID != "my-secret" {
		t.Errorf("CustomPatterns[0].ID = %q, want %q", pat.ID, "my-secret")
	}
	if pat.Category != CategorySecrets {
		t.Errorf("CustomPatterns[0].Category = %q, want %q", pat.Category, CategorySecrets)
	}
	if pat.Pattern != "MYSECRET-[A-Z0-9]+" {
		t.Errorf("CustomPatterns[0].Pattern = %q, want %q", pat.Pattern, "MYSECRET-[A-Z0-9]+")
	}
	if pat.Replacement != "<MY_SECRET>" {
		t.Errorf("CustomPatterns[0].Replacement = %q, want %q", pat.Replacement, "<MY_SECRET>")
	}

	if cfg.Redaction.CustomPatterns[1].Category != CategoryPII {
		t.Errorf("CustomPatterns[1].Category = %q, want %q", cfg.Redaction.CustomPatterns[1].Category, CategoryPII)
	}
	if cfg.Redaction.CustomPatterns[2].Category != CategoryPaths {
		t.Errorf("CustomPatterns[2].Category = %q, want %q", cfg.Redaction.CustomPatterns[2].Category, CategoryPaths)
	}
}

// --- TestParse_CustomPatterns_EmptyID ---

func TestParse_CustomPatterns_EmptyID(t *testing.T) {
	yaml := `
version: 1
redaction:
  level: minimal
  custom_patterns:
    - id: ""
      category: secrets
      pattern: "SECRET"
      replacement: "<REDACTED>"
sources:
  claude-code:
    enabled: true
    paths:
      - ~/.claude/projects
`

	_, err := Parse([]byte(yaml))
	if err == nil {
		t.Fatal("Parse with empty custom_patterns id: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "empty id") {
		t.Errorf("error = %q, want to contain %q", err.Error(), "empty id")
	}
}

// --- TestParse_CustomPatterns_UnknownCategory ---

func TestParse_CustomPatterns_UnknownCategory(t *testing.T) {
	yaml := `
version: 1
redaction:
  level: minimal
  custom_patterns:
    - id: bad-cat
      category: credentials
      pattern: "SECRET"
      replacement: "<REDACTED>"
sources:
  claude-code:
    enabled: true
    paths:
      - ~/.claude/projects
`

	_, err := Parse([]byte(yaml))
	if err == nil {
		t.Fatal("Parse with unknown category: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "credentials") {
		t.Errorf("error = %q, want to contain the unknown value %q", err.Error(), "credentials")
	}
	if !strings.Contains(err.Error(), "unknown category") {
		t.Errorf("error = %q, want to contain %q", err.Error(), "unknown category")
	}
}

// --- TestParse_CustomPatterns_InvalidRegex ---

func TestParse_CustomPatterns_InvalidRegex(t *testing.T) {
	yaml := `
version: 1
redaction:
  level: minimal
  custom_patterns:
    - id: bad-regex
      category: secrets
      pattern: "[invalid("
      replacement: "<REDACTED>"
sources:
  claude-code:
    enabled: true
    paths:
      - ~/.claude/projects
`

	_, err := Parse([]byte(yaml))
	if err == nil {
		t.Fatal("Parse with invalid regex: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "invalid regex") {
		t.Errorf("error = %q, want to contain %q", err.Error(), "invalid regex")
	}
}

func TestParse_CustomPatterns_DuplicateID(t *testing.T) {
	yaml := `
version: 1
redaction:
  level: minimal
  custom_patterns:
    - id: my-pattern
      category: secrets
      pattern: "secret-[0-9]+"
      replacement: "<REDACTED>"
    - id: my-pattern
      category: pii
      pattern: "user-[0-9]+"
      replacement: "<PII>"
sources:
  claude-code:
    enabled: true
    paths:
      - ~/.claude/projects
`

	_, err := Parse([]byte(yaml))
	if err == nil {
		t.Fatal("Parse with duplicate pattern ID: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "duplicate custom pattern id") {
		t.Errorf("error = %q, want to contain %q", err.Error(), "duplicate custom pattern id")
	}
}

// --- TestPatternCategory_RoundTripYAML ---

func TestPatternCategory_RoundTripYAML(t *testing.T) {
	// Verify that PatternCategory constants round-trip through string conversion.
	cases := []struct {
		cat  PatternCategory
		want string
	}{
		{CategorySecrets, "secrets"},
		{CategoryPII, "pii"},
		{CategoryPaths, "paths"},
	}

	for _, tc := range cases {
		if tc.cat.String() != tc.want {
			t.Errorf("PatternCategory(%q).String() = %q, want %q", tc.cat, tc.cat.String(), tc.want)
		}
		// Verify round-trip: PatternCategory("secrets") == CategorySecrets
		roundTripped := PatternCategory(tc.want)
		if roundTripped != tc.cat {
			t.Errorf("PatternCategory(%q) = %q, want %q", tc.want, roundTripped, tc.cat)
		}
	}
}

// --- TestCustomPatternsToUserPatterns_Valid ---

func TestCustomPatternsToUserPatterns_Valid(t *testing.T) {
	patterns := []CustomPattern{
		{ID: "test-secret", Category: CategorySecrets, Pattern: "TOPSECRET-[A-Z]+", Replacement: "<REDACTED>"},
	}

	userPatterns, err := CustomPatternsToUserPatterns(patterns)
	if err != nil {
		t.Fatalf("CustomPatternsToUserPatterns: unexpected error: %v", err)
	}
	if len(userPatterns) != 1 {
		t.Fatalf("len(userPatterns) = %d, want 1", len(userPatterns))
	}
	if userPatterns[0].ID != "test-secret" {
		t.Errorf("userPatterns[0].ID = %q, want %q", userPatterns[0].ID, "test-secret")
	}
	if userPatterns[0].Pattern != "TOPSECRET-[A-Z]+" {
		t.Errorf("userPatterns[0].Pattern = %q, want %q", userPatterns[0].Pattern, "TOPSECRET-[A-Z]+")
	}
	if userPatterns[0].Replacement != "<REDACTED>" {
		t.Errorf("userPatterns[0].Replacement = %q, want %q", userPatterns[0].Replacement, "<REDACTED>")
	}
}

// --- TestParse_MockConfig_Defaults ---

func TestParse_MockConfig_Defaults(t *testing.T) {
	// Minimal YAML without mock section — defaults should apply.
	yaml := `
version: 1
sources:
  claude-code:
    enabled: true
    paths:
      - ~/.claude/projects
`

	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}

	if cfg.Sources.Mock.Enabled != defaults.DefaultMockEnabled {
		t.Errorf("Mock.Enabled = %v, want %v", cfg.Sources.Mock.Enabled, defaults.DefaultMockEnabled)
	}
	if len(cfg.Sources.Mock.Web) != len(defaults.DefaultMockWebSections) {
		t.Errorf("Mock.Web len = %d, want %d", len(cfg.Sources.Mock.Web), len(defaults.DefaultMockWebSections))
	}
	if len(cfg.Sources.Mock.TUI) != len(defaults.DefaultMockTUISections) {
		t.Errorf("Mock.TUI len = %d, want %d", len(cfg.Sources.Mock.TUI), len(defaults.DefaultMockTUISections))
	}
	if len(cfg.Sources.Mock.API) != len(defaults.DefaultMockAPISections) {
		t.Errorf("Mock.API len = %d, want %d", len(cfg.Sources.Mock.API), len(defaults.DefaultMockAPISections))
	}
}

// --- TestParse_MockConfig_Explicit ---

func TestParse_MockConfig_Explicit(t *testing.T) {
	yaml := `
version: 1
sources:
  claude-code:
    enabled: true
    paths:
      - ~/.claude/projects
  mock:
    enabled: true
    web: [dashboard, sessions]
    tui: [sessions]
    api: []
`

	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}

	if !bool(cfg.Sources.Mock.Enabled) {
		t.Errorf("Mock.Enabled = false, want true")
	}
	if len(cfg.Sources.Mock.Web) != 2 {
		t.Fatalf("Mock.Web len = %d, want 2", len(cfg.Sources.Mock.Web))
	}
	if cfg.Sources.Mock.Web[0] != defaults.MockSections.Dashboard {
		t.Errorf("Mock.Web[0] = %q, want %q", cfg.Sources.Mock.Web[0], defaults.MockSections.Dashboard)
	}
	if cfg.Sources.Mock.Web[1] != defaults.MockSections.Sessions {
		t.Errorf("Mock.Web[1] = %q, want %q", cfg.Sources.Mock.Web[1], defaults.MockSections.Sessions)
	}
	if len(cfg.Sources.Mock.TUI) != 1 {
		t.Fatalf("Mock.TUI len = %d, want 1", len(cfg.Sources.Mock.TUI))
	}
	if cfg.Sources.Mock.TUI[0] != defaults.MockSections.Sessions {
		t.Errorf("Mock.TUI[0] = %q, want %q", cfg.Sources.Mock.TUI[0], defaults.MockSections.Sessions)
	}
	if len(cfg.Sources.Mock.API) != 0 {
		t.Errorf("Mock.API len = %d, want 0", len(cfg.Sources.Mock.API))
	}
}

// --- TestParse_MockConfig_InvalidSection ---

func TestParse_MockConfig_InvalidSection(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "invalid web section",
			yaml: `
version: 1
sources:
  claude-code:
    enabled: true
    paths:
      - ~/.claude/projects
  mock:
    enabled: true
    web: [dashboard, nonexistent]
`,
			wantErr: "unknown mock.web section",
		},
		{
			name: "invalid tui section",
			yaml: `
version: 1
sources:
  claude-code:
    enabled: true
    paths:
      - ~/.claude/projects
  mock:
    enabled: true
    tui: [badvalue]
`,
			wantErr: "unknown mock.tui section",
		},
		{
			name: "invalid api section",
			yaml: `
version: 1
sources:
  claude-code:
    enabled: true
    paths:
      - ~/.claude/projects
  mock:
    enabled: true
    api: [sessions, invalid]
`,
			wantErr: "unknown mock.api section",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
			if err == nil {
				t.Fatalf("Parse(%q): expected error, got nil", tt.name)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want to contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

// --- TestParse_MockConfig_DisabledSkipsValidation ---

func TestParse_MockConfig_DisabledSkipsValidation(t *testing.T) {
	// When mock is disabled, invalid section values should be accepted
	// because they won't be used.
	yaml := `
version: 1
sources:
  claude-code:
    enabled: true
    paths:
      - ~/.claude/projects
  mock:
    enabled: false
    web: [nonexistent]
`

	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse with disabled mock + invalid sections: unexpected error: %v", err)
	}
	if bool(cfg.Sources.Mock.Enabled) {
		t.Errorf("Mock.Enabled = true, want false")
	}
}

// --- TestLoadDefaults_MockConfig ---

func TestLoadDefaults_MockConfig(t *testing.T) {
	git := testutil.DefaultGitResolver()
	cfg := LoadDefaults(context.Background(), git)

	if cfg.Sources.Mock.Enabled != defaults.DefaultMockEnabled {
		t.Errorf("Mock.Enabled = %v, want %v", cfg.Sources.Mock.Enabled, defaults.DefaultMockEnabled)
	}
	// Default sections should match defaults package.
	if len(cfg.Sources.Mock.Web) != len(defaults.DefaultMockWebSections) {
		t.Errorf("Mock.Web len = %d, want %d", len(cfg.Sources.Mock.Web), len(defaults.DefaultMockWebSections))
	}
	for i, s := range cfg.Sources.Mock.Web {
		if s != defaults.DefaultMockWebSections[i] {
			t.Errorf("Mock.Web[%d] = %q, want %q", i, s, defaults.DefaultMockWebSections[i])
		}
	}
}

// --- TestApplyMockOverrides ---

func TestApplyMockOverrides(t *testing.T) {
	t.Run("nil opts does nothing", func(t *testing.T) {
		cfg := BaseConfig()
		origEnabled := cfg.Sources.Mock.Enabled
		cfg.ApplyMockOverrides(nil, defaults.MockComponents.Web)
		if cfg.Sources.Mock.Enabled != origEnabled {
			t.Errorf("Mock.Enabled changed from %v to %v", origEnabled, cfg.Sources.Mock.Enabled)
		}
	})

	t.Run("component override enables mock and clears sections", func(t *testing.T) {
		cfg := BaseConfig()
		opts := &MockDataStoreOptions{
			Components: map[defaults.MockComponent]bool{defaults.MockComponents.Web: true},
			Sections:   map[defaults.MockSection]bool{},
		}
		cfg.ApplyMockOverrides(opts, defaults.MockComponents.Web)
		if !bool(cfg.Sources.Mock.Enabled) {
			t.Errorf("Mock.Enabled = false, want true after override")
		}
		if cfg.Sources.Mock.Web != nil {
			t.Errorf("Mock.Web = %v, want nil (all sections mocked)", cfg.Sources.Mock.Web)
		}
	})

	t.Run("section override replaces configured sections", func(t *testing.T) {
		cfg := BaseConfig()
		opts := &MockDataStoreOptions{
			Components: map[defaults.MockComponent]bool{},
			Sections: map[defaults.MockSection]bool{
				defaults.MockSections.Sessions: true,
			},
		}
		cfg.ApplyMockOverrides(opts, defaults.MockComponents.Web)
		if !bool(cfg.Sources.Mock.Enabled) {
			t.Errorf("Mock.Enabled = false, want true after override")
		}
		if len(cfg.Sources.Mock.Web) != 1 {
			t.Fatalf("Mock.Web len = %d, want 1", len(cfg.Sources.Mock.Web))
		}
		if cfg.Sources.Mock.Web[0] != defaults.MockSections.Sessions {
			t.Errorf("Mock.Web[0] = %q, want %q", cfg.Sources.Mock.Web[0], defaults.MockSections.Sessions)
		}
	})
}

// --- TestParse_SelectionModeValidation ---

func TestParse_SelectionModeValidation(t *testing.T) {
	yaml := `
version: 1
sources:
  claude-code:
    enabled: true
    paths:
      - ~/.claude/projects
selection:
  mode: cherry-pick
`

	_, err := Parse([]byte(yaml))
	if err == nil {
		t.Fatal("Parse with unknown selection.mode: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "unknown selection.mode") {
		t.Errorf("error = %q, want to contain %q", err.Error(), "unknown selection.mode")
	}
}

// --- TestParse_SelectionSelectedNoProviders ---

func TestParse_SelectionSelectedNoHarnesses(t *testing.T) {
	yaml := `
version: 1
sources:
  claude-code:
    enabled: true
    paths:
      - ~/.claude/projects
selection:
  mode: selected
`

	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse with mode=selected and no harnesses: %v", err)
	}
	if cfg.Selection.Mode != SelectionModeSelected || len(cfg.Selection.Harnesses) != 0 {
		t.Fatalf("Selection = %+v, want a valid selected-empty configuration", cfg.Selection)
	}
}

// --- TestParse_SelectionValidConfig ---

func TestParse_SelectionValidConfig(t *testing.T) {
	t.Run("mode all", func(t *testing.T) {
		yaml := `
version: 1
sources:
  claude-code:
    enabled: true
    paths:
      - ~/.claude/projects
selection:
  mode: all
  autoIngestNewBranches: true
`
		cfg, err := Parse([]byte(yaml))
		if err != nil {
			t.Fatalf("Parse with mode=all: unexpected error: %v", err)
		}
		if cfg.Selection.Mode != SelectionModeAll {
			t.Errorf("Selection.Mode = %q, want %q", cfg.Selection.Mode, SelectionModeAll)
		}
		if !cfg.Selection.AutoIngestNewBranches {
			t.Error("Selection.AutoIngestNewBranches = false, want true")
		}
	})

	t.Run("mode selected with harnesses", func(t *testing.T) {
		yaml := `
version: 1
sources:
  claude-code:
    enabled: true
    paths:
      - ~/.claude/projects
selection:
  mode: selected
  autoIngestNewBranches: false
  harnesses:
    claude-code:
      projects:
        - gitRemote: https://github.com/org/repo.git
          branches:
            - main
            - develop
      sessions:
        - abc-123
`
		cfg, err := Parse([]byte(yaml))
		if err != nil {
			t.Fatalf("Parse with mode=selected: unexpected error: %v", err)
		}
		if cfg.Selection.Mode != SelectionModeSelected {
			t.Errorf("Selection.Mode = %q, want %q", cfg.Selection.Mode, SelectionModeSelected)
		}
		if cfg.Selection.AutoIngestNewBranches {
			t.Error("Selection.AutoIngestNewBranches = true, want false")
		}
		harnessCfg, ok := cfg.Selection.Harnesses[string(defaults.HarnessClaudeCode)]
		if !ok {
			t.Fatal("Selection.Harnesses missing 'claude-code' key")
		}
		if len(harnessCfg.Projects) != 1 {
			t.Fatalf("claude projects len = %d, want 1", len(harnessCfg.Projects))
		}
		if harnessCfg.Projects[0].GitRemote != "https://github.com/org/repo.git" {
			t.Errorf("GitRemote = %q, want %q", harnessCfg.Projects[0].GitRemote, "https://github.com/org/repo.git")
		}
		if len(harnessCfg.Projects[0].Branches) != 2 {
			t.Errorf("Branches len = %d, want 2", len(harnessCfg.Projects[0].Branches))
		}
		if len(harnessCfg.Sessions) != 1 || harnessCfg.Sessions[0] != "abc-123" {
			t.Errorf("Sessions = %v, want [abc-123]", harnessCfg.Sessions)
		}
	})
}

// TestParse_LegacySelectionProvidersRejected verifies the deprecation contract:
// the legacy "selection.providers:" YAML key (renamed to "harnesses" in the bestiary
// migration) is REJECTED with remediation, not silently ignored.
// The compatibility field remains so persisted legacy configuration fails with
// actionable remediation instead of being silently ignored.
func TestParse_LegacySelectionProvidersRejected(t *testing.T) {
	const head = `
version: 1
sources:
  claude-code:
    enabled: true
    paths:
      - ~/.claude/projects
`
	cases := []struct {
		name      string
		selection string
	}{
		{
			name: "legacy providers key only",
			selection: `selection:
  mode: selected
  providers:
    claude-code:
      projects:
        - gitRemote: https://github.com/org/repo.git
`,
		},
		{
			// Both keys present must still reject (legacy-key-wins-rejection is
			// intentional: validate() fires on any non-empty DeprecatedProviders,
			// independent of Harnesses).
			name: "both legacy providers and new harnesses",
			selection: `selection:
  mode: selected
  harnesses:
    claude-code:
      projects:
        - name: foo
  providers:
    claude-code:
      projects:
        - gitRemote: https://github.com/org/repo.git
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(head + tc.selection))
			if err == nil {
				t.Fatal("Parse with legacy selection.providers key: expected rejection error, got nil")
			}
			msg := err.Error()
			// Pin the message to the deprecation path explicitly (not just the
			// absence of the token in some other error): it must name the old key,
			// say it is no longer accepted, and point at the new key.
			if !strings.Contains(msg, "selection.providers") ||
				!strings.Contains(msg, "no longer accepted") ||
				!strings.Contains(msg, "harnesses") {
				t.Errorf("error should explain the providers->harnesses rename (deprecation path), got: %v", err)
			}
		})
	}
}

// --- helpers ---

// intToStr converts an int to its decimal string representation without importing strconv.
func intToStr(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	buf := make([]byte, 0, 20)
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	if neg {
		buf = append([]byte{'-'}, buf...)
	}
	return string(buf)
}

// --- TestParse_PushFieldVisibility_ProjectHash ---

// TestParse_PushFieldVisibility_ProjectHashLegacyKeyPresent verifies that the
// deprecated `push.fields.projectHash` key still parses (non-strict YAML) and is
// detected via HasDeprecatedProjectHashKey so the push path can warn. It must
// NOT fail parsing and must NOT gate anything (project.hash is always sent).
func TestParse_PushFieldVisibility_ProjectHashLegacyKeyPresent(t *testing.T) {
	yaml := `
version: 1
sources:
  claude-code:
    enabled: true
    paths:
      - ~/.claude/projects
push:
  method: all
  visibility: private
  fields:
    projectHash: true
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}

	if !cfg.Push.Fields.HasDeprecatedProjectHashKey() {
		t.Error("HasDeprecatedProjectHashKey() = false, want true when projectHash key is present")
	}
	if cfg.Push.Fields.ProjectHashLegacy == nil || *cfg.Push.Fields.ProjectHashLegacy != true {
		t.Errorf("ProjectHashLegacy = %v, want pointer to true", cfg.Push.Fields.ProjectHashLegacy)
	}
}

// TestParse_PushFieldVisibility_ProjectHashKeyAbsent verifies that with no
// projectHash key the legacy presence pointer stays nil and no deprecation
// warning would fire.
func TestParse_PushFieldVisibility_ProjectHashKeyAbsent(t *testing.T) {
	yaml := `
version: 1
sources:
  claude-code:
    enabled: true
    paths:
      - ~/.claude/projects
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}

	if cfg.Push.Fields.HasDeprecatedProjectHashKey() {
		t.Error("HasDeprecatedProjectHashKey() = true, want false when projectHash key is absent")
	}
	if cfg.Push.Fields.ProjectHashLegacy != nil {
		t.Errorf("ProjectHashLegacy = %v, want nil when key absent", cfg.Push.Fields.ProjectHashLegacy)
	}
}

// TestDefaultPushFieldVisibility_ProjectNameFalse verifies that ProjectName
// defaults to false in DefaultPushFieldVisibility — project names may reveal
// local directory structure or work context and must be opt-in.
func TestDefaultPushFieldVisibility_ProjectNameFalse(t *testing.T) {
	defaults := DefaultPushFieldVisibility()
	if defaults.ProjectName {
		t.Errorf("DefaultPushFieldVisibility().ProjectName = true, want false")
	}
}

// TestParse_PushFieldVisibility_ProjectNameRoundtrip verifies that ProjectName
// can be set to true via YAML and is correctly parsed.
func TestParse_PushFieldVisibility_ProjectNameRoundtrip(t *testing.T) {
	yaml := `
version: 1
sources:
  claude-code:
    enabled: true
    paths:
      - ~/.claude/projects
push:
  method: all
  visibility: private
  fields:
    projectName: true
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}

	if !cfg.Push.Fields.ProjectName {
		t.Errorf("Push.Fields.ProjectName = false, want true after parsing projectName: true")
	}
}

// TestParse_PushFieldVisibility_ProjectNameDefaultFalse verifies that
// ProjectName defaults to false when not specified in YAML.
func TestParse_PushFieldVisibility_ProjectNameDefaultFalse(t *testing.T) {
	yaml := `
version: 1
sources:
  claude-code:
    enabled: true
    paths:
      - ~/.claude/projects
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}

	if cfg.Push.Fields.ProjectName {
		t.Errorf("Push.Fields.ProjectName = true, want false (default must be omit)")
	}
}

// TestBaseConfig_OutputBaseHonorsXDGDataHome verifies the default config output
// base honors XDG_DATA_HOME — so ingest, which writes to
// cfg.Output.BasePath, is fully sandboxed by XDG_DATA_HOME (matching the DB path).
func TestBaseConfig_OutputBaseHonorsXDGDataHome(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmp)

	cfg := BaseConfig()
	want := filepath.Join(tmp, "peasant", "peasant-sync")
	if cfg.Output.BasePath != want {
		t.Errorf("BaseConfig output base = %q, want %q (must honor XDG_DATA_HOME)", cfg.Output.BasePath, want)
	}
}
