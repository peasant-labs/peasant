// Command release-pr-delta decides which release-PR validation gates a push
// must re-run.
//
// The release-PR gate re-runs its expensive matrices on every synchronize push.
// This tool narrows that: a gate runs unless both of these hold:
//
//   - the push does not touch the gate's inputs, and
//   - the previous head carries a successful latest attempt of every required
//     child job of the gate.
//
// "Successful" is exact: every child named in gates.yaml must exist with a
// successful latest attempt. A child that is missing, skipped (unless the
// manifest marks it optional), in progress, failed, or cancelled fails the
// proof, as does an unknown child under the gate's name prefix or a run from a
// foreign producer. Everything unknown fails open: a non-synchronize event, a
// missing previous head, an unreadable revision, an API error, and an empty
// delta all run every gate.
//
// The path delta uses `git diff --no-renames --name-only -z`, so a rename
// reports both endpoints and unusual file names stay readable; matching happens
// in Go, with no pipe whose early exit could invert a result.
//
// The tool writes `e2e=` and `release_validate=` lines to $GITHUB_OUTPUT and
// prints them to stdout. A write failure exits non-zero; the workflow treats a
// failed probe as "run the gates".
package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

//go:embed gates.yaml
var gateManifestBytes []byte

const defaultAPIBase = "https://api.github.com"

type childSpec struct {
	Name     string `yaml:"name"`
	Optional bool   `yaml:"optional"`
}

type gateSpec struct {
	Key      string      `yaml:"key"`
	Prefix   string      `yaml:"prefix"`
	Patterns []string    `yaml:"patterns"`
	Children []childSpec `yaml:"children"`

	compiled []*regexp.Regexp
	required map[string]bool // child name -> optional
}

// loadGates parses the embedded manifest.
func loadGates() ([]gateSpec, error) {
	return parseGates(gateManifestBytes)
}

