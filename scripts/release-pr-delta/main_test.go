package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/cases.yaml
var deltaFixtureBytes []byte

//go:embed testdata/manifest_cases.yaml
var manifestFixtureBytes []byte

//go:embed testdata/manifest_policy.yaml
var manifestPolicyBytes []byte

//go:embed testdata/reusable_calls.yaml
var reusableCallsBytes []byte

const githubActionsSlug = "github-actions"

type deltaFixture struct {
	Cases         []deltaCase    `yaml:"cases"`
	EvidenceCases []evidenceCase `yaml:"evidence_cases"`
}

type deltaCase struct {
	Name        string          `yaml:"name"`
	Action      string          `yaml:"action"`
	Changed     []string        `yaml:"changed"`
	E2EEvidence string          `yaml:"e2e_evidence"`
	RVEvidence  string          `yaml:"rv_evidence"`
	Want        map[string]bool `yaml:"want"`
}

type evidenceCase struct {
	Name string       `yaml:"name"`
	Gate string       `yaml:"gate"`
	Runs []fixtureRun `yaml:"runs"`
	Want bool         `yaml:"want"`
}

type fixtureRun struct {
	Name       string `yaml:"name"`
	Conclusion string `yaml:"conclusion"`
	StartedAt  string `yaml:"started_at"`
	AppSlug    string `yaml:"app_slug"`
}

type manifestFixture struct {
	Cases []manifestCase `yaml:"cases"`
}

type manifestCase struct {
	Name      string `yaml:"name"`
	YAML      string `yaml:"yaml"`
	WantError bool   `yaml:"want_error"`
}

func loadDeltaFixture(t *testing.T) deltaFixture {
	t.Helper()
	var fixture deltaFixture
	if err := yaml.Unmarshal(deltaFixtureBytes, &fixture); err != nil {
		t.Fatalf("parse testdata/cases.yaml: %v", err)
	}
	if len(fixture.Cases) == 0 || len(fixture.EvidenceCases) == 0 {
		t.Fatal("testdata/cases.yaml must define cases and evidence_cases")
	}
	return fixture
}

func loadTestGates(t *testing.T) []gateSpec {
	t.Helper()
	gates, err := loadGates()
	if err != nil {
		t.Fatalf("load gates.yaml: %v", err)
	}
	return gates
}

func gateByKey(t *testing.T, gates []gateSpec, key string) gateSpec {
	t.Helper()
	for _, g := range gates {
		if g.Key == key {
			return g
		}
	}
	t.Fatalf("gate %q not defined in gates.yaml", key)
	return gateSpec{}
}

// timestamp returns a deterministic RFC 3339 timestamp for ordering attempts.
func timestamp(seconds int) string {
	return fmt.Sprintf("2026-09-15T00:%02d:%02dZ", seconds/60, seconds%60)
}

// buildRuns renders an evidence kind from the production manifest, so a
// "complete" set is complete by construction and the incomplete kinds drop or
// alter exactly one child.
func buildRuns(t *testing.T, g gateSpec, kind string) []checkRun {
	t.Helper()
	mark := func(conclusion string, seconds int) checkRun {
		return checkRun{
			Name:       g.Prefix + g.Children[0].Name,
			Conclusion: conclusion,
			StartedAt:  timestamp(seconds),
			App:        appSlug(githubActionsSlug),
		}
	}
	allChildren := func(conclusions func(childSpec) string) []checkRun {
		var runs []checkRun
		for i, child := range g.Children {
			runs = append(runs, checkRun{
				Name:       g.Prefix + child.Name,
				Conclusion: conclusions(child),
				StartedAt:  timestamp(i),
				App:        appSlug(githubActionsSlug),
			})
		}
		return runs
	}
	requiredSuccess := func(child childSpec) string {
		if child.Optional {
			return "skipped"
		}
		return "success"
	}
	switch kind {
	case "complete":
		return allChildren(func(child childSpec) string { return "success" })
	case "complete-optional-skipped":
		return allChildren(requiredSuccess)
	case "incomplete":
		runs := allChildren(func(child childSpec) string { return "success" })
		for i := len(runs) - 1; i >= 0; i-- {
			if !g.required[g.Children[i].Name] {
				return append(runs[:i], runs[i+1:]...)
			}
		}
		t.Fatalf("gate %s has no required child to drop", g.Key)
	case "missing":
		return nil
	case "failed":
		return append(allChildren(requiredSuccess), mark("failure", len(g.Children)+1))
	case "cancelled":
		return append(allChildren(requiredSuccess), mark("cancelled", len(g.Children)+1))
	case "required-skipped":
		runs := allChildren(requiredSuccess)
		runs[0].Conclusion = "skipped"
		return runs
	case "optional-failed":
		runs := allChildren(requiredSuccess)
		for i, child := range g.Children {
			if child.Optional {
				runs[i].Conclusion = "failure"
				runs[i].StartedAt = timestamp(len(g.Children) + 1)
			}
		}
		return runs
	case "foreign-child":
		return append(allChildren(requiredSuccess), checkRun{
			Name:       g.Prefix + "mystery job",
			Conclusion: "success",
			StartedAt:  timestamp(len(g.Children) + 1),
			App:        appSlug(githubActionsSlug),
		})
	case "rerun-success":
		failed := allChildren(requiredSuccess)
		failed[0].Conclusion = "failure"
		return append(failed, mark("success", len(g.Children)+1))
	case "in-progress":
		return append(allChildren(requiredSuccess), mark("", len(g.Children)+1))
	default:
		t.Fatalf("unknown evidence kind %q", kind)
		return nil
	}
	return nil
}

