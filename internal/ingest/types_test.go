package ingest

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// --- SessionID ---

func TestNewSessionID(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
		errMsg  string
	}{
		// Valid formats
		{"valid UUID", "99d59925-36bc-424c-a789-8be54d9702ba", false, ""},
		{"valid UUID all zeros", "00000000-0000-0000-0000-000000000000", false, ""},
		{"valid subagent", "agent-a3aee4f", false, ""},
		{"valid subagent long hex", "agent-abcdef0123456789", false, ""},
		{"valid opencode session", "ses_3cd91f52effeXd3QAJ54jOyzv5", false, ""},
		{"valid opencode session short", "ses_abc", false, ""},
		{"valid opencode message", "msg_001abc", false, ""},
		{"valid opencode message short", "msg_x", false, ""},

		// Invalid: empty
		{"empty string", "", true, "empty string"},

		// Invalid: path traversal
		{"path traversal dotdot", "../evil", true, "path separator or traversal"},
		{"path traversal slash", "foo/bar", true, "path separator or traversal"},
		{"path traversal backslash", "foo\\bar", true, "path separator or traversal"},
		{"embedded dotdot", "ses_..abc", true, "path separator or traversal"},

		// Invalid: wrong format
		{"random string", "foobar", true, "must be UUID"},
		{"uppercase UUID", "99D59925-36BC-424C-A789-8BE54D9702BA", true, "must be UUID"},
		{"UUID with extra chars", "99d59925-36bc-424c-a789-8be54d9702baX", true, "must be UUID"},
		{"agent without hex", "agent-GHIJK", true, "must be UUID"},
		{"agent missing dash", "agenta3aee4f", true, "must be UUID"},
		{"ses missing underscore", "ses-abc", true, "must be UUID"},
		{"just ses_", "ses_", true, "must be UUID"},
		{"just msg_", "msg_", true, "must be UUID"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, err := NewSessionID(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("NewSessionID(%q) = %q, want error containing %q", tt.input, id, tt.errMsg)
				} else if !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("NewSessionID(%q) error = %q, want error containing %q", tt.input, err, tt.errMsg)
				}
			} else {
				if err != nil {
					t.Errorf("NewSessionID(%q) unexpected error: %v", tt.input, err)
				}
				if string(id) != tt.input {
					t.Errorf("NewSessionID(%q) = %q, want %q", tt.input, id, tt.input)
				}
			}
		})
	}
}

func TestSessionID_String(t *testing.T) {
	id, err := NewSessionID("99d59925-36bc-424c-a789-8be54d9702ba")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id.String() != "99d59925-36bc-424c-a789-8be54d9702ba" {
		t.Errorf("String() = %q, want %q", id.String(), "99d59925-36bc-424c-a789-8be54d9702ba")
	}
}

// --- ModelID ---

func TestNewModelID(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"valid claude opus", "claude-opus-4-6", false},
		{"valid claude sonnet", "claude-sonnet-4-6", false},
		{"valid arbitrary", "gpt-4o-mini", false},
		{"empty string", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, err := NewModelID(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("NewModelID(%q) = %q, want error", tt.input, id)
				}
			} else {
				if err != nil {
					t.Errorf("NewModelID(%q) unexpected error: %v", tt.input, err)
				}
				if string(id) != tt.input {
					t.Errorf("NewModelID(%q) = %q, want %q", tt.input, id, tt.input)
				}
			}
		})
	}
}

// --- ProjectHash ---

