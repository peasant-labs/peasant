package ingest

import (
	_ "embed"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/duplicate_sessions.yaml
var duplicateSessionFixtureYAML []byte

type duplicateSessionFixture struct {
	RequiredNames []string `yaml:"required_names"`
	Cases         []struct {
		Name       string `yaml:"name"`
		Harness    string `yaml:"harness"`
		OutOfScope bool   `yaml:"out_of_scope"`
		Refused    bool   `yaml:"refused"`
	} `yaml:"cases"`
}

func loadDuplicateSessionFixture(t *testing.T) duplicateSessionFixture {
	t.Helper()
	var fixture duplicateSessionFixture
	if err := yaml.Unmarshal(duplicateSessionFixtureYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(fixture.Cases))
	for _, c := range fixture.Cases {
		names = append(names, c.Name)
	}
	for _, required := range fixture.RequiredNames {
		if !slices.Contains(names, required) {
			t.Fatalf("fixture lost required case %q", required)
		}
	}
	return fixture
}

// TestRefuseDuplicateSessionIDsScope proves only file-per-session harnesses
// are refused, a duplicate outside the run's scope is left alone, and the
// refusal names the newer and larger file and says not to delete either.
func TestRefuseDuplicateSessionIDsScope(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	small, big := filepath.Join(dir, "a.jsonl"), filepath.Join(dir, "b.jsonl")
	if err := os.WriteFile(small, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(big, []byte("xxxxxxx"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	pair := func(h Harness) []DiscoveredSession {
		return []DiscoveredSession{
			{Harness: h, SessionID: "s1", SourcePath: ResolvedPath(small), ModTime: now},
			{Harness: h, SessionID: "s1", SourcePath: ResolvedPath(big), ModTime: now.Add(-time.Hour)},
		}
	}
	fixture := loadDuplicateSessionFixture(t)
	for _, c := range fixture.Cases {
		var inScope func(DiscoveredSession) bool
		if c.OutOfScope {
			inScope = func(DiscoveredSession) bool { return false }
		}
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			kept, refused := refuseDuplicateSessionIDs(&OSFileSystem{}, pair(Harness(c.Harness)), inScope)
			if got := len(refused) == 1; got != c.Refused {
				t.Fatalf("refused = %v, want %v", refused, c.Refused)
			}
			if !c.Refused {
				if len(kept) != 2 {
					t.Fatalf("kept %d sessions, want both", len(kept))
				}
				return
			}
			msg := refused[0].Error.Error()
			for _, want := range []string{"newer: " + small, "larger: " + big, "do not delete"} {
				if !strings.Contains(msg, want) {
					t.Fatalf("message %q lacks %q", msg, want)
				}
			}
		})
	}
}

// TestRefuseDuplicateSessionIDsLeavesOutChildrenOfARefusedParent proves a
// subagent of a refused session is not kept to be stored on its own.
func TestRefuseDuplicateSessionIDsLeavesOutChildrenOfARefusedParent(t *testing.T) {
	t.Parallel()
	parent := SessionID("s1")
	sessions := []DiscoveredSession{
		{Harness: HarnessClaudeCode, SessionID: parent, SourcePath: "/a/s1.jsonl"},
		{Harness: HarnessClaudeCode, SessionID: parent, SourcePath: "/b/s1.jsonl"},
		{Harness: HarnessClaudeCode, SessionID: "child", ParentUUID: &parent, SourcePath: "/a/s1/subagents/agent-child.jsonl"},
		{Harness: HarnessClaudeCode, SessionID: "other", SourcePath: "/a/other.jsonl"},
	}
	kept, refused := refuseDuplicateSessionIDs(nil, sessions, nil)
	if len(refused) != 1 || len(kept) != 1 || kept[0].SessionID != "other" {
		t.Fatalf("kept %+v refused %d, want only the unrelated session kept and one refusal", kept, len(refused))
	}
}