func appSlug(slug string) struct {
	Slug string `json:"slug"`
} {
	var app struct {
		Slug string `json:"slug"`
	}
	app.Slug = slug
	return app
}

func TestDecideFromFixture(t *testing.T) {
	gates := loadTestGates(t)
	for _, tc := range loadDeltaFixture(t).Cases {
		t.Run(tc.Name, func(t *testing.T) {
			evidenceRuns := map[string][]checkRun{
				"e2e":              buildRuns(t, gateByKey(t, gates, "e2e"), tc.E2EEvidence),
				"release_validate": buildRuns(t, gateByKey(t, gates, "release_validate"), tc.RVEvidence),
			}
			evidence := func(key string) (bool, string, error) {
				passed, reason := gatePassed(evidenceRuns[key], gateByKey(t, gates, key))
				return passed, reason, nil
			}
			plan := decide(gates, tc.Action, tc.Changed, evidence)
			for _, g := range gates {
				want, ok := tc.Want[g.Key]
				if !ok {
					t.Fatalf("case %s is missing want.%s", tc.Name, g.Key)
				}
				if plan[g.Key] != want {
					t.Errorf("%s: %s = %v, want %v", tc.Name, g.Key, plan[g.Key], want)
				}
			}
		})
	}
}

func TestGateEvidenceFromFixture(t *testing.T) {
	gates := loadTestGates(t)
	for _, tc := range loadDeltaFixture(t).EvidenceCases {
		t.Run(tc.Name, func(t *testing.T) {
			g := gateByKey(t, gates, tc.Gate)
			runs := make([]checkRun, 0, len(tc.Runs))
			for _, run := range tc.Runs {
				slug := run.AppSlug
				switch slug {
				case "":
					slug = githubActionsSlug
				case "missing":
					slug = ""
				}
				check := checkRun{Name: g.Prefix + run.Name, Conclusion: run.Conclusion, StartedAt: run.StartedAt}
				check.App = appSlug(slug)
				runs = append(runs, check)
			}
			if got, _ := gatePassed(runs, g); got != tc.Want {
				t.Fatalf("gatePassed = %v, want %v", got, tc.Want)
			}
		})
	}
}

func TestParseGatesManifestCases(t *testing.T) {
	var fixture manifestFixture
	if err := yaml.Unmarshal(manifestFixtureBytes, &fixture); err != nil {
		t.Fatalf("parse testdata/manifest_cases.yaml: %v", err)
	}
	if len(fixture.Cases) == 0 {
		t.Fatal("testdata/manifest_cases.yaml must define cases")
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			_, err := parseGates([]byte(tc.YAML))
			if tc.WantError && err == nil {
				t.Fatal("parseGates accepted a malformed manifest")
			}
			if !tc.WantError && err != nil {
				t.Fatalf("parseGates rejected a valid manifest: %v", err)
			}
		})
	}
}

func TestLoadGatesManifest(t *testing.T) {
	gates := loadTestGates(t)
	if len(gates) != 2 {
		t.Fatalf("gates = %d, want e2e and release_validate", len(gates))
	}
	rv := gateByKey(t, gates, "release_validate")
	if !rv.required["macos cask install (rc only)"] {
		t.Fatal("macos cask install must stay optional (rc only)")
	}
	if rv.required["goreleaser snapshot"] {
		t.Fatal("goreleaser snapshot must stay required")
	}
}