func TestNewProjectHash(t *testing.T) {
	validHash := "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"

	tests := []struct {
		name    string
		input   string
		wantErr bool
		errMsg  string
	}{
		{"valid 64-char hex", validHash, false, ""},
		{"valid all zeros", "0000000000000000000000000000000000000000000000000000000000000000", false, ""},
		{"valid all f", "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff", false, ""},
		{"too short", "a1b2c3", true, "64-character"},
		{"too long", validHash + "ff", true, "64-character"},
		{"uppercase hex", "A1B2C3D4E5F6A1B2C3D4E5F6A1B2C3D4E5F6A1B2C3D4E5F6A1B2C3D4E5F6A1B2", true, "lowercase hex"},
		{"non-hex characters", "g1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2", true, "lowercase hex"},
		{"empty", "", true, "64-character"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hash, err := NewProjectHash(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("NewProjectHash(%q) = %q, want error containing %q", tt.input, hash, tt.errMsg)
				} else if !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("NewProjectHash(%q) error = %q, want error containing %q", tt.input, err, tt.errMsg)
				}
			} else {
				if err != nil {
					t.Errorf("NewProjectHash(%q) unexpected error: %v", tt.input, err)
				}
				if string(hash) != tt.input {
					t.Errorf("NewProjectHash(%q) = %q, want %q", tt.input, hash, tt.input)
				}
			}
		})
	}
}

// --- HostSlug ---

func TestNewHostSlug(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
		errMsg  string
	}{
		{"valid github slug", "github.com--acme-dev--peasant", false, ""},
		{"valid gitlab slug", "gitlab.com--acme-dev--project", false, ""},
		{"valid untracked slug (new format)", "__peasant-untracked__--630ca830--my-project", false, ""},
		{"valid simple", "my-project", false, ""},
		{"valid with dots and dashes", "a.b-c_d", false, ""},

		{"empty string", "", true, "empty string"},
		{"contains slash", "foo/bar", true, "must contain only"},
		{"contains backslash", "foo\\bar", true, "must contain only"},
		{"contains space", "foo bar", true, "must contain only"},
		{"contains colon", "foo:bar", true, "must contain only"},
		{"path traversal", "..", true, "must not contain '..'"},
		{"hidden traversal", "foo..bar", true, "must not contain '..'"},
		{"leading traversal", "..hidden", true, "must not contain '..'"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			slug, err := NewHostSlug(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("NewHostSlug(%q) = %q, want error containing %q", tt.input, slug, tt.errMsg)
				} else if !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("NewHostSlug(%q) error = %q, want error containing %q", tt.input, err, tt.errMsg)
				}
			} else {
				if err != nil {
					t.Errorf("NewHostSlug(%q) unexpected error: %v", tt.input, err)
				}
				if string(slug) != tt.input {
					t.Errorf("NewHostSlug(%q) = %q, want %q", tt.input, slug, tt.input)
				}
			}
		})
	}
}

// --- ResolvedPath ---

func TestNewResolvedPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("failed to get home dir: %v", err)
	}

	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
		errMsg  string
	}{
		{"absolute path", "/tmp/foo", "/tmp/foo", false, ""},
		{"absolute with trailing slash", "/tmp/foo/", "/tmp/foo", false, ""},
		{"absolute with double slash", "/tmp//foo", "/tmp/foo", false, ""},
		{"absolute with dot", "/tmp/./foo", "/tmp/foo", false, ""},
		{"tilde expansion", "~/foo", filepath.Join(home, "foo"), false, ""},
		{"tilde only", "~", home, false, ""},
		{"tilde nested", "~/foo/bar/baz", filepath.Join(home, "foo/bar/baz"), false, ""},

		{"empty string", "", "", true, "empty string"},
		{"relative path", "foo/bar", "", true, "must be absolute"},
		{"just dot", ".", "", true, "must be absolute"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.wantErr {
				_, err := NewResolvedPath(tt.input)
				if err == nil {
					t.Errorf("NewResolvedPath(%q) expected error containing %q", tt.input, tt.errMsg)
				} else if !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("NewResolvedPath(%q) error = %q, want error containing %q", tt.input, err, tt.errMsg)
				}
			} else {
				path, err := NewResolvedPath(tt.input)
				if err != nil {
					t.Errorf("NewResolvedPath(%q) unexpected error: %v", tt.input, err)
				} else if string(path) != tt.want {
					t.Errorf("NewResolvedPath(%q) = %q, want %q", tt.input, path, tt.want)
				}
			}
		})
	}
}

