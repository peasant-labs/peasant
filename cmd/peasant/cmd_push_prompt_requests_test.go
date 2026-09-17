package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/githooks"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/prompt_request_remote_match.yaml
var promptRequestRemoteMatchYAML []byte

//go:embed testdata/prompt_request_remote_match_manifest.yaml
var promptRequestRemoteMatchManifestYAML []byte

//go:embed testdata/prompt_request_lookup_failure.yaml
var promptRequestLookupFailureYAML []byte

//go:embed testdata/prompt_request_lookup_failure_manifest.yaml
var promptRequestLookupFailureManifestYAML []byte

type promptRequestRemoteMatchFixtures struct {
	Cases []struct {
		Name          string `yaml:"name"`
		PushedRemote  string `yaml:"pushedRemote"`
		RequestRemote string `yaml:"requestRemote"`
		State         string `yaml:"state"`
		WantMatch     bool   `yaml:"wantMatch"`
	} `yaml:"cases"`
}

type promptRequestLookupFailureFixtures struct {
	Cases []struct {
		Name   string `yaml:"name"`
		Status int    `yaml:"status"`
		Body   string `yaml:"body"`
	} `yaml:"cases"`
}

func loadPromptRequestRemoteMatchFixtures(t *testing.T) promptRequestRemoteMatchFixtures {
	t.Helper()
	var fixtures promptRequestRemoteMatchFixtures
	if err := yaml.Unmarshal(promptRequestRemoteMatchYAML, &fixtures); err != nil {
		t.Fatal(err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(promptRequestRemoteMatchManifestYAML, "prompt request remote match")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(fixtures.Cases))
	for _, fixture := range fixtures.Cases {
		names = append(names, fixture.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "prompt request remote match"); err != nil {
		t.Fatal(err)
	}
	return fixtures
}

func loadPromptRequestLookupFailureFixtures(t *testing.T) promptRequestLookupFailureFixtures {
	t.Helper()
	var fixtures promptRequestLookupFailureFixtures
	if err := yaml.Unmarshal(promptRequestLookupFailureYAML, &fixtures); err != nil {
		t.Fatal(err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(promptRequestLookupFailureManifestYAML, "prompt request lookup failure")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(fixtures.Cases))
	for _, fixture := range fixtures.Cases {
		names = append(names, fixture.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "prompt request lookup failure"); err != nil {
		t.Fatal(err)
	}
	return fixtures
}

// TestPromptRequestPrinting proves the repository a waiting request is reported
// against is the repository being pushed, and that nothing else reaches the
// author's eyes.
//
// It asserts the printed output rather than the comparison behind it, because a
// hint about a pull request that is not this repository's — or one that is no
// longer waiting — is the failure that matters here: it sends an author looking
// for a request that is not open.
func TestPromptRequestPrinting(t *testing.T) {
	t.Parallel()
	for _, fixture := range loadPromptRequestRemoteMatchFixtures(t).Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			t.Parallel()
			state := schema.VillagePullRequestAttachmentState(fixture.State)
			if state == "" {
				state = schema.VillagePullRequestAttachmentWaiting
			}
			request := schema.VillagePromptRequest{
				Remote: fixture.RequestRemote,
				Number: 216,
				State:  state,
			}
			var out bytes.Buffer
			printed := printWaitingPromptRequests(&out, []schema.VillagePromptRequest{request},
				githubRepositoryFullName(fixture.PushedRemote))

			if printed != 1 {
				if fixture.WantMatch {
					t.Errorf("pushed remote %q against request remote %q: nothing was printed, want the hint; output:\n%s",
						fixture.PushedRemote, fixture.RequestRemote, out.String())
					return
				}
				if out.Len() != 0 {
					t.Errorf("pushed remote %q against request remote %q: printed without counting it; output:\n%s",
						fixture.PushedRemote, fixture.RequestRemote, out.String())
				}
				return
			}
			if !fixture.WantMatch {
				t.Errorf("pushed remote %q against request remote %q: printed %q, want nothing",
					fixture.PushedRemote, fixture.RequestRemote, out.String())
				return
			}
			want := promptRequestLine(fixture.RequestRemote, 216)
			if out.String() != want {
				t.Errorf("pushed remote %q: got %q, want %q", fixture.PushedRemote, out.String(), want)
			}
		})
	}
}

// promptRequestLine is the exact line the hint prints for one request. The test
// asserts whole lines, so a change to the wording is a change to the test rather
// than a silent drift in what an author reads.
func promptRequestLine(remote string, number int) string {
	return "waiting: " + remote + "#" + strconv.Itoa(number) + " — run 'peasant village push' to attach the prompts behind it\n"
}

// TestPromptRequestLookupBoundStaysUnderTheHookBudget keeps the convenience from
// spending the thing it must not spend.
//
// A hook gives the whole push githooks.DefaultUploadBudget, and the lookup runs
// first on that same clock, so the time it can take belongs to the upload's
// budget. A bound anywhere near the budget would let one stalled read leave the
// upload too little room. The two values are separate constants in separate
// packages, so they are held apart here rather than by a comment.
func TestPromptRequestLookupBoundStaysUnderTheHookBudget(t *testing.T) {
	t.Parallel()
	if promptRequestLookupTimeout*githooks.LookupBudgetShare > githooks.DefaultUploadBudget {
		t.Fatalf("the lookup bound (%s) must stay well under the hook's whole-push budget (%s), or a stalled lookup leaves the upload too little room",
			promptRequestLookupTimeout, githooks.DefaultUploadBudget)
	}
}

// waitingPromptRequestsServer serves the prompt-request lookup the hint makes.
// Every other path answers 404, so a test that reaches the publish step still
// fails loudly rather than hanging.
func waitingPromptRequestsServer(t *testing.T, requests []map[string]any) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/users/me/prompt-requests" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-api-key-abc" {
			t.Errorf("the lookup must carry the caller's credentials; got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{"requests": requests}); err != nil {
			t.Errorf("encode prompt requests: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// promptRequest builds one waiting request as the village serves it.
func promptRequest(remote string, number int) map[string]any {
	owner, name, _ := strings.Cut(remote, "/")
	return map[string]any{
		"owner":        owner,
		"name":         name,
		"number":       number,
		"state":        "waiting",
		"remote":       remote,
		"requested_at": "2026-09-16T00:00:00Z",
	}
}

// gitRepositoryWithRemote creates a real repository with the given origin, which
// is what --repository resolves through git rather than through a stub.
func gitRepositoryWithRemote(t *testing.T, dir, remote string) string {
	t.Helper()
	repo := filepath.Join(dir, "repository")
	hooksGit(t, dir, "", "init", "--quiet", repo)
	hooksGit(t, repo, "", "remote", "add", "origin", remote)
	return repo
}

// TestPushCmd_PrintsOneLinePerWaitingRequestForThisRepository is the acceptance
// shape: several requests, of which only the pushed repository's are reported,
// one line each.
func TestPushCmd_PrintsOneLinePerWaitingRequestForThisRepository(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	server := waitingPromptRequestsServer(t, []map[string]any{
		promptRequest("peasant-labs/village", 216),
		promptRequest("someone-else/village", 7),
		promptRequest("peasant-labs/village", 217),
	})
	writeTestCredentialsFor(t, dir, server.URL)
	repo := gitRepositoryWithRemote(t, dir, "git@github.com:peasant-labs/village.git")

	stdout, stderr, err := executePushCmdSeparate(t, dir, []string{"--dry-run", "--repository", repo})
	if err != nil {
		t.Fatalf("a waiting request must not fail the push: %v\nstdout=%s\nstderr=%s", err, stdout, stderr)
	}
	want := promptRequestLine("peasant-labs/village", 216) + promptRequestLine("peasant-labs/village", 217)
	if !strings.Contains(stdout, want) {
		t.Errorf("the two requests for the pushed repository must print one line each, in order:\nwant:\n%sgot:\n%s", want, stdout)
	}
	if strings.Contains(stdout, "someone-else") {
		t.Errorf("a request for another repository must not be reported; stdout:\n%s", stdout)
	}
	if got := strings.Count(stdout, "waiting: "); got != 2 {
		t.Errorf("printed %d hint lines, want 2; stdout:\n%s", got, stdout)
	}
}

// TestPushCmd_AnUnscopedPushUsesTheWorkingDirectory is the manual push's shape:
// no --repository, so the repository being pushed is the one the author is
// standing in. A hook always passes the flag, so this is the only path that
// reaches git through the working directory.
func TestPushCmd_AnUnscopedPushUsesTheWorkingDirectory(t *testing.T) {
	// Not parallel: the run must happen in the repository, and the working
	// directory is process-global.
	dir := t.TempDir()
	server := waitingPromptRequestsServer(t, []map[string]any{
		promptRequest("peasant-labs/village", 216),
	})
	writeTestCredentialsFor(t, dir, server.URL)
	t.Chdir(gitRepositoryWithRemote(t, dir, "git@github.com:peasant-labs/village.git"))

	stdout, _, err := executePushCmdSeparate(t, dir, []string{"--dry-run"})
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if !strings.Contains(stdout, promptRequestLine("peasant-labs/village", 216)) {
		t.Errorf("an unscoped push must report the request waiting on the repository it runs in; stdout:\n%s", stdout)
	}
}

// TestPushCmd_PrintsNothingWithoutAWaitingRequest keeps the hint silent when
// there is nothing to say. A hook fires on every commit, so a line that prints
// on every run would be noise rather than a signal.
func TestPushCmd_PrintsNothingWithoutAWaitingRequest(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	server := waitingPromptRequestsServer(t, nil)
	writeTestCredentialsFor(t, dir, server.URL)
	repo := gitRepositoryWithRemote(t, dir, "git@github.com:peasant-labs/village.git")

	stdout, _, err := executePushCmdSeparate(t, dir, []string{"--dry-run", "--repository", repo})
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if strings.Contains(stdout, "waiting: ") {
		t.Errorf("no request was waiting, so nothing may be printed; stdout:\n%s", stdout)
	}
}

// TestPushCmd_ARequestForAnotherRepositoryIsSilent covers the pushed repository
// having no request while others do: the hint is about this repository only.
func TestPushCmd_ARequestForAnotherRepositoryIsSilent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	server := waitingPromptRequestsServer(t, []map[string]any{
		promptRequest("peasant-labs/village", 216),
	})
	writeTestCredentialsFor(t, dir, server.URL)
	repo := gitRepositoryWithRemote(t, dir, "git@github.com:peasant-labs/schema.git")

	stdout, _, err := executePushCmdSeparate(t, dir, []string{"--dry-run", "--repository", repo})
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if strings.Contains(stdout, "waiting: ") {
		t.Errorf("the waiting request belongs to another repository; stdout:\n%s", stdout)
	}
}

// TestPushCmd_AFailedLookupIsSilentAndDoesNotBlockThePush is the failure
// discipline the lookup requires: a fetch that fails in any way prints nothing
// and leaves the run at the same point it would have reached without the call.
func TestPushCmd_AFailedLookupIsSilentAndDoesNotBlockThePush(t *testing.T) {
	t.Parallel()
	for _, fixture := range loadPromptRequestLookupFailureFixtures(t).Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			body := fixture.Body
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(fixture.Status)
				_, _ = w.Write([]byte(body))
			}))
			t.Cleanup(server.Close)
			writeTestCredentialsFor(t, dir, server.URL)
			repo := gitRepositoryWithRemote(t, dir, "git@github.com:peasant-labs/village.git")

			stdout, stderr, err := executePushCmdSeparate(t, dir, []string{"--dry-run", "--repository", repo})
			if err != nil {
				t.Fatalf("a failed lookup must not fail the push: %v\nstdout=%s\nstderr=%s", err, stdout, stderr)
			}
			if strings.Contains(stdout, "waiting: ") {
				t.Errorf("a failed lookup must print nothing; stdout:\n%s", stdout)
			}
			if strings.Contains(stderr, "prompt request") {
				t.Errorf("a failed lookup must not surface as a diagnostic the reader has to interpret; stderr:\n%s", stderr)
			}
			// The pipeline is built and the dry run announces itself: the lookup
			// changed nothing about where the run got to.
			if !strings.Contains(stdout, "Dry run — no uploads will be made") {
				t.Errorf("the push must reach the same point it would have reached without the lookup; stdout:\n%s", stdout)
			}
		})
	}
}