// parseGates decodes and validates a gate manifest. Unknown fields are
// rejected, so a misspelled field fails loudly instead of leaving a gate with
// no patterns or children.
func parseGates(data []byte) ([]gateSpec, error) {
	var manifest struct {
		Gates []gateSpec `yaml:"gates"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("parse gates.yaml: %w", err)
	}
	if len(manifest.Gates) == 0 {
		return nil, fmt.Errorf("gates.yaml defines no gates")
	}
	seenKeys := make(map[string]bool)
	for i := range manifest.Gates {
		g := &manifest.Gates[i]
		if g.Key == "" || g.Prefix == "" || len(g.Patterns) == 0 || len(g.Children) == 0 {
			return nil, fmt.Errorf("gate %d must define key, prefix, patterns, and children", i)
		}
		if seenKeys[g.Key] {
			return nil, fmt.Errorf("duplicate gate key %q", g.Key)
		}
		seenKeys[g.Key] = true
		for _, pattern := range g.Patterns {
			if strings.TrimSpace(pattern) == "" {
				return nil, fmt.Errorf("gate %s has an empty pattern", g.Key)
			}
			compiled, err := regexp.Compile(pattern)
			if err != nil {
				return nil, fmt.Errorf("gate %s pattern %q: %w", g.Key, pattern, err)
			}
			g.compiled = append(g.compiled, compiled)
		}
		g.required = make(map[string]bool, len(g.Children))
		required := 0
		for _, child := range g.Children {
			if strings.TrimSpace(child.Name) == "" {
				return nil, fmt.Errorf("gate %s has a child with an empty name", g.Key)
			}
			if _, duplicate := g.required[child.Name]; duplicate {
				return nil, fmt.Errorf("gate %s has duplicate child %q", g.Key, child.Name)
			}
			g.required[child.Name] = child.Optional
			if !child.Optional {
				required++
			}
		}
		if required == 0 {
			return nil, fmt.Errorf("gate %s has no required child", g.Key)
		}
	}
	return manifest.Gates, nil
}

// affected reports whether any changed path is an input of the gate.
func (g gateSpec) affected(changed []string) bool {
	for _, path := range changed {
		for _, pattern := range g.compiled {
			if pattern.MatchString(path) {
				return true
			}
		}
	}
	return false
}

// checkRun is one check run observed on the previous head.
type checkRun struct {
	Name       string `json:"name"`
	Conclusion string `json:"conclusion"`
	StartedAt  string `json:"started_at"`
	App        struct {
		Slug string `json:"slug"`
	} `json:"app"`
	CheckSuite struct {
		ID int64 `json:"id"`
	} `json:"check_suite"`
}

// gatePassed reports whether the recorded runs prove the gate completed: the
// latest attempt of every required child is a success, and every child under
// the gate's prefix is known and produced by GitHub Actions. Optional children
// may be absent or skipped; if one ran and failed, the gate fails. Unknown or
// ambiguous evidence - an unknown child, a foreign producer, an unparseable or
// equal timestamp with conflicting conclusions - fails the proof rather than
// being ignored. The returned reason explains a failed proof.
func gatePassed(runs []checkRun, g gateSpec) (bool, string) {
	type attempt struct {
		conclusion string
		at         time.Time
	}
	latest := make(map[string]attempt)
	for _, run := range runs {
		suffix, ok := strings.CutPrefix(run.Name, g.Prefix)
		if !ok {
			continue
		}
		if run.App.Slug != "github-actions" {
			return false, fmt.Sprintf("check run %q has producer %q", run.Name, run.App.Slug)
		}
		if _, known := g.required[suffix]; !known {
			return false, fmt.Sprintf("unknown check run %q", run.Name)
		}
		at, err := time.Parse(time.RFC3339, run.StartedAt)
		if err != nil {
			return false, fmt.Sprintf("check run %q has no usable started_at (%q)", run.Name, run.StartedAt)
		}
		previous, seen := latest[suffix]
		switch {
		case !seen:
			latest[suffix] = attempt{conclusion: run.Conclusion, at: at}
		case at.After(previous.at):
			latest[suffix] = attempt{conclusion: run.Conclusion, at: at}
		case at.Equal(previous.at) && run.Conclusion != previous.conclusion:
			return false, fmt.Sprintf("check run %q has conflicting conclusions at %s", run.Name, run.StartedAt)
		}
	}
	for _, child := range g.Children {
		run, seen := latest[child.Name]
		if !seen {
			if child.Optional {
				continue
			}
			return false, fmt.Sprintf("required child %q never ran", child.Name)
		}
		switch run.conclusion {
		case "success":
		case "skipped", "neutral":
			if !child.Optional {
				return false, fmt.Sprintf("required child %q is %s", child.Name, run.conclusion)
			}
		default:
			return false, fmt.Sprintf("latest attempt of %q is %q", child.Name, run.conclusion)
		}
	}
	return true, ""
}

// decide returns, per gate key, whether the gate must run. A non-synchronize
// event, an empty delta, a delta that touches a gate's inputs, and absent or
// failing evidence all run that gate; the evidence function is called only for
// gates the delta leaves untouched. Every decision is logged with its reason.
func decide(gates []gateSpec, action string, changed []string, evidence func(key string) (bool, string, error)) map[string]bool {
	plan := make(map[string]bool, len(gates))
	failOpen := ""
	switch {
	case action != "synchronize":
		failOpen = "event action " + action + " is not synchronize"
	case len(changed) == 0:
		failOpen = "empty delta"
	}
	for _, g := range gates {
		run := failOpen != "" || g.affected(changed)
		switch {
		case failOpen != "":
			fmt.Fprintf(os.Stderr, "release-pr-delta: gate %s runs: %s\n", g.Key, failOpen)
		case run:
			fmt.Fprintf(os.Stderr, "release-pr-delta: gate %s runs: the push touches its inputs\n", g.Key)
		default:
			passed, reason, err := evidence(g.Key)
			switch {
			case err != nil:
				fmt.Fprintf(os.Stderr, "release-pr-delta: gate %s runs: evidence unavailable: %v\n", g.Key, err)
				run = true
			case !passed:
				fmt.Fprintf(os.Stderr, "release-pr-delta: gate %s runs: %s\n", g.Key, reason)
				run = true
			default:
				fmt.Fprintf(os.Stderr, "release-pr-delta: gate %s skipped: inputs untouched and the previous head has passing evidence\n", g.Key)
			}
		}
		plan[g.Key] = run
	}
	return plan
}

// changedPaths lists the paths between two revisions in the working directory.
func changedPaths(before, head string) ([]string, error) {
	return changedPathsIn("", before, head)
}

// changedPathsIn lists the paths between two revisions without rename detection
// and without Git's quoting, so a rename reports both endpoints and unusual
// file names stay readable.
func changedPathsIn(dir, before, head string) ([]string, error) {
	cmd := exec.Command("git", "diff", "--no-renames", "--name-only", "-z", before, head)
	if dir != "" {
		cmd.Dir = dir
	}
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git diff %s %s: %w", before, head, err)
	}
	var paths []string
	for _, raw := range strings.Split(string(output), "\x00") {
		if raw != "" {
			paths = append(paths, raw)
		}
	}
	return paths, nil
}

type githubChecks struct {
	repo    string
	token   string
	pr      int
	baseURL string
	client  *http.Client
}

// suites returns the check-suite IDs on a commit that GitHub associates with
// the pull request. Evidence from a suite that is not associated with this PR
// is not proof for this PR even when the commit matches, so an unverifiable
// association is an error and the caller runs the gates.
func (g githubChecks) suites(sha string) (map[int64]bool, error) {
	if g.pr <= 0 {
		return nil, fmt.Errorf("no pull request number; cannot verify check-run provenance")
	}
	base := g.baseURL
	if base == "" {
		base = defaultAPIBase
	}
	next := fmt.Sprintf("%s/repos/%s/commits/%s/check-suites?per_page=100", base, g.repo, url.PathEscape(sha))
	allowed := make(map[int64]bool)
	for next != "" {
		req, err := http.NewRequest(http.MethodGet, next, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("Authorization", "Bearer "+g.token)
		resp, err := g.client.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("check-suites for %s: %s", sha, resp.Status)
		}
		var payload struct {
			CheckSuites []struct {
				ID           int64 `json:"id"`
				PullRequests []struct {
					Number int `json:"number"`
				} `json:"pull_requests"`
			} `json:"check_suites"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			resp.Body.Close()
			return nil, err
		}
		for _, suite := range payload.CheckSuites {
			for _, pr := range suite.PullRequests {
				if pr.Number == g.pr {
					allowed[suite.ID] = true
				}
			}
		}
		next = nextPage(resp)
		resp.Body.Close()
	}
	if len(allowed) == 0 {
		return nil, fmt.Errorf("no check suite on %s is associated with pull request #%d", sha, g.pr)
	}
	return allowed, nil
}