func TestResolvedPath_String(t *testing.T) {
	path, err := NewResolvedPath("/tmp/test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if path.String() != "/tmp/test" {
		t.Errorf("String() = %q, want %q", path.String(), "/tmp/test")
	}
}

// --- Role ---

func TestRole_IsValid(t *testing.T) {
	tests := []struct {
		name  string
		role  Role
		valid bool
	}{
		{"user", RoleUser, true},
		{"assistant", RoleAssistant, true},
		{"tool", RoleTool, true},
		{"system", RoleSystem, true},
		{"empty", Role(""), false},
		{"unknown", Role("unknown"), false},
		{"capitalized", Role("User"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.role.IsValid(); got != tt.valid {
				t.Errorf("Role(%q).IsValid() = %v, want %v", tt.role, got, tt.valid)
			}
		})
	}
}

// --- Harness ---

func TestProvider_IsValid(t *testing.T) {
	tests := []struct {
		name     string
		provider Harness
		valid    bool
	}{
		{"claude-code", HarnessClaudeCode, true},
		{"gemini-cli", HarnessGeminiCLI, true},
		{"codex", HarnessCodex, true},
		{"opencode", HarnessOpenCode, true},
		{"empty", Harness(""), false},
		{"unknown", Harness("chatgpt"), false},
		{"capitalized", Harness("Claude"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.provider.IsKnown(); got != tt.valid {
				t.Errorf("Harness(%q).IsValid() = %v, want %v", tt.provider, got, tt.valid)
			}
		})
	}
}

// --- EntryType ---

func TestEntryType_IsValid(t *testing.T) {
	t.Parallel()

	cases := loadEntryTypeValidationFixtures(t)
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			entry := EntryType(tc.Entry)
			if got := entry.IsValid(); got != tc.Valid {
				t.Errorf("EntryType(%q).IsValid() = %v, want %v", entry, got, tc.Valid)
			}
		})
	}
}

type entryTypeValidationFixture struct {
	Name  string `yaml:"name"`
	Entry string `yaml:"entry"`
	Valid bool   `yaml:"valid"`
}

//go:embed testdata/entry_type_validation.yaml
var entryTypeValidationFixtureYAML []byte

func loadEntryTypeValidationFixtures(t *testing.T) []entryTypeValidationFixture {
	t.Helper()
	cases, err := decodeEntryTypeValidationFixtures(entryTypeValidationFixtureYAML)
	if err != nil {
		t.Fatalf("parse entry type validation fixture: %v", err)
	}
	const expectedRows = 13
	if len(cases) != expectedRows {
		t.Fatalf("entry type validation fixture has %d rows, want %d", len(cases), expectedRows)
	}
	return cases
}

func decodeEntryTypeValidationFixtures(data []byte) ([]entryTypeValidationFixture, error) {
	var cases []entryTypeValidationFixture
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cases); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("entry type validation fixture must contain exactly one YAML document")
	}
	seen := make(map[string]bool, len(cases))
	for index, fixtureCase := range cases {
		if strings.TrimSpace(fixtureCase.Name) == "" || seen[fixtureCase.Name] {
			return nil, fmt.Errorf("entry type validation fixture row %d needs a non-empty unique name", index)
		}
		seen[fixtureCase.Name] = true
	}
	return cases, nil
}

func TestEntryTypeValidationFixturesRejectUnknownFields(t *testing.T) {
	mutated := bytes.Replace(entryTypeValidationFixtureYAML, []byte("valid: true"), []byte("valid: true\n  unknown: rejected"), 1)
	if _, err := decodeEntryTypeValidationFixtures(mutated); err == nil || !strings.Contains(err.Error(), "field unknown") {
		t.Fatalf("unknown fixture field error = %v, want strict field rejection", err)
	}
}

func TestEntryType_String(t *testing.T) {
	t.Parallel()
	if got := EntryTypeText.String(); got != "text" {
		t.Errorf("EntryTypeText.String() = %q, want %q", got, "text")
	}
	if got := EntryTypeToolUse.String(); got != "tool_use" {
		t.Errorf("EntryTypeToolUse.String() = %q, want %q", got, "tool_use")
	}
}

