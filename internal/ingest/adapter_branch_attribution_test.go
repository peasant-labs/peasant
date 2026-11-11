package ingest_test

import (
	"context"
	_ "embed"
	"fmt"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/adapter_branch_attribution.yaml
var adapterBranchAttributionData []byte

type adapterBranchCase struct {
	Name              string `yaml:"name"`
	Harness           string `yaml:"harness"`
	RecordedBranch    string `yaml:"recordedBranch"`
	Checkout          string `yaml:"checkout"`
	WantBranch        string `yaml:"wantBranch"`
	WantCheckoutCalls int    `yaml:"wantCheckoutCalls"`
}

// TestAdapterMetadataBranchAttribution runs each harness's real metadata
// extraction over a recording and a git double whose checked-out branch differs
// from the recorded one.
func TestAdapterMetadataBranchAttribution(t *testing.T) {
	t.Parallel()
	var fixtures struct {
		RequiredNames []string            `yaml:"requiredNames"`
		Cases         []adapterBranchCase `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(adapterBranchAttributionData, &fixtures); err != nil {
		t.Fatalf("decode adapter branch attribution fixture: %v", err)
	}
	present := make(map[string]bool, len(fixtures.Cases))
	for _, c := range fixtures.Cases {
		present[c.Name] = true
	}
	if err := testutil.RequireFixtureNames("internal/ingest/testdata/adapter_branch_attribution.yaml", "case", fixtures.RequiredNames, present); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixtures.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			stub := testutil.DefaultGitResolver()
			stub.BranchName = c.Checkout
			git := &checkoutCountingGit{StubGitResolver: stub}
			var stored *string
			switch ingest.Harness(c.Harness) {
			case ingest.HarnessClaudeCode:
				stored = extractClaudeBranch(t, git, c)
			case ingest.HarnessCursor:
				stored = extractCursorBranch(t, git, c)
			case ingest.HarnessOpenCode:
				stored = extractOpenCodeBranch(t, git, c)
			case ingest.HarnessPi:
				stored = extractPiBranch(t, git, c)
			default:
				t.Fatalf("fixture case %q names the harness %q, which this test does not drive", c.Name, c.Harness)
			}
			got := ""
			if stored != nil {
				got = *stored
			}
			if got != c.WantBranch {
				t.Fatalf("stored branch = %q, want %q", got, c.WantBranch)
			}
			if git.calls != c.WantCheckoutCalls {
				t.Fatalf("checked-out branch asked %d times, want %d", git.calls, c.WantCheckoutCalls)
			}
		})
	}
}

func extractClaudeBranch(t *testing.T, git ingest.GitResolver, c adapterBranchCase) *string {
	t.Helper()
	const base, slug = "/home/test/.claude/projects", "-home-test-project"
	mfs := testutil.NewMemFS()
	data := sampleClaudeJSONL(testutil.TestSessionUUID, "2.1.14", "/home/test/project", c.RecordedBranch)
	path := setupClaudeFS(t, mfs, base, slug, testutil.TestSessionUUID, data)
	sid, err := ingest.NewSessionID(testutil.TestSessionUUID)
	if err != nil {
		t.Fatal(err)
	}
	session := ingest.DiscoveredSession{
		SessionID:     sid,
		Harness:       ingest.HarnessClaudeCode,
		SourcePath:    ingest.ResolvedPath(path),
		SourceFormat:  ingest.SourceFormatJSONL,
		SubagentPaths: []ingest.ResolvedPath{},
		DebugPaths:    []ingest.ResolvedPath{},
	}
	meta, err := ingest.NewClaudeAdapter(mfs, git, salt.Salt{}).ExtractMetadata(context.Background(), session)
	if err != nil {
		t.Fatalf("ExtractMetadata: %v", err)
	}
	return meta.Git.Branch
}

func extractCursorBranch(t *testing.T, git ingest.GitResolver, c adapterBranchCase) *string {
	t.Helper()
	const path = "/cursor/branch.jsonl"
	mfs := testutil.NewMemFS()
	if err := mfs.WriteFile(path, sampleCursorJSONL(), 0o644); err != nil {
		t.Fatal(err)
	}
	sid, err := ingest.NewSessionID(testutil.TestSessionUUID)
	if err != nil {
		t.Fatal(err)
	}
	session := ingest.DiscoveredSession{
		SessionID:     sid,
		Harness:       ingest.HarnessCursor,
		SourcePath:    ingest.ResolvedPath(path),
		SourceFormat:  ingest.SourceFormatJSONL,
		CWD:           testutil.TestDefaultWorktreeDir,
		Branch:        c.RecordedBranch,
		SubagentPaths: []ingest.ResolvedPath{},
		DebugPaths:    []ingest.ResolvedPath{},
	}
	meta, err := ingest.NewCursorAdapter(mfs, git, salt.Salt{}).ExtractMetadata(context.Background(), session)
	if err != nil {
		t.Fatalf("ExtractMetadata: %v", err)
	}
	return meta.Git.Branch
}

func extractOpenCodeBranch(t *testing.T, git ingest.GitResolver, c adapterBranchCase) *string {
	t.Helper()
	const (
		root      = "/opencode"
		projHash  = "projhashBranch"
		sesID     = "ses_branchABCXYZ123"
		directory = "/home/test/project"
	)
	fs := testutil.NewMemFS()
	populateOpenCodeStorage(t, fs, root, projHash, directory, sesID, directory, "", 1770000000000, 1770000100000, nil)
	adapter := newTestOpenCodeAdapter(fs, git, salt.Salt{})
	sessions, err := adapter.Discover(context.Background(), ingest.SourceConfig{Paths: []ingest.ResolvedPath{ingest.ResolvedPath(root)}, Enabled: true})
	if err != nil || len(sessions) != 1 {
		t.Fatalf("Discover returned %d sessions, error %v", len(sessions), err)
	}
	sessions[0].Branch = c.RecordedBranch
	meta, err := adapter.ExtractMetadata(context.Background(), sessions[0])
	if err != nil {
		t.Fatalf("ExtractMetadata: %v", err)
	}
	return meta.Git.Branch
}

func extractPiBranch(t *testing.T, git ingest.GitResolver, c adapterBranchCase) *string {
	t.Helper()
	const path = "/pi/branch.jsonl"
	fs := testutil.NewMemFS()
	header := fmt.Sprintf(`{"type":"session","version":3,"id":%q,"cwd":"/workspace","timestamp":"2026-01-01T00:00:00Z"}`, testutil.TestSessionUUID)
	if err := fs.WriteFile(path, []byte(header+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sid, err := ingest.NewSessionID(testutil.TestSessionUUID)
	if err != nil {
		t.Fatal(err)
	}
	session := ingest.DiscoveredSession{
		SessionID:     sid,
		Harness:       ingest.HarnessPi,
		SourcePath:    ingest.ResolvedPath(path),
		SourceFormat:  ingest.SourceFormatJSONL,
		Branch:        c.RecordedBranch,
		SubagentPaths: []ingest.ResolvedPath{},
		DebugPaths:    []ingest.ResolvedPath{},
	}
	meta, err := ingest.NewPiAdapter(fs, git, salt.Salt{}).ExtractMetadata(context.Background(), session)
	if err != nil {
		t.Fatalf("ExtractMetadata: %v", err)
	}
	return meta.Git.Branch
}
