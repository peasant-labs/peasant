// Command post-merge-evidence proves whether a push to a long-lived branch may
// skip the `make check` gate because the merged pull request already passed it.
//
// A squash merge of a green pull request lands a tree that the pull request's
// `Tests / make check` job has already validated. The push run repeats that
// work. This probe suppresses the repeat only when all of these hold:
//
//   - the pushed commit is the merge_commit_sha of exactly one merged pull
//     request (so the push is that pull request's squash merge, not a direct
//     push, a merge commit, or an ambiguous rebase association),
//   - the pushed commit's tree is identical to the pull request head's tree
//     (so the merged content is the content the pull request's checks ran on;
//     a base that moved under the pull request leaves a different tree), and
//   - the pull request head carries a successful latest `make check` attempt
//     produced by GitHub Actions.
//
// The check-run lookup matches the job name exactly and the caller-qualified
// form `... / make check`, because inside a reusable-workflow call the job
// name carries its caller prefix. Evidence is content-addressed: the run is
// accepted because its commit is the pull request head whose tree equals the
// merged tree, not because a check suite names the pull request.
//
// Everything unknown fails open. No associated pull request, more than one
// candidate, an unmerged pull request, a merge-commit mismatch, a missing or
// unparseable tree, a skipped, cancelled, failed, or missing check, a producer
// other than GitHub Actions, an empty head, and any API error all leave
// `skip=false`, so the gate still runs. The tool exits non-zero only when it
// cannot write its own output; the workflow treats that as "run the gate".
//
// The tool writes `skip=<bool>` to $GITHUB_OUTPUT and prints it to stdout.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const defaultAPIBase = "https://api.github.com"

// pullRef is the part of a pull request the proof needs.
type pullRef struct {
	Number         int     `json:"number"`
	MergedAt       *string `json:"merged_at"`
	MergeCommitSHA string  `json:"merge_commit_sha"`
	Head           struct {
		SHA string `json:"sha"`
	} `json:"head"`
}

// checkRun is one check run recorded on the pull request head.
type checkRun struct {
	Name       string `json:"name"`
	Conclusion string `json:"conclusion"`
	StartedAt  string `json:"started_at"`
	App        struct {
		Slug string `json:"slug"`
	} `json:"app"`
}

// api is the GitHub surface the proof needs.
type api interface {
	pullsForCommit(sha string) ([]pullRef, error)
	commitTree(sha string) (string, error)
	checkRuns(sha string) ([]checkRun, error)
}

// githubAPI fetches the proof inputs over the REST API.
type githubAPI struct {
	repo    string
	token   string
	baseURL string
	client  *http.Client
}