// --- SelectionMatcher ---

func TestSelectionMatcher_MatchesByGitRemote(t *testing.T) {
	t.Parallel()
	m := NewSelectionMatcherBuilder().
		AddProject(string(HarnessClaudeCode), "git@github.com:user/repo.git", "").
		Build()

	// Same git remote → selected.
	if !m.Matches(HarnessClaudeCode, "git@github.com:user/repo.git", "any-project", "00000000-0000-0000-0000-000000000001") {
		t.Error("expected session with matching git remote to be selected")
	}
	// Different git remote → not selected.
	if m.Matches(HarnessClaudeCode, "git@github.com:user/other.git", "any-project", "00000000-0000-0000-0000-000000000002") {
		t.Error("expected session with non-matching git remote to not be selected")
	}
	// Empty git remote on session → not selected (cannot match).
	if m.Matches(HarnessClaudeCode, "", "any-project", "00000000-0000-0000-0000-000000000003") {
		t.Error("expected session with empty git remote to not be selected by remote rule")
	}
}

// TestSelectionMatcher_MatchesByGitRemote_SSHConfigVsNormalizedStoredForm
// regression-locks selection matching when kickstart persists remotes in whatever form
// `git remote -v` showed (SSH/SCP here), but the projects table stores the
// bare normalized form. Both must be treated as identifying the same project.
func TestSelectionMatcher_MatchesByGitRemote_SSHConfigVsNormalizedStoredForm(t *testing.T) {
	t.Parallel()
	m := NewSelectionMatcherBuilder().
		AddProject(string(HarnessClaudeCode), "git@github.com:example-org/garden-app.git", "").
		Build()

	// The stored, already-normalized form (no scheme) must match the SSH-form config rule.
	if !m.Matches(HarnessClaudeCode, "github.com/example-org/garden-app", "any-project", "00000000-0000-0000-0000-000000000004") {
		t.Error("expected the normalized stored remote form to match an SSH-form config rule for the same repo")
	}
	// A different repo, even in the same normalized form family, must not match.
	if m.Matches(HarnessClaudeCode, "github.com/example-org/other-repo", "any-project", "00000000-0000-0000-0000-000000000005") {
		t.Error("expected a different repo's normalized remote to not match")
	}
}

// TestSelectionMatcher_MatchesByProjectName_PathShapedStoredName
// regression-locks a real-data selection-matching regression: several store readers populate their row's
// "project name" as the session's full working-directory PATH (the projects
// table has no separate short-name column); a short config name must still
// match a path whose final segment is that name.
func TestSelectionMatcher_MatchesByProjectName_PathShapedStoredName(t *testing.T) {
	t.Parallel()
	m := NewSelectionMatcherBuilder().
		AddProject(string(HarnessClaudeCode), "", "sample-project").
		Build()

	if !m.Matches(HarnessClaudeCode, "", "/home/developer/work/sample-project", "00000000-0000-0000-0000-000000000006") {
		t.Error("expected a path-shaped stored project name to match a short config name naming its final segment")
	}
	// A different project's path must not match.
	if m.Matches(HarnessClaudeCode, "", "/home/developer/work/other-project", "00000000-0000-0000-0000-000000000007") {
		t.Error("expected a different project's path to not match")
	}
}

func TestSelectionMatcher_MatchesByProjectName(t *testing.T) {
	t.Parallel()
	m := NewSelectionMatcherBuilder().
		AddProject(string(HarnessClaudeCode), "", "my-project").
		Build()

	// Matching project name → selected.
	if !m.Matches(HarnessClaudeCode, "", "my-project", "00000000-0000-0000-0000-000000000001") {
		t.Error("expected session with matching project name to be selected")
	}
	// Different project name → not selected.
	if m.Matches(HarnessClaudeCode, "", "other-project", "00000000-0000-0000-0000-000000000002") {
		t.Error("expected session with non-matching project name to not be selected")
	}
}