// TestPushCmd_AStalledLookupDoesNotFailThePush is the property that makes this
// lookup safe to put in front of an upload.
//
// A village that accepts the connection and then never answers is the realistic
// failure. The lookup exhausts its own bound and nothing else: the push still
// reaches its dry run, and no deadline is reported as the push's own.
func TestPushCmd_AStalledLookupDoesNotFailThePush(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	stalled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-stalled
	}))
	t.Cleanup(func() {
		close(stalled)
		server.Close()
	})
	writeTestCredentialsFor(t, dir, server.URL)
	repo := gitRepositoryWithRemote(t, dir, "git@github.com:peasant-labs/village.git")

	stdout, _, err := executePushCmdSeparate(t, dir, []string{"--dry-run", "--timeout", "5s", "--repository", repo})
	if err != nil {
		t.Fatalf("a stalled lookup must not fail the push: %v\nstdout=%s", err, stdout)
	}
	if !strings.Contains(stdout, "Dry run — no uploads will be made") {
		t.Errorf("the push must still reach its own dry run; stdout:\n%s", stdout)
	}
	if strings.Contains(stdout, "waiting: ") {
		t.Errorf("a stalled lookup must print nothing; stdout:\n%s", stdout)
	}
}

// TestPushCmd_ATightBudgetSkipsTheLookup keeps the convenience out of a budget
// that cannot spare it.
//
// The upload has work of its own to finish under the same cap, so a run whose
// budget is too small to afford the lookup behaves exactly as it did before the
// lookup existed: the read is not made at all, and the run fails — or succeeds —
// for its own reasons.
func TestPushCmd_ATightBudgetSkipsTheLookup(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var lookups atomic.Int64
	server := waitingPromptRequestsServer(t, []map[string]any{
		promptRequest("peasant-labs/village", 216),
	})
	t.Cleanup(server.Close)
	// The lookup would print if it ran; counting requests proves it did not.
	counter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lookups.Add(1)
		http.Redirect(w, r, server.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(counter.Close)
	writeTestCredentialsFor(t, dir, counter.URL)
	repo := gitRepositoryWithRemote(t, dir, "git@github.com:peasant-labs/village.git")

	_, _, err := executePushCmdSeparate(t, dir, []string{"--dry-run", "--timeout", "100ms", "--repository", repo})
	if err == nil {
		t.Fatalf("a 100ms budget cannot cover this run, so it must report its own budget failure")
	}
	if got := lookups.Load(); got != 0 {
		t.Errorf("the lookup must not run under a budget this tight; it was requested %d time(s)", got)
	}
	if !strings.Contains(err.Error(), "the upload ran out of its") {
		t.Errorf("the failure must stay the budget's own, unrelated to the lookup; got: %v", err)
	}
}

// TestPushCmd_QuietStillPrintsAWaitingPromptRequest is the hook surface.
//
// A hook runs the push with --quiet, and a hook run is the only place the author
// is reachable while they are working. A hint suppressed by the quiet a hook
// uses would be a hint nobody ever reads, which is the whole reason this lookup
// exists.
func TestPushCmd_QuietStillPrintsAWaitingPromptRequest(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	server := waitingPromptRequestsServer(t, []map[string]any{
		promptRequest("peasant-labs/village", 216),
	})
	writeTestCredentialsFor(t, dir, server.URL)
	repo := gitRepositoryWithRemote(t, dir, "git@github.com:peasant-labs/village.git")

	stdout, _, err := executePushCmdSeparate(t, dir, []string{"--quiet", "--repository", repo})
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if !strings.Contains(stdout, promptRequestLine("peasant-labs/village", 216)) {
		t.Errorf("--quiet must still print the waiting request, which is why it is exempt; stdout:\n%s", stdout)
	}
}

// TestPushCmd_JsonOutputCarriesNoHint keeps the hint out of a machine-readable
// document. --json is the one run whose stdout is not a console, so a hint line
// in it would reach a consumer that parses the output and is not a reader at
// all.
func TestPushCmd_JsonOutputCarriesNoHint(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	server := waitingPromptRequestsServer(t, []map[string]any{
		promptRequest("peasant-labs/village", 216),
	})
	writeTestCredentialsFor(t, dir, server.URL)
	repo := gitRepositoryWithRemote(t, dir, "git@github.com:peasant-labs/village.git")

	stdout, _, err := executePushCmdSeparate(t, dir, []string{"--dry-run", "--json", "--repository", repo})
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if strings.Contains(stdout, "waiting: ") {
		t.Errorf("--json output must carry no hint line; stdout:\n%s", stdout)
	}
}

// TestPushCmd_AnUnreachableVillageIsSilent covers the transport failure, which is
// the shape a hook hits when the network is down: the lookup must not turn a
// commit into a wait or an error.
func TestPushCmd_AnUnreachableVillageIsSilent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// A server that is closed before the run: the connection is refused.
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	address := server.URL
	server.Close()
	writeTestCredentialsFor(t, dir, address)
	repo := gitRepositoryWithRemote(t, dir, "git@github.com:peasant-labs/village.git")

	stdout, _, err := executePushCmdSeparate(t, dir, []string{"--dry-run", "--repository", repo})
	if err != nil {
		t.Fatalf("an unreachable village must not fail the push: %v", err)
	}
	if strings.Contains(stdout, "waiting: ") {
		t.Errorf("an unreachable village must print nothing; stdout:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Dry run — no uploads will be made") {
		t.Errorf("the push must reach the same point it would have reached without the lookup; stdout:\n%s", stdout)
	}
}