func (g githubAPI) get(path string, out any) error {
	base := g.baseURL
	if base == "" {
		base = defaultAPIBase
	}
	req, err := http.NewRequest(http.MethodGet, base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if g.token != "" {
		req.Header.Set("Authorization", "Bearer "+g.token)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (g githubAPI) pullsForCommit(sha string) ([]pullRef, error) {
	var payload []pullRef
	err := g.get(fmt.Sprintf("/repos/%s/commits/%s/pulls?per_page=100", g.repo, url.PathEscape(sha)), &payload)
	return payload, err
}

func (g githubAPI) commitTree(sha string) (string, error) {
	var payload struct {
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	if err := g.get(fmt.Sprintf("/repos/%s/git/commits/%s", g.repo, url.PathEscape(sha)), &payload); err != nil {
		return "", err
	}
	if payload.Tree.SHA == "" {
		return "", fmt.Errorf("commit %s has no tree", sha)
	}
	return payload.Tree.SHA, nil
}

func (g githubAPI) checkRuns(sha string) ([]checkRun, error) {
	base := g.baseURL
	if base == "" {
		base = defaultAPIBase
	}
	next := base + fmt.Sprintf("/repos/%s/commits/%s/check-runs?filter=all&per_page=100", g.repo, url.PathEscape(sha))
	var result []checkRun
	for next != "" {
		req, err := http.NewRequest(http.MethodGet, next, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		if g.token != "" {
			req.Header.Set("Authorization", "Bearer "+g.token)
		}
		resp, err := g.client.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("GET %s: %s", req.URL.Path, resp.Status)
		}
		var payload struct {
			CheckRuns []checkRun `json:"check_runs"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			resp.Body.Close()
			return nil, err
		}
		result = append(result, payload.CheckRuns...)
		next = nextPage(resp)
		resp.Body.Close()
	}
	return result, nil
}

// nextPage returns the URL of the next page from the response's Link header.
func nextPage(resp *http.Response) string {
	for _, header := range resp.Header.Values("Link") {
		for _, link := range strings.Split(header, ",") {
			sections := strings.Split(link, ";")
			if len(sections) < 2 || !strings.Contains(sections[1], `rel="next"`) {
				continue
			}
			return strings.Trim(strings.TrimSpace(sections[0]), "<>")
		}
	}
	return ""
}

// latestAttempt returns the conclusion of the latest `child` attempt among the
// runs. A run matches the exact job name or its caller-qualified form
// `... / child`; a foreign producer, an unusable timestamp, or conflicting
// conclusions at the same start time fails the proof.
func latestAttempt(runs []checkRun, child string) (string, bool, error) {
	type attempt struct {
		conclusion string
		at         time.Time
	}
	var latest attempt
	seen := false
	for _, run := range runs {
		if run.Name != child && !strings.HasSuffix(run.Name, "/ "+child) {
			continue
		}
		if run.App.Slug != "github-actions" {
			return "", false, fmt.Errorf("check run %q has producer %q", run.Name, run.App.Slug)
		}
		at, err := time.Parse(time.RFC3339, run.StartedAt)
		if err != nil {
			return "", false, fmt.Errorf("check run %q has no usable started_at (%q)", run.Name, run.StartedAt)
		}
		switch {
		case !seen:
			latest = attempt{conclusion: run.Conclusion, at: at}
			seen = true
		case at.After(latest.at):
			latest = attempt{conclusion: run.Conclusion, at: at}
		case at.Equal(latest.at) && run.Conclusion != latest.conclusion:
			return "", false, fmt.Errorf("check runs for %q conflict at %s", child, run.StartedAt)
		}
	}
	if !seen {
		return "", false, nil
	}
	return latest.conclusion, true, nil
}

// prove returns the skip decision. The reason explains a false decision; an
// error is a decision that could not be made and must run the gate.
func prove(client api, head, child string) (bool, string, error) {
	candidates := []pullRef{}
	pulls, err := client.pullsForCommit(head)
	if err != nil {
		return false, "", err
	}
	for _, pull := range pulls {
		if pull.MergedAt != nil && pull.MergeCommitSHA == head && pull.Head.SHA != "" {
			candidates = append(candidates, pull)
		}
	}
	if len(candidates) == 0 {
		return false, "no merged pull request has this commit as its merge commit", nil
	}
	if len(candidates) > 1 {
		return false, fmt.Sprintf("%d merged pull requests name this commit as their merge commit", len(candidates)), nil
	}
	headTree, err := client.commitTree(head)
	if err != nil {
		return false, "", err
	}
	pullTree, err := client.commitTree(candidates[0].Head.SHA)
	if err != nil {
		return false, "", err
	}
	if headTree != pullTree {
		return false, fmt.Sprintf("the merged tree %s differs from the pull request head tree %s", headTree, pullTree), nil
	}
	runs, err := client.checkRuns(candidates[0].Head.SHA)
	if err != nil {
		return false, "", err
	}
	conclusion, found, err := latestAttempt(runs, child)
	if err != nil {
		return false, "", err
	}
	if !found {
		return false, fmt.Sprintf("the pull request head has no %q check run", child), nil
	}
	if conclusion != "success" {
		return false, fmt.Sprintf("the latest %q attempt is %q", child, conclusion), nil
	}
	return true, "", nil
}

func token() string {
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if value := os.Getenv(key); value != "" {
			return value
		}
	}
	return ""
}

// writeSkip prints the decision and appends it to $GITHUB_OUTPUT when set.
func writeSkip(skip bool) error {
	line := fmt.Sprintf("skip=%t", skip)
	fmt.Println("post-merge-evidence: " + line)
	outputPath := os.Getenv("GITHUB_OUTPUT")
	if outputPath == "" {
		return nil
	}
	file, err := os.OpenFile(outputPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(file, line); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

func run() error {
	head := flag.String("head", "", "pushed head SHA (github.sha)")
	repo := flag.String("repo", os.Getenv("GITHUB_REPOSITORY"), "owner/name for the API lookups")
	child := flag.String("child", "make check", "the job name whose evidence proves the gate")
	flag.Parse()

	skip := false
	if *head == "" || *repo == "" {
		fmt.Fprintln(os.Stderr, "post-merge-evidence: missing head or repo; running the gate")
	} else {
		client := githubAPI{repo: *repo, token: token(), client: &http.Client{Timeout: 30 * time.Second}}
		proved, reason, err := prove(client, *head, *child)
		switch {
		case err != nil:
			fmt.Fprintf(os.Stderr, "post-merge-evidence: running the gate: %v\n", err)
		case !proved:
			fmt.Fprintf(os.Stderr, "post-merge-evidence: running the gate: %s\n", reason)
		default:
			fmt.Fprintf(os.Stderr, "post-merge-evidence: skipping %q: the merged tree is the tested pull request head\n", *child)
			skip = true
		}
	}
	if err := writeSkip(skip); err != nil {
		return fmt.Errorf("write GITHUB_OUTPUT: %w", err)
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "post-merge-evidence: %v\n", err)
		os.Exit(1)
	}
}