func TestSelectionMatcher_NoMatchWhenProviderMissing(t *testing.T) {
	t.Parallel()
	// Only claude is configured.
	m := NewSelectionMatcherBuilder().
		AddProject(string(HarnessClaudeCode), "", "any").
		Build()

	// opencode session → not selected (provider not in config).
	if m.Matches(HarnessOpenCode, "", "any", "00000000-0000-0000-0000-000000000001") {
		t.Error("expected opencode session to not be selected when only claude is configured")
	}
}

func TestSelectionMatcher_EmptyProviderMatchesAll(t *testing.T) {
	t.Parallel()
	// Harness present but no projects/sessions restrictions.
	m := NewSelectionMatcherBuilder().
		AddHarness(string(HarnessClaudeCode)).
		Build()

	// Any session from claude should be selected.
	if !m.Matches(HarnessClaudeCode, "git@github.com:whatever/repo.git", "any-project", "00000000-0000-0000-0000-000000000001") {
		t.Error("expected all claude sessions to be selected when provider has no restrictions")
	}
	if !m.Matches(HarnessClaudeCode, "", "", "00000000-0000-0000-0000-000000000002") {
		t.Error("expected claude session with empty fields to be selected when provider has no restrictions")
	}
	// But opencode still not selected.
	if m.Matches(HarnessOpenCode, "", "", "00000000-0000-0000-0000-000000000003") {
		t.Error("expected opencode session to not be selected")
	}
}