// runs lists every check run recorded on a commit by a check suite associated
// with the pull request. The lookup follows the pagination links: a busy pull
// request accumulates check runs from its synchronize, edited, labeled, and
// re-run workflows, and a truncated page could hide a failure.
func (g githubChecks) runs(sha string) ([]checkRun, error) {
	allowed, err := g.suites(sha)
	if err != nil {
		return nil, err
	}
	base := g.baseURL
	if base == "" {
		base = defaultAPIBase
	}
	next := fmt.Sprintf("%s/repos/%s/commits/%s/check-runs?filter=all&per_page=100", base, g.repo, url.PathEscape(sha))
	var result []checkRun
	for next != "" {
		req, err := http.NewRequest(http.MethodGet, next, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("Authorization", "Bearer "+g.token)
		resp, err := g.client.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("check-runs for %s: %s", sha, resp.Status)
		}
		var payload struct {
			CheckRuns []checkRun `json:"check_runs"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			resp.Body.Close()
			return nil, err
		}
		for _, run := range payload.CheckRuns {
			if allowed[run.CheckSuite.ID] {
				result = append(result, run)
			}
		}
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

func token() string {
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if value := os.Getenv(key); value != "" {
			return value
		}
	}
	return ""
}

// writePlan prints the plan and appends it to $GITHUB_OUTPUT when set. The
// error is returned so a missing output cannot pass for a successful probe.
func writePlan(gates []gateSpec, plan map[string]bool) error {
	var lines []string
	for _, g := range gates {
		lines = append(lines, fmt.Sprintf("%s=%t", g.Key, plan[g.Key]))
	}
	for _, line := range lines {
		fmt.Println("release-pr-delta: " + line)
	}
	outputPath := os.Getenv("GITHUB_OUTPUT")
	if outputPath == "" {
		return nil
	}
	file, err := os.OpenFile(outputPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	for _, line := range lines {
		if _, err := fmt.Fprintln(file, line); err != nil {
			file.Close()
			return err
		}
	}
	return file.Close()
}

func run(gates []gateSpec) error {
	before := flag.String("before", "", "previous head SHA (github.event.before)")
	head := flag.String("head", "", "pushed head SHA (github.event.pull_request.head.sha)")
	repo := flag.String("repo", os.Getenv("GITHUB_REPOSITORY"), "owner/name for the check-run lookup")
	pr := flag.Int("pr", 0, "pull request number for check-run provenance (github.event.pull_request.number)")
	action := flag.String("action", "", "the pull_request action that triggered the run")
	flag.Parse()

	plan := make(map[string]bool, len(gates))
	runEverything := func(reason string) {
		fmt.Fprintf(os.Stderr, "release-pr-delta: %s; running every gate\n", reason)
		for _, g := range gates {
			plan[g.Key] = true
		}
	}

	switch {
	case *action != "synchronize":
		runEverything("event action " + *action + " is not synchronize")
	case *before == "" || *head == "":
		runEverything("missing before or head revision")
	default:
		changed, err := changedPaths(*before, *head)
		if err != nil {
			runEverything(err.Error())
			break
		}
		if len(changed) == 0 {
			runEverything("empty delta")
			break
		}

		checks := githubChecks{repo: *repo, token: token(), pr: *pr, client: &http.Client{Timeout: 30 * time.Second}}
		var cached []checkRun
		var cachedErr error
		loaded := false
		evidence := func(key string) (bool, string, error) {
			if !loaded {
				cached, cachedErr = checks.runs(*before)
				loaded = true
			}
			if cachedErr != nil {
				return false, "", cachedErr
			}
			for _, g := range gates {
				if g.Key == key {
					passed, reason := gatePassed(cached, g)
					return passed, reason, nil
				}
			}
			return false, "", fmt.Errorf("unknown gate %q", key)
		}

		fmt.Printf("release-pr-delta: %d changed path(s) since %s\n", len(changed), *before)
		plan = decide(gates, *action, changed, evidence)
	}

	if err := writePlan(gates, plan); err != nil {
		return fmt.Errorf("write GITHUB_OUTPUT: %w", err)
	}
	return nil
}

func main() {
	gates, err := loadGates()
	if err != nil {
		fmt.Fprintf(os.Stderr, "release-pr-delta: %v\n", err)
		os.Exit(1)
	}
	if err := run(gates); err != nil {
		fmt.Fprintf(os.Stderr, "release-pr-delta: %v\n", err)
		os.Exit(1)
	}
}