// TestManifestOptionalPartitionPolicy pins the required/optional partition by
// name: only the rc-only macOS cask install may skip. Marking any other child
// optional would weaken the proof without changing the child names.
func TestManifestOptionalPartitionPolicy(t *testing.T) {
	var policy struct {
		OptionalChildren map[string][]string `yaml:"optional_children"`
	}
	if err := yaml.Unmarshal(manifestPolicyBytes, &policy); err != nil {
		t.Fatalf("parse testdata/manifest_policy.yaml: %v", err)
	}
	gates := loadTestGates(t)
	if len(policy.OptionalChildren) == 0 {
		t.Fatal("testdata/manifest_policy.yaml must define optional_children")
	}
	for _, g := range gates {
		wantOptional := make(map[string]bool)
		for _, name := range policy.OptionalChildren[g.Key] {
			wantOptional[name] = true
		}
		for _, child := range g.Children {
			if child.Optional != wantOptional[child.Name] {
				t.Errorf("gate %s child %q optional = %v, want %v (manifest_policy.yaml)", g.Key, child.Name, child.Optional, wantOptional[child.Name])
			}
		}
		for _, name := range policy.OptionalChildren[g.Key] {
			if _, defined := g.required[name]; !defined {
				t.Errorf("manifest_policy.yaml lists optional child %q that gate %s does not define", name, g.Key)
			}
		}
	}
}

// --- Workflow/manifest parity -------------------------------------------------

type workflowJob struct {
	Name     string `yaml:"name"`
	Uses     string `yaml:"uses"`
	Strategy struct {
		Matrix map[string]any `yaml:"matrix"`
	} `yaml:"strategy"`
}

type workflowDoc struct {
	Jobs map[string]workflowJob `yaml:"jobs"`
}

var matrixReference = regexp.MustCompile(`\$\{\{\s*matrix\.([A-Za-z0-9_.-]+)\s*\}\}`)

// reusableCalls maps a reusable-workflow `uses` value to the job names the
// called workflow defines. GitHub names the resulting check runs
// "<caller> / <called job>", so the called names cannot be read from this
// repository; the fixture declares them.
type reusableCalls map[string][]string

// loadReusableCalls parses the called-job manifest and rejects an entry that
// names no job, so a malformed fixture fails loudly.
func loadReusableCalls(t *testing.T) reusableCalls {
	t.Helper()
	var manifest struct {
		Calls reusableCalls `yaml:"calls"`
	}
	if err := yaml.Unmarshal(reusableCallsBytes, &manifest); err != nil {
		t.Fatalf("parse testdata/reusable_calls.yaml: %v", err)
	}
	if len(manifest.Calls) == 0 {
		t.Fatal("testdata/reusable_calls.yaml must define calls")
	}
	for uses, jobs := range manifest.Calls {
		if strings.TrimSpace(uses) == "" || len(jobs) == 0 {
			t.Fatalf("testdata/reusable_calls.yaml entry %q must name at least one called job", uses)
		}
		for _, job := range jobs {
			if strings.TrimSpace(job) == "" {
				t.Fatalf("testdata/reusable_calls.yaml entry %q has an empty called job", uses)
			}
		}
	}
	return manifest.Calls
}

func workflowJobs(t *testing.T, path string) map[string]workflowJob {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc workflowDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if len(doc.Jobs) == 0 {
		t.Fatalf("%s defines no jobs", path)
	}
	return doc.Jobs
}

// expandJobNames renders every check-run name a job produces. A job is named by
// its display name, or by its id when it declares none. A job that calls a
// reusable workflow produces one check run per called job, named
// "<caller> / <called job>". Matrix variables expand into every concrete value
// combination. An undeclared reusable call fails: the name cannot be guessed.
func expandJobNames(t *testing.T, jobID string, job workflowJob, calls reusableCalls) []string {
	t.Helper()
	base := job.Name
	if strings.TrimSpace(base) == "" {
		base = jobID
	}
	names := expandMatrixNames(base, job.Strategy.Matrix)
	if job.Uses == "" {
		return names
	}
	called, ok := calls[job.Uses]
	if !ok {
		t.Fatalf("job %q calls %q with no entry in testdata/reusable_calls.yaml", jobID, job.Uses)
	}
	var expanded []string
	for _, callerName := range names {
		for _, calledJob := range called {
			expanded = append(expanded, callerName+" / "+calledJob)
		}
	}
	return expanded
}

