package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/commit_detector_branch.yaml
var commitDetectorBranchFixtureYAML []byte

type commitDetectorBranchFixture struct {
	RequiredCaseNames []string                   `yaml:"required_case_names"`
	Cases             []commitDetectorBranchCase `yaml:"cases"`
}

type commitDetectorBranchCase struct {
	Name                 string          `yaml:"name"`
	SessionBranch        string          `yaml:"session_branch"`
	UserEmailMissing     bool            `yaml:"user_email_missing"`
	Candidates           []string        `yaml:"candidates"`
	Ancestry             map[string]bool `yaml:"ancestry"`
	AncestryError        string          `yaml:"ancestry_error"`
	AncestryErrorOnQuery int             `yaml:"ancestry_error_on_query"`
	WantHashes           []string        `yaml:"want_hashes"`
	WantErrorTypes       []string        `yaml:"want_error_types"`
	WantAncestryQueries  int             `yaml:"want_ancestry_queries"`
}

// decodeStrictYAML decodes exactly one YAML document into out, rejecting
// unknown fields and trailing documents. Both fixture families in this file
// share it.
func decodeStrictYAML(t *testing.T, data []byte, label string, out any) {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(out); err != nil {
		t.Fatalf("decode %s: %v", label, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("%s must contain exactly one YAML document: %v", label, err)
	}
}

func loadCommitDetectorBranchFixture(t *testing.T) commitDetectorBranchFixture {
	t.Helper()
	var fixture commitDetectorBranchFixture
	decodeStrictYAML(t, commitDetectorBranchFixtureYAML, "branch-aware commit fixture", &fixture)
	present := make(map[string]bool, len(fixture.Cases))
	for _, tc := range fixture.Cases {
		if present[tc.Name] {
			t.Fatalf("branch-aware commit fixture repeats case %q", tc.Name)
		}
		present[tc.Name] = true
	}
	if err := testutil.RequireFixtureNames("branch-aware commit fixture", "case", fixture.RequiredCaseNames, present); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// branchFixtureAnalyzer builds the stub the way every harness in this family
// needs it: the candidates become the window's commits, ancestry answers
// reachability against refs/heads/<branch>, and ancestry_error makes every
// reachability check fail with the named cause.
func branchFixtureAnalyzer(t *testing.T, tc commitDetectorBranchCase) *testutil.StubGitDiffAnalyzer {
	t.Helper()
	infos := make([]ingest.CommitInfo, 0, len(tc.Candidates))
	for _, hash := range tc.Candidates {
		infos = append(infos, testCommitFixture(hash, "commit "+hash))
	}
	ancestry := make(map[string]bool, len(tc.Ancestry))
	for hash, reachable := range tc.Ancestry {
		ancestry[hash+"@refs/heads/"+tc.SessionBranch] = reachable
	}
	stub := &testutil.StubGitDiffAnalyzer{CommitInfos: infos, Ancestry: ancestry}
	stub.IsAncestorErrOnQuery = tc.AncestryErrorOnQuery
	switch tc.AncestryError {
	case "":
	case "timeout":
		stub.IsAncestorErr = context.DeadlineExceeded
	case "unknown_ref":
		stub.IsAncestorErr = errors.New("git merge-base --is-ancestor in /repo: exit status 128: fatal: Not a valid object name refs/heads/" + tc.SessionBranch)
	default:
		t.Fatalf("case %q: unknown ancestry_error %q", tc.Name, tc.AncestryError)
	}
	return stub
}

// hashesOf projects the detector's output to the slice the fixtures compare.
func hashesOf(commits []ingest.CommitInfo) []string {
	hashes := make([]string, 0, len(commits))
	for _, c := range commits {
		hashes = append(hashes, c.Hash)
	}
	return hashes
}

// errorTypesOf projects diagnostics to the slice the fixtures compare.
func errorTypesOf(diags []ingest.DiagnosticEntry) []string {
	types := make([]string, 0, len(diags))
	for _, d := range diags {
		types = append(types, d.ErrorType)
	}
	return types
}

// sessionEmailFor returns the git user email a case runs under.
func sessionEmailFor(tc commitDetectorBranchCase) string {
	if tc.UserEmailMissing {
		return ""
	}
	return testutil.TestEmail
}

func TestCommitDetectorBranchFixtureGuards(t *testing.T) {
	loadCommitDetectorBranchFixture(t)
}

// TestTimestampDetection_SessionBranch drives the detector directly: the
// recorded branch narrows the window's candidates to the reachable ones, and
// every failure to answer falls back to the unfiltered window with one
// diagnostic instead of dropping commits.
func TestTimestampDetection_SessionBranch(t *testing.T) {
	fixture := loadCommitDetectorBranchFixture(t)
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			stub := branchFixtureAnalyzer(t, tc)
			cd := ingest.NewCommitDetector(stub, sessionEmailFor(tc), ingest.WithSessionBranch(tc.SessionBranch))

			now := time.Now()
			commits, diags := cd.TimestampDetection(context.Background(), "/repo", now.Add(-time.Hour), now)

			if got := hashesOf(commits); !slices.Equal(got, tc.WantHashes) {
				t.Errorf("commits = %v, want exactly %v", got, tc.WantHashes)
			}
			if got := errorTypesOf(diags); !slices.Equal(got, tc.WantErrorTypes) {
				t.Errorf("diagnostic types = %v, want exactly %v (%+v)", got, tc.WantErrorTypes, diags)
			}
			for _, d := range diags {
				if d.Remediation == "" {
					t.Errorf("diagnostic %q has no remediation", d.ErrorType)
				}
			}
			if got := stub.AncestorQueries(); got != tc.WantAncestryQueries {
				t.Errorf("IsAncestor was asked %d times, want %d", got, tc.WantAncestryQueries)
			}
		})
	}
}

