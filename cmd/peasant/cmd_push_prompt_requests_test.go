package main

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/prompt_request_remote_match.yaml
var promptRequestRemoteMatchYAML []byte

//go:embed testdata/prompt_request_remote_match_manifest.yaml
var promptRequestRemoteMatchManifestYAML []byte

type promptRequestRemoteMatchFixtures struct {
	Cases []struct {
		Name          string `yaml:"name"`
		PushedRemote  string `yaml:"pushedRemote"`
		RequestRemote string `yaml:"requestRemote"`
		WantMatch     bool   `yaml:"wantMatch"`
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

// TestPromptRequestRemoteMatch proves the repository a waiting request is
// reported against is the repository being pushed, and nothing else.
//
// The failure this guards is a hint about a pull request that does not belong to
// the pushed repository: it sends an author looking for a request that is not
// theirs, and the mirror-clone shape is the one that looks right by name while
// naming a different repository.
func TestPromptRequestRemoteMatch(t *testing.T) {
	t.Parallel()
	for _, fixture := range loadPromptRequestRemoteMatchFixtures(t).Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			t.Parallel()
			got := sameRepositoryFullName(githubRepositoryFullName(fixture.PushedRemote), fixture.RequestRemote)
			if got != fixture.WantMatch {
				t.Errorf("pushed remote %q against request remote %q: matched=%v, want %v",
					fixture.PushedRemote, fixture.RequestRemote, got, fixture.WantMatch)
			}
		})
	}
}

// promptRequestLine is the exact line the hint prints for one request. The test
// asserts whole lines, so a change to the wording is a change to the test rather
// than a silent drift in what an author reads.
func promptRequestLine(owner, name string, number int) string {
	return "waiting: " + owner + "/" + name + "#" + strconv.Itoa(number) + " — run 'peasant village push' to attach the prompts behind it\n"
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
	want := promptRequestLine("peasant-labs", "village", 216) + promptRequestLine("peasant-labs", "village", 217)
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
	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{
			name: "server-error",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
		},
		{
			name: "unauthorized",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
			},
		},
		{
			name: "malformed-body",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte("{"))
			},
		},
		{
			name: "village-predating-the-endpoint",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.NotFound(w, r)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			server := httptest.NewServer(tc.handler)
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
			// The pipeline is built and the dry run announces itself: the
			// lookup changed nothing about where the run got to.
			if !strings.Contains(stdout, "Dry run — no uploads will be made") {
				t.Errorf("the push must reach the same point it would have reached without the lookup; stdout:\n%s", stdout)
			}
		})
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
	if !strings.Contains(stdout, promptRequestLine("peasant-labs", "village", 216)) {
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