func TestSelectionMatcher_MatchesByExplicitSessionID(t *testing.T) {
	t.Parallel()
	specificSID := SessionID("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	m := NewSelectionMatcherBuilder().
		AddSession(string(HarnessClaudeCode), string(specificSID)).
		Build()

	// Explicit session ID match.
	if !m.Matches(HarnessClaudeCode, "", "any", specificSID) {
		t.Error("expected explicitly listed session to be selected")
	}
	// Different session ID → not selected.
	if m.Matches(HarnessClaudeCode, "", "any", "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb") {
		t.Error("expected non-listed session to not be selected")
	}
}

func TestPruneSessionRow_IsSelectedBy(t *testing.T) {
	t.Parallel()
	m := NewSelectionMatcherBuilder().
		AddProject(string(HarnessClaudeCode), "git@github.com:user/repo.git", "").
		Build()

	selected := PruneSessionRow{
		SessionID: "11111111-1111-1111-1111-111111111111",
		Harness:   HarnessClaudeCode,
		GitRemote: "git@github.com:user/repo.git",
	}
	if !selected.IsSelectedBy(m) {
		t.Error("expected selected row to match")
	}

	unselected := PruneSessionRow{
		SessionID:   "22222222-2222-2222-2222-222222222222",
		Harness:     HarnessOpenCode,
		ProjectName: "some-project",
	}
	if unselected.IsSelectedBy(m) {
		t.Error("expected unselected row to not match")
	}
}

func TestSelectionMatcher_MatchBranch(t *testing.T) {
	t.Parallel()

	const sid = "00000000-0000-0000-0000-000000000001"

	t.Run("harness not in selection -> No", func(t *testing.T) {
		m := NewSelectionMatcherBuilder().AddProject(string(HarnessClaudeCode), "", "p").Build()
		if got := m.MatchBranch(HarnessOpenCode, "", "p", "main", sid); got != BranchMatchNo {
			t.Errorf("got %v, want BranchMatchNo", got)
		}
	})

	t.Run("no-restriction harness -> Yes (any branch)", func(t *testing.T) {
		m := NewSelectionMatcherBuilder().AddHarness(string(HarnessClaudeCode)).Build()
		if got := m.MatchBranch(HarnessClaudeCode, "r", "p", "anything", sid); got != BranchMatchYes {
			t.Errorf("got %v, want BranchMatchYes", got)
		}
	})

	t.Run("explicit session allowlist -> Yes (branch-independent)", func(t *testing.T) {
		m := NewSelectionMatcherBuilder().AddSession(string(HarnessClaudeCode), sid).Build()
		if got := m.MatchBranch(HarnessClaudeCode, "r", "p", "any-branch", sid); got != BranchMatchYes {
			t.Errorf("got %v, want BranchMatchYes", got)
		}
	})

	t.Run("empty branch set = all branches -> Yes", func(t *testing.T) {
		m := NewSelectionMatcherBuilder().AddProject(string(HarnessClaudeCode), "git@github.com:u/r.git", "").Build()
		if got := m.MatchBranch(HarnessClaudeCode, "git@github.com:u/r.git", "", "whatever", sid); got != BranchMatchYes {
			t.Errorf("got %v, want BranchMatchYes", got)
		}
	})

	t.Run("branch in set -> Yes; not in set -> No", func(t *testing.T) {
		m := NewSelectionMatcherBuilder().AddProject(string(HarnessClaudeCode), "git@github.com:u/r.git", "", "main", "dev").Build()
		if got := m.MatchBranch(HarnessClaudeCode, "git@github.com:u/r.git", "", "main", sid); got != BranchMatchYes {
			t.Errorf("main: got %v, want BranchMatchYes", got)
		}
		if got := m.MatchBranch(HarnessClaudeCode, "git@github.com:u/r.git", "", "feature", sid); got != BranchMatchNo {
			t.Errorf("feature: got %v, want BranchMatchNo", got)
		}
	})

	t.Run("unknown branch under restricted project -> Yes (conservative)", func(t *testing.T) {
		m := NewSelectionMatcherBuilder().AddProject(string(HarnessClaudeCode), "git@github.com:u/r.git", "", "main").Build()
		if got := m.MatchBranch(HarnessClaudeCode, "git@github.com:u/r.git", "", "", sid); got != BranchMatchYes {
			t.Errorf("unknown branch: got %v, want BranchMatchYes", got)
		}
	})

	// Multi-project: a session matching two project entries (one by remote, one
	// by name) with different branch sets.
	multi := NewSelectionMatcherBuilder().
		AddProject(string(HarnessClaudeCode), "git@github.com:u/r.git", "", "main"). // by remote, allows main
		AddProject(string(HarnessClaudeCode), "", "proj", "dev").                    // by name, allows dev
		Build()

	t.Run("multi-project both admit -> Yes", func(t *testing.T) {
		both := NewSelectionMatcherBuilder().
			AddProject(string(HarnessClaudeCode), "git@github.com:u/r.git", "", "main").
			AddProject(string(HarnessClaudeCode), "", "proj", "main").
			Build()
		if got := both.MatchBranch(HarnessClaudeCode, "git@github.com:u/r.git", "proj", "main", sid); got != BranchMatchYes {
			t.Errorf("got %v, want BranchMatchYes", got)
		}
	})

	t.Run("multi-project disagree -> WithheldConflict", func(t *testing.T) {
		// remote-project admits main; name-project (dev only) rejects main -> conflict.
		if got := multi.MatchBranch(HarnessClaudeCode, "git@github.com:u/r.git", "proj", "main", sid); got != BranchMatchWithheldConflict {
			t.Errorf("got %v, want BranchMatchWithheldConflict", got)
		}
	})

	t.Run("multi-project both reject -> No", func(t *testing.T) {
		if got := multi.MatchBranch(HarnessClaudeCode, "git@github.com:u/r.git", "proj", "feature", sid); got != BranchMatchNo {
			t.Errorf("got %v, want BranchMatchNo", got)
		}
	})
}

// --- SessionOutcome ---

func TestSessionOutcome_IsValid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		outcome SessionOutcome
		valid   bool
	}{
		{"resolved", OutcomeResolved, true},
		{"partial", OutcomePartial, true},
		{"failed", OutcomeFailed, true},
		{"empty", SessionOutcome(""), false},
		{"unknown", SessionOutcome("unknown"), false},
		{"capitalized", SessionOutcome("Resolved"), false},
		{"mixed_case", SessionOutcome("FAILED"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.outcome.IsValid(); got != tt.valid {
				t.Errorf("SessionOutcome(%q).IsValid() = %v, want %v", tt.outcome, got, tt.valid)
			}
		})
	}
}