//go:embed testdata/commit_detector_branch_repo.yaml
var commitDetectorBranchRepoFixtureYAML []byte

type commitDetectorBranchRepoFixture struct {
	RequiredCaseNames []string                       `yaml:"required_case_names"`
	Cases             []commitDetectorBranchRepoCase `yaml:"cases"`
}

type commitDetectorBranchRepoCase struct {
	Name           string   `yaml:"name"`
	SessionBranch  string   `yaml:"session_branch"`
	WantMessages   []string `yaml:"want_messages"`
	WantErrorTypes []string `yaml:"want_error_types"`
}

func loadCommitDetectorBranchRepoFixture(t *testing.T) commitDetectorBranchRepoFixture {
	t.Helper()
	var fixture commitDetectorBranchRepoFixture
	decodeStrictYAML(t, commitDetectorBranchRepoFixtureYAML, "branch-aware commit repository fixture", &fixture)
	present := make(map[string]bool, len(fixture.Cases))
	for _, tc := range fixture.Cases {
		if present[tc.Name] {
			t.Fatalf("branch-aware commit repository fixture repeats case %q", tc.Name)
		}
		present[tc.Name] = true
	}
	if err := testutil.RequireFixtureNames("branch-aware commit repository fixture", "case", fixture.RequiredCaseNames, present); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// buildBranchTopologyRepo builds the topology the repository fixture documents
// and returns its path. HEAD is left on integration so both modifying commits
// are window candidates.
func buildBranchTopologyRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mustGitCmd(t, dir, "git", "init", "-q", "-b", "main")
	mustGitCmd(t, dir, "git", "config", "user.email", testutil.TestEmail)
	mustGitCmd(t, dir, "git", "config", "user.name", "Test User")
	mustGitCmd(t, dir, "git", "config", "commit.gpgsign", "false")

	writeRepoFile(t, dir, "main.txt", "base\n")
	writeRepoFile(t, dir, "feature.txt", "base\n")
	mustGitCmd(t, dir, "git", "add", "main.txt", "feature.txt")
	mustGitCmd(t, dir, "git", "commit", "-q", "-m", "base: add files")

	mustGitCmd(t, dir, "git", "checkout", "-q", "-b", "feature")
	writeRepoFile(t, dir, "feature.txt", "work\n")
	mustGitCmd(t, dir, "git", "commit", "-q", "-a", "-m", "feature: work")

	mustGitCmd(t, dir, "git", "checkout", "-q", "main")
	writeRepoFile(t, dir, "main.txt", "advance\n")
	mustGitCmd(t, dir, "git", "commit", "-q", "-a", "-m", "main: advance")

	mustGitCmd(t, dir, "git", "checkout", "-q", "-b", "integration")
	mustGitCmd(t, dir, "git", "merge", "-q", "--no-ff", "-m", "integration: merge feature", "feature")
	return dir
}

func writeRepoFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func TestCommitDetectorBranchRepoFixtureGuards(t *testing.T) {
	loadCommitDetectorBranchRepoFixture(t)
}

// TestTimestampDetection_SessionBranch_RealGit proves the refs/heads/ anchoring
// and the merge-base call against real git over the fixture's topology.
func TestTimestampDetection_SessionBranch_RealGit(t *testing.T) {
	fixture := loadCommitDetectorBranchRepoFixture(t)
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			dir := buildBranchTopologyRepo(t)
			cd := ingest.NewCommitDetector(ingest.NewExecGitDiffAnalyzer(), testutil.TestEmail, ingest.WithSessionBranch(tc.SessionBranch))

			now := time.Now()
			commits, diags := cd.TimestampDetection(context.Background(), dir, now.Add(-time.Hour), now.Add(time.Hour))

			got := make([]string, 0, len(commits))
			for _, c := range commits {
				got = append(got, c.Message)
			}
			slices.Sort(got)
			want := slices.Clone(tc.WantMessages)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("commit subjects = %v, want exactly %v", got, want)
			}
			if gotTypes := errorTypesOf(diags); !slices.Equal(gotTypes, tc.WantErrorTypes) {
				t.Errorf("diagnostic types = %v, want exactly %v (%+v)", gotTypes, tc.WantErrorTypes, diags)
			}
		})
	}
}