// expandMatrixNames expands the matrix variables of a single job name.
func expandMatrixNames(name string, matrix map[string]any) []string {
	if !strings.Contains(name, "${{ matrix.") {
		return []string{name}
	}
	var names []string
	seen := make(map[string]bool)
	for _, variables := range matrixCombinations(matrix) {
		expanded := matrixReference.ReplaceAllStringFunc(name, func(match string) string {
			parts := matrixReference.FindStringSubmatch(match)
			return variables[parts[1]]
		})
		if !seen[expanded] {
			seen[expanded] = true
			names = append(names, expanded)
		}
	}
	return names
}

// matrixCombinations returns the variable assignments of a strategy matrix:
// one per `include` entry, plus the Cartesian product of the list-valued
// variables. Nested maps flatten to `parent.child` names.
func matrixCombinations(matrix map[string]any) []map[string]string {
	var combinations []map[string]string
	if include, ok := matrix["include"].([]any); ok {
		for _, item := range include {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			variables := make(map[string]string)
			flattenVariables("", entry, variables)
			combinations = append(combinations, variables)
		}
	}
	keys := make([]string, 0, len(matrix))
	for key := range matrix {
		if key != "include" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	product := []map[string]string{}
	hasProductKeys := false
	for _, key := range keys {
		values, ok := matrix[key].([]any)
		if !ok {
			continue
		}
		if !hasProductKeys {
			product = []map[string]string{{}}
			hasProductKeys = true
		}
		var options []map[string]string
		for _, item := range values {
			variables := make(map[string]string)
			if nested, ok := item.(map[string]any); ok {
				flattenVariables(key, nested, variables)
			} else {
				variables[key] = fmt.Sprint(item)
			}
			options = append(options, variables)
		}
		var next []map[string]string
		for _, base := range product {
			for _, option := range options {
				merged := make(map[string]string, len(base)+len(option))
				for name, value := range base {
					merged[name] = value
				}
				for name, value := range option {
					merged[name] = value
				}
				next = append(next, merged)
			}
		}
		product = next
	}
	return append(combinations, product...)
}

func flattenVariables(prefix string, values map[string]any, into map[string]string) {
	for key, value := range values {
		name := key
		if prefix != "" {
			name = prefix + "." + key
		}
		if nested, ok := value.(map[string]any); ok {
			flattenVariables(name, nested, into)
			continue
		}
		into[name] = fmt.Sprint(value)
	}
}

// TestManifestMatchesWorkflows pins the required child names (and the caller
// prefixes) to the jobs the workflows actually define, with matrix expansion
// and reusable-call nesting. This is the required-NAME guard: a child removed
// from gates.yaml fails here instead of silently weakening the evidence.
func TestManifestMatchesWorkflows(t *testing.T) {
	gates := loadTestGates(t)
	calls := loadReusableCalls(t)
	root := filepath.Join("..", "..")
	caller := workflowJobs(t, filepath.Join(root, ".github", "workflows", "release-pr.yml"))
	plans := []struct {
		gateKey    string
		callerJob  string
		calledFile string
	}{
		{"e2e", "e2e", ".github/workflows/e2e.yml"},
		{"release_validate", "release-validate", ".github/workflows/release-validate.yml"},
	}
	for _, plan := range plans {
		t.Run(plan.gateKey, func(t *testing.T) {
			callerJob, ok := caller[plan.callerJob]
			if !ok {
				t.Fatalf("release-pr.yml has no job %q", plan.callerJob)
			}
			g := gateByKey(t, gates, plan.gateKey)
			if want := callerJob.Name + " / "; g.Prefix != want {
				t.Errorf("gate %s prefix = %q, want %q (caller job name)", g.Key, g.Prefix, want)
			}
			jobs := workflowJobs(t, filepath.Join(root, plan.calledFile))
			jobIDs := make([]string, 0, len(jobs))
			for jobID := range jobs {
				jobIDs = append(jobIDs, jobID)
			}
			sort.Strings(jobIDs)
			var want []string
			for _, jobID := range jobIDs {
				want = append(want, expandJobNames(t, jobID, jobs[jobID], calls)...)
			}
			got := make([]string, 0, len(g.Children))
			for _, child := range g.Children {
				got = append(got, child.Name)
			}
			if !sameStringSet(got, want) {
				t.Fatalf("gate %s children = %v, want the workflow jobs %v", g.Key, got, want)
			}
		})
	}
}

func sameStringSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	counts := make(map[string]int, len(got))
	for _, value := range got {
		counts[value]++
	}
	for _, value := range want {
		counts[value]--
		if counts[value] < 0 {
			return false
		}
	}
	for _, count := range counts {
		if count != 0 {
			return false
		}
	}
	return true
}

