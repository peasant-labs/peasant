package ingest_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/commit_history_capture.yaml
var commitHistoryCaptureYAML []byte

type commitHistoryCaptureFixture struct {
	RequiredCaseNames []string `yaml:"required_case_names"`
	Cases             []struct {
		Name         string    `yaml:"name"`
		SessionStart time.Time `yaml:"session_start"`
		Transcript   string    `yaml:"transcript"`
		WantMessages []string  `yaml:"want_messages"`
	} `yaml:"cases"`
}

func loadCommitHistoryCaptureFixtures(t *testing.T) commitHistoryCaptureFixture {
	t.Helper()
	var fixture commitHistoryCaptureFixture
	if err := yaml.Unmarshal(commitHistoryCaptureYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	present := make(map[string]bool)
	for _, tc := range fixture.Cases {
		if present[tc.Name] {
			t.Fatalf("duplicate history capture case %q", tc.Name)
		}
		present[tc.Name] = true
	}
	if err := testutil.RequireFixtureNames("commit history capture", "case", fixture.RequiredCaseNames, present); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// buildRewrittenHistory leaves HEAD on integration, which retains A/B even
// though the recorded feature ref has been squashed. All Git state is synthetic.
func buildRewrittenHistory(t *testing.T) (string, map[string]string) {
	t.Helper()
	dir := t.TempDir()
	mustGitCmd(t, dir, "git", "init", "-q", "-b", "main")
	mustGitCmd(t, dir, "git", "config", "user.email", testutil.TestEmail)
	mustGitCmd(t, dir, "git", "config", "user.name", "Test User")
	mustGitCmd(t, dir, "git", "config", "commit.gpgsign", "false")
	commit := func(message, date, email string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "work.txt"), []byte(message+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		mustGitCmd(t, dir, "git", "add", "work.txt")
		mustGitCmdEnv(t, dir, []string{"GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date, "GIT_AUTHOR_EMAIL=" + email}, "git", "commit", "-q", "-m", message)
		out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	commit("base", "2024-02-01T12:00:00Z", testutil.TestEmail)
	mustGitCmd(t, dir, "git", "checkout", "-q", "-b", "feature")
	commit("outside window", "2024-02-02T12:00:00Z", testutil.TestEmail)
	hashes := make(map[string]string)
	hashes["session A"] = commit("session A", "2024-02-19T12:01:00Z", testutil.TestEmail)
	hashes["session B"] = commit("session B", "2024-02-19T12:02:00Z", testutil.TestEmail)
	commit("another author", "2024-02-19T12:03:00Z", "other@example.com")
	mustGitCmd(t, dir, "git", "branch", "integration")
	mustGitCmd(t, dir, "git", "checkout", "-q", "main")
	// Move only the synthetic feature ref; do not rewrite the test worktree.
	mustGitCmd(t, dir, "git", "branch", "-f", "feature", "main")
	mustGitCmd(t, dir, "git", "checkout", "-q", "feature")
	mustGitCmd(t, dir, "git", "merge", "--squash", "integration")
	commit("squashed C", "2024-02-20T12:00:00Z", testutil.TestEmail)
	mustGitCmd(t, dir, "git", "checkout", "-q", "integration")
	for message, hash := range hashes {
		err := exec.Command("git", "-C", dir, "merge-base", "--is-ancestor", hash, "refs/heads/feature").Run()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			t.Fatalf("%s reachability = %v, want a successful negative ancestry answer", message, err)
		}
	}
	return dir, hashes
}

func TestPipeline_CommitHistoryCapture(t *testing.T) {
	for _, tc := range loadCommitHistoryCaptureFixtures(t).Cases {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			repo, hashes := buildRewrittenHistory(t)
			root := t.TempDir()
			source := filepath.Join(root, "session.jsonl")
			if err := os.WriteFile(source, []byte(tc.Transcript+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			session := makeDiscoveredSession(t, testSessionID, source, tc.SessionStart)
			session.TranscriptOrigin = ingest.TranscriptOriginFile
			database := openEvidenceStore(t, filepath.Join(root, "peasant.db"))
			output := filepath.Join(root, "output")
			cfg := makePipelineConfig(output)
			cfg.Force = true
			run := func(wantMessages []string) []ingest.CurrentCommitAssociation {
				t.Helper()
				meta := makeMinimalMeta(t, testSessionID)
				meta.Source.Format = ingest.SourceFormatJSONL
				meta.Source.FilePath = source
				meta.Git.Worktree = &repo
				meta.Project.FilePath = repo
				branch := "feature"
				meta.Git.Branch = &branch
				meta.Timestamp.Start = tc.SessionStart.UnixMilli()
				meta.Timestamp.End = tc.SessionStart.Add(time.Hour).UnixMilli()
				adapters := map[ingest.Harness]ingest.AdapterFactory{
					ingest.HarnessClaudeCode: makeStubAdapter([]ingest.DiscoveredSession{session}, map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta}),
				}
				pipeline, err := ingest.NewPipeline(&ingest.OSFileSystem{}, testutil.DefaultGitResolver(), adapters, cfg,
					ingest.WithGitDiffAnalyzer(ingest.NewExecGitDiffAnalyzer()), ingest.WithStore(database))
				if err != nil {
					t.Fatal(err)
				}
				result, err := pipeline.Run(ctx)
				if err != nil || result.Summary.Errors != 0 || result.Summary.StoreError != nil {
					t.Fatalf("ingestion failed: %v, result: %+v", err, result)
				}
				data, err := os.ReadFile(filepath.Join(expectedOutputBase(output, testSessionID), testSessionID+"--metadata.json"))
				if err != nil {
					t.Fatal(err)
				}
				var written ingest.UnifiedMetadata
				if err := json.Unmarshal(data, &written); err != nil {
					t.Fatal(err)
				}
				gotMessages := make([]string, 0, len(written.Git.Commits))
				for _, c := range written.Git.Commits {
					gotMessages = append(gotMessages, c.Message)
					if c.Hash != hashes[c.Message] {
						t.Errorf("%s hash = %s, want original %s", c.Message, c.Hash, hashes[c.Message])
					}
				}
				if !slices.Equal(gotMessages, wantMessages) {
					t.Errorf("metadata subjects = %v, want exactly %v", gotMessages, wantMessages)
				}
				associations, err := database.ListCurrentSessionCommitAssociations(ctx, session.SessionID)
				if err != nil {
					t.Fatal(err)
				}
				gotHashes := make([]string, 0, len(associations))
				for _, a := range associations {
					if err := a.ID.Validate(); err != nil {
						t.Fatal(err)
					}
					gotHashes = append(gotHashes, a.ObservedCommitHash)
				}
				wantHashes := make([]string, 0, len(wantMessages))
				for _, message := range wantMessages {
					wantHashes = append(wantHashes, hashes[message])
				}
				slices.Sort(wantHashes)
				if !slices.Equal(gotHashes, wantHashes) {
					t.Fatalf("stored observations = %v, want exactly %v", gotHashes, wantHashes)
				}
				return associations
			}
			first := run(tc.WantMessages)
			// Removing the current projection must not destroy the historical IDs.
			mustGitCmd(t, repo, "git", "checkout", "-q", "main")
			run(nil)
			mustGitCmd(t, repo, "git", "checkout", "-q", "integration")
			replayed := run(tc.WantMessages)
			if !reflect.DeepEqual(first, replayed) {
				t.Fatalf("re-ingestion changed durable identities: before %+v, after %+v", first, replayed)
			}
		})
	}
}
