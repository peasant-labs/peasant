package main

import (
	_ "embed"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/cases.yaml
var casesYAML []byte

type evidenceCase struct {
	Name  string `yaml:"name"`
	Pulls []struct {
		Number         int    `yaml:"number"`
		MergedAt       string `yaml:"merged_at"`
		MergeCommitSHA string `yaml:"merge_commit_sha"`
		HeadSHA        string `yaml:"head_sha"`
	} `yaml:"pulls"`
	Trees     map[string]string `yaml:"trees"`
	CheckRuns []struct {
		Name       string `yaml:"name"`
		Conclusion string `yaml:"conclusion"`
		StartedAt  string `yaml:"started_at"`
		App        string `yaml:"app"`
	} `yaml:"check_runs"`
	APIError   string `yaml:"api_error"`
	WantSkip   bool   `yaml:"want_skip"`
	WantReason string `yaml:"want_reason"`
}

// fakeAPI serves the case's API facts in memory.
type fakeAPI struct {
	spec evidenceCase
}

func (f fakeAPI) pullsForCommit(string) ([]pullRef, error) {
	if f.spec.APIError == "pulls" {
		return nil, fmt.Errorf("pulls unavailable")
	}
	pulls := make([]pullRef, 0, len(f.spec.Pulls))
	for _, pull := range f.spec.Pulls {
		ref := pullRef{Number: pull.Number, MergeCommitSHA: pull.MergeCommitSHA}
		if pull.MergedAt != "" {
			mergedAt := pull.MergedAt
			ref.MergedAt = &mergedAt
		}
		ref.Head.SHA = pull.HeadSHA
		pulls = append(pulls, ref)
	}
	return pulls, nil
}

func (f fakeAPI) commitTree(sha string) (string, error) {
	if f.spec.APIError == "tree" {
		return "", fmt.Errorf("tree unavailable")
	}
	tree, ok := f.spec.Trees[sha]
	if !ok {
		return "", fmt.Errorf("commit %s not found", sha)
	}
	return tree, nil
}

func (f fakeAPI) checkRuns(string) ([]checkRun, error) {
	if f.spec.APIError == "runs" {
		return nil, fmt.Errorf("runs unavailable")
	}
	runs := make([]checkRun, 0, len(f.spec.CheckRuns))
	for _, recorded := range f.spec.CheckRuns {
		run := checkRun{Name: recorded.Name, Conclusion: recorded.Conclusion, StartedAt: recorded.StartedAt}
		run.App.Slug = recorded.App
		runs = append(runs, run)
	}
	return runs, nil
}

func TestProveFromFixture(t *testing.T) {
	var fixture struct {
		Cases []evidenceCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(casesYAML, &fixture); err != nil {
		t.Fatalf("parse testdata/cases.yaml: %v", err)
	}
	if len(fixture.Cases) == 0 {
		t.Fatal("testdata/cases.yaml defines no cases")
	}
	seen := make(map[string]bool, len(fixture.Cases))
	for _, testCase := range fixture.Cases {
		if strings.TrimSpace(testCase.Name) == "" {
			t.Fatal("testdata/cases.yaml has a case without a name")
		}
		if seen[testCase.Name] {
			t.Fatalf("testdata/cases.yaml repeats case %q", testCase.Name)
		}
		seen[testCase.Name] = true
		t.Run(testCase.Name, func(t *testing.T) {
			skip, reason, err := prove(fakeAPI{spec: testCase}, "squash", "make check")
			if skip != testCase.WantSkip {
				t.Fatalf("skip = %t, want %t (reason %q, err %v)", skip, testCase.WantSkip, reason, err)
			}
			if testCase.WantSkip {
				if err != nil || reason != "" {
					t.Fatalf("a proved skip must carry no reason or error; got reason %q, err %v", reason, err)
				}
				return
			}
			if testCase.WantReason != "" && !strings.Contains(reason, testCase.WantReason) {
				t.Fatalf("reason %q does not contain %q", reason, testCase.WantReason)
			}
		})
	}
}

// TestGithubAPIClient follows the pagination links and decodes every payload
// the proof reads.
func TestGithubAPIClient(t *testing.T) {
	var server *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/commits/abc/pulls", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, `[{"number":7,"merged_at":"2026-09-18T20:30:33Z","merge_commit_sha":"abc","head":{"sha":"def"}}]`)
	})
	mux.HandleFunc("/repos/o/r/git/commits/abc", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, `{"tree":{"sha":"t1"}}`)
	})
	mux.HandleFunc("/repos/o/r/git/commits/def", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, `{"tree":{"sha":"t1"}}`)
	})
	mux.HandleFunc("/repos/o/r/commits/def/check-runs", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			fmt.Fprintln(w, `{"check_runs":[{"name":"tests (release PR) / make check","conclusion":"success","started_at":"2026-09-18T20:16:54Z","app":{"slug":"github-actions"}}]}`)
			return
		}
		w.Header().Set("Link", fmt.Sprintf(`<%s/repos/o/r/commits/def/check-runs?page=2>; rel="next"`, server.URL))
		fmt.Fprintln(w, `{"check_runs":[{"name":"make check","conclusion":"skipped","started_at":"2026-09-18T20:16:04Z","app":{"slug":"github-actions"}}]}`)
	})
	server = httptest.NewServer(mux)
	defer server.Close()

	client := githubAPI{repo: "o/r", token: "test", baseURL: server.URL, client: server.Client()}
	pulls, err := client.pullsForCommit("abc")
	if err != nil || len(pulls) != 1 || pulls[0].Head.SHA != "def" || pulls[0].MergedAt == nil {
		t.Fatalf("pullsForCommit = %+v, %v", pulls, err)
	}
	if tree, err := client.commitTree("abc"); err != nil || tree != "t1" {
		t.Fatalf("commitTree = %q, %v", tree, err)
	}
	runs, err := client.checkRuns("def")
	if err != nil {
		t.Fatalf("checkRuns: %v", err)
	}
	if len(runs) != 2 || runs[0].Name != "make check" || runs[1].Name != "tests (release PR) / make check" {
		t.Fatalf("checkRuns must follow pagination: %+v", runs)
	}
}

func TestLatestAttempt(t *testing.T) {
	success := checkRun{Name: "make check", Conclusion: "success", StartedAt: "2026-09-18T20:00:00Z"}
	success.App.Slug = "github-actions"
	skipped := checkRun{Name: "make check", Conclusion: "skipped", StartedAt: "2026-09-18T21:00:00Z"}
	skipped.App.Slug = "github-actions"
	qualified := checkRun{Name: "tests (release PR) / make check", Conclusion: "success", StartedAt: "2026-09-18T22:00:00Z"}
	qualified.App.Slug = "github-actions"

	if conclusion, found, err := latestAttempt(nil, "make check"); err != nil || found {
		t.Fatalf("no runs must report not found; got %q, %t, %v", conclusion, found, err)
	}
	if conclusion, found, err := latestAttempt([]checkRun{success}, "make check"); err != nil || !found || conclusion != "success" {
		t.Fatalf("single success = %q, %t, %v", conclusion, found, err)
	}
	if conclusion, found, err := latestAttempt([]checkRun{qualified, success}, "make check"); err != nil || !found || conclusion != "success" {
		t.Fatalf("the caller-qualified name must match: %q, %t, %v", conclusion, found, err)
	}
	if conclusion, found, err := latestAttempt([]checkRun{success, skipped}, "make check"); err != nil || !found || conclusion != "skipped" {
		t.Fatalf("the latest attempt must win: %q, %t, %v", conclusion, found, err)
	}
}