func TestNextPageParsesLinkHeader(t *testing.T) {
	responseWith := func(header string) *http.Response {
		resp := &http.Response{Header: http.Header{}}
		if header != "" {
			resp.Header.Set("Link", header)
		}
		return resp
	}
	if got := nextPage(responseWith("")); got != "" {
		t.Fatalf("no Link header: nextPage = %q, want empty", got)
	}
	next := `<https://api.github.com/repos/o/r/commits/abc/check-runs?page=2>; rel="next", <https://api.github.com/repos/o/r/commits/abc/check-runs?page=5>; rel="last"`
	want := "https://api.github.com/repos/o/r/commits/abc/check-runs?page=2"
	if got := nextPage(responseWith(next)); got != want {
		t.Fatalf("nextPage = %q, want %q", got, want)
	}
	lastOnly := `<https://api.github.com/repos/o/r/commits/abc/check-runs?page=1>; rel="prev"`
	if got := nextPage(responseWith(lastOnly)); got != "" {
		t.Fatalf("rel prev only: nextPage = %q, want empty", got)
	}
}

func withSuite(run checkRun, id int64) checkRun {
	run.CheckSuite.ID = id
	return run
}

func TestRunsFollowsPaginationAndFiltersByPullRequest(t *testing.T) {
	page := func(runs []checkRun) string {
		payload, err := json.Marshal(map[string]any{"check_runs": runs})
		if err != nil {
			t.Fatal(err)
		}
		return string(payload)
	}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("Authorization = %q, want bearer token", r.Header.Get("Authorization"))
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/check-suites"):
			fmt.Fprint(w, `{"check_suites":[{"id":1,"pull_requests":[{"number":42}]},{"id":2,"pull_requests":[{"number":7}]}]}`)
		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			if r.URL.Query().Get("filter") != "all" {
				t.Errorf("filter = %q, want all", r.URL.Query().Get("filter"))
			}
			switch r.URL.Query().Get("page") {
			case "":
				w.Header().Set("Link", fmt.Sprintf(`<%s/repos/o/r/commits/abc/check-runs?filter=all&page=2>; rel="next"`, server.URL))
				fmt.Fprint(w, page([]checkRun{
					withSuite(checkRun{Name: "this-pr", Conclusion: "success"}, 1),
					withSuite(checkRun{Name: "other-pr", Conclusion: "success"}, 2),
				}))
			case "2":
				fmt.Fprint(w, page([]checkRun{withSuite(checkRun{Name: "this-pr-page2", Conclusion: "success"}, 1)}))
			default:
				t.Errorf("unexpected page %q", r.URL.Query().Get("page"))
			}
		}
	}))
	defer server.Close()

	checks := githubChecks{repo: "o/r", token: "test-token", pr: 42, baseURL: server.URL, client: server.Client()}
	runs, err := checks.runs("abc")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].Name != "this-pr" || runs[1].Name != "this-pr-page2" {
		t.Fatalf("runs = %+v, want both pages of this PR's suite only", runs)
	}

	unassociated := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"check_suites":[{"id":2,"pull_requests":[{"number":7}]}]}`)
	}))
	defer unassociated.Close()
	if _, err := (githubChecks{repo: "o/r", token: "t", pr: 42, baseURL: unassociated.URL, client: unassociated.Client()}).runs("abc"); err == nil {
		t.Fatal("no check suite associated with this PR must return an error so the caller runs the gates")
	}
	if _, err := (githubChecks{repo: "o/r", token: "t", pr: 0, baseURL: unassociated.URL, client: unassociated.Client()}).runs("abc"); err == nil {
		t.Fatal("a missing pull request number must return an error so the caller runs the gates")
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer bad.Close()
	if _, err := (githubChecks{repo: "o/r", token: "t", pr: 42, baseURL: bad.URL, client: bad.Client()}).runs("abc"); err == nil {
		t.Fatal("non-200 response must return an error so the caller runs the gates")
	}

	malformed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "{not json")
	}))
	defer malformed.Close()
	if _, err := (githubChecks{repo: "o/r", token: "t", pr: 42, baseURL: malformed.URL, client: malformed.Client()}).runs("abc"); err == nil {
		t.Fatal("malformed JSON must return an error so the caller runs the gates")
	}
}

func TestWritePlan(t *testing.T) {
	gates := loadTestGates(t)
	plan := map[string]bool{"e2e": false, "release_validate": true}

	output := filepath.Join(t.TempDir(), "github_output")
	t.Setenv("GITHUB_OUTPUT", output)
	if err := writePlan(gates, plan); err != nil {
		t.Fatalf("writePlan: %v", err)
	}
	content, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(content)); got != "e2e=false\nrelease_validate=true" {
		t.Fatalf("GITHUB_OUTPUT = %q", got)
	}

	t.Setenv("GITHUB_OUTPUT", filepath.Join(t.TempDir(), "missing", "github_output"))
	if err := writePlan(gates, plan); err == nil {
		t.Fatal("unwritable GITHUB_OUTPUT must return an error, never pass for a successful probe")
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	git(t, dir, "config", "user.email", "test@example.com")
	git(t, dir, "config", "user.name", "Test")
	return dir
}

func commit(t *testing.T, dir, message string) string {
	t.Helper()
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", message)
	return git(t, dir, "rev-parse", "HEAD")
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestChangedPathsReportsRenameEndpoints(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "cmd/main.go", "package main\n")
	base := commit(t, dir, "add cmd/main.go")
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "mv", "cmd/main.go", "docs/main.go")
	write(t, dir, "docs/main.go", "// moved\npackage main\n")
	head := commit(t, dir, "move cmd/main.go to docs/main.go")

	paths, err := changedPathsIn(dir, base, head)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"cmd/main.go": true, "docs/main.go": true}
	if len(paths) != len(want) {
		t.Fatalf("paths = %v, want both rename endpoints", paths)
	}
	for _, path := range paths {
		if !want[path] {
			t.Fatalf("unexpected path %q in %v", path, paths)
		}
	}
}

func TestChangedPathsKeepsUnusualNames(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "docs/guide.md", "base\n")
	base := commit(t, dir, "base")
	write(t, dir, "web/café.js", "console.log(1)\n")
	write(t, dir, "web/tab\tname.js", "console.log(2)\n")
	head := commit(t, dir, "add unusual names")

	paths, err := changedPathsIn(dir, base, head)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(paths, "\n")
	for _, want := range []string{"web/café.js", "web/tab\tname.js"} {
		if !strings.Contains(got, want) {
			t.Fatalf("paths = %q, want to contain %q", got, want)
		}
	}
}

func TestChangedPathsUnknownRevisionErrors(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "README.md", "base\n")
	head := commit(t, dir, "base")
	if _, err := changedPathsIn(dir, "0000000000000000000000000000000000000000", head); err == nil {
		t.Fatal("unknown revision must return an error so the caller fails open")
	}
}

func TestChangedPathsDivergentRevisions(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "base.md", "base\n")
	base := commit(t, dir, "base")
	git(t, dir, "checkout", "-q", "-b", "old", base)
	write(t, dir, "cmd/old.go", "package main\n")
	oldTip := commit(t, dir, "old branch change")
	git(t, dir, "checkout", "-q", "-b", "new", base)
	write(t, dir, "cmd/new.go", "package main\n")
	newTip := commit(t, dir, "new branch change")

	paths, err := changedPathsIn(dir, oldTip, newTip)
	if err != nil {
		t.Fatalf("divergent but available revisions must diff: %v", err)
	}
	want := map[string]bool{"cmd/old.go": true, "cmd/new.go": true}
	if len(paths) != len(want) {
		t.Fatalf("paths = %v, want both divergent branch changes", paths)
	}
	for _, path := range paths {
		if !want[path] {
			t.Fatalf("unexpected path %q in %v", path, paths)
		}
	}
}

func TestChangedPathsLargeDelta(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "README.md", "base\n")
	base := commit(t, dir, "base")
	const files = 300
	for i := 0; i < files; i++ {
		write(t, dir, fmt.Sprintf("web/generated/file-%03d.js", i), "// generated\n")
	}
	head := commit(t, dir, "large delta")

	paths, err := changedPathsIn(dir, base, head)
	if err != nil {
		t.Fatalf("large delta: %v", err)
	}
	if len(paths) != files {
		t.Fatalf("paths = %d, want %d (no truncation)", len(paths), files)
	}
}
