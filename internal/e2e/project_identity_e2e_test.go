//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
)

// TestPushProjectIdentityDefaultsRenderOnVillage drives a real push, through
// the real peasant binary, against a real Village, for two disposable
// repositories - one with a recognizable git remote, one without - and reads
// back the Village project page each pushed session belongs to
// (GET /api/v1/users/{username}/projects/{projectHash}).
//
// It proves the D8/D10 defaults end-to-end rather than at the mapper unit
// level: with no push.fields block in config.yaml (every tri-state field
// absent, so gitRemote/projectPath/projectName all resolve on), the
// remote-bearing repository's project renders the repository label
// (host:owner/repo) the Village derives from the pushed git remote, and the
// remote-less repository's project does NOT carry that raw git identity -
// it falls back to Peasant's privacy-safe label, because no remote means no
// label is sendable and the canonical project path is not itself a
// user-facing "project page" name.
func TestPushProjectIdentityDefaultsRenderOnVillage(t *testing.T) {
	fixture, err := loadProjectIdentityFixtures()
	if err != nil {
		t.Fatal(err)
	}
	want := fixture.Cases[0]

	bins := resolveVillageBinaries(t)
	peasantBin := buildPeasant(t)
	stack := provisionHarnessStack(t, bins)
	if stack.external {
		t.Skip("project identity evidence requires the harness-owned Village process and Postgres")
	}

	sandbox := newDisposableSandbox(t, peasantBin)

	remoteRepo := sandbox.initRepository(t, "identity-remote")
	runGit(t, sandbox.environment, remoteRepo, "remote", "add", "origin", want.Remote)
	remoteSource := reseedClaudeFixture(t, claudeReseed{
		Destination:              filepath.Join(sandbox.root, "fixtures", "identity-remote"),
		RecordedWorkingDirectory: remoteRepo,
		RootSessionID:            want.RemoteRootSessionID,
		SubagentSessionID:        want.RemoteSubagentSessionID,
	})

	noRemoteRepo := sandbox.initRepository(t, "identity-no-remote")
	noRemoteSource := reseedClaudeFixture(t, claudeReseed{
		Destination:              filepath.Join(sandbox.root, "fixtures", "identity-no-remote"),
		RecordedWorkingDirectory: noRemoteRepo,
		RootSessionID:            want.NoRemoteRootSessionID,
		SubagentSessionID:        want.NoRemoteSubagentSessionID,
	})

	writeProjectIdentityConfig(t, sandbox, remoteSource, noRemoteSource)
	_ = mintDemoCredentials(t, bins.setupDemo, stack.dsn, stack.villageURL, sandbox.configHome)
	runPeasantInSandbox(t, peasantBin, sandbox, "ingest", "--include-active")

	for _, repository := range []string{remoteRepo, noRemoteRepo} {
		pushProjectIdentityRepository(t, peasantBin, sandbox, repository)
	}

	dbPath := filepath.Join(sandbox.dataHome, string(defaults.AppName), "peasant.db")
	username := readDemoUsername(t, sandbox.configHome)
	apiKey := readAPIKey(t, sandbox.configHome)

	remoteProjectHash := readLocalProjectHash(t, dbPath, want.RemoteRootSessionID)
	remoteProject := getVillageProject(t, stack.villageURL, apiKey, username, remoteProjectHash)
	if remoteProject.DisplayName != want.ExpectedRemoteLabel {
		t.Errorf("remote-bearing project display name = %q, want the repository label %q (D8: sent by default)",
			remoteProject.DisplayName, want.ExpectedRemoteLabel)
	}
	if remoteProject.RemoteLabel != want.ExpectedRemoteLabel {
		t.Errorf("remote-bearing project remote label = %q, want %q", remoteProject.RemoteLabel, want.ExpectedRemoteLabel)
	}

	noRemoteProjectHash := readLocalProjectHash(t, dbPath, want.NoRemoteRootSessionID)
	noRemoteProject := getVillageProject(t, stack.villageURL, apiKey, username, noRemoteProjectHash)
	if noRemoteProject.RemoteLabel != "" {
		t.Errorf("remote-less project remote label = %q, want empty (no recognizable remote was ever pushed)", noRemoteProject.RemoteLabel)
	}
	// Positive, exact assertion (symmetric with the remote-bearing case
	// above): peasant sent no repository label and no remote for this
	// project (D8/D10), so village's project-identity resolver falls
	// through override/consented/remote to the path tier (see the fixture
	// comment) and renders the redacted local path verbatim. Pinning both
	// the exact expected display name and its name source catches a
	// regression that would drop the path, source it from the wrong tier,
	// or leak an unredacted string in its place.
	if noRemoteProject.NameSource != want.ExpectedNoRemoteNameSource {
		t.Errorf("remote-less project name source = %q, want %q (village had no override, consented name, or remote to resolve from, and a redacted path to fall back on)",
			noRemoteProject.NameSource, want.ExpectedNoRemoteNameSource)
	}
	if noRemoteProject.DisplayName != want.ExpectedNoRemoteDisplayName {
		t.Errorf("remote-less project display name = %q, want the redacted canonical path %q",
			noRemoteProject.DisplayName, want.ExpectedNoRemoteDisplayName)
	}
	// Defense in depth beyond the exact match above: an unredacted absolute
	// path can never legitimately appear here, whatever the display name
	// ends up being, because the safety net (NewPipeline's nil-redactor
	// refusal, D11) and this fixture's own <PATH> placeholder both exist
	// specifically to keep a real home directory off the wire.
	for _, rawSegment := range []string{"/home/", "/Users/"} {
		if strings.Contains(noRemoteProject.DisplayName, rawSegment) {
			t.Errorf("remote-less project display name = %q contains the raw path segment %q; "+
				"a real home directory leaked onto the village project page",
				noRemoteProject.DisplayName, rawSegment)
		}
	}
}

// pushProjectIdentityRepository pushes one repository's sessions with the
// real CLI, non-interactively, exactly as a user's automation would.
func pushProjectIdentityRepository(t *testing.T, binary string, sandbox disposableSandbox, repository string) {
	t.Helper()
	command := exec.Command(binary, "village", "push", "--non-interactive", "--yes", "--repository", repository)
	command.Env = sandbox.environment
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("push %s: %v\n%s", repository, err, output)
	}
}

// writeProjectIdentityConfig writes a config carrying NO push.fields block at
// all, so every tri-state field (gitRemote, projectPath, projectName) is
// absent and resolves on (D8) - the production default a fresh install
// ships with, not an opt-in.
func writeProjectIdentityConfig(t *testing.T, sandbox disposableSandbox, remoteSource, noRemoteSource string) {
	writeDisposableSandboxConfig(t, sandbox, fmt.Sprintf(`version: 1
redaction:
  level: standard
sources:
  claude-code:
    enabled: true
    paths: [%q, %q]
  opencode: {enabled: false}
  codex: {enabled: false}
  cursor: {enabled: false}
  strike: {enabled: false}
output:
  basePath: %q
push:
  visibility: private
`, remoteSource, noRemoteSource, sandbox.transcriptOutputPath()))
}

// villageProject is the shape of GET /api/v1/users/{username}/projects/{projectHash}
// this test cares about.
type villageProject struct {
	ProjectHash string `json:"project_hash"`
	DisplayName string `json:"project_display_name"`
	NameSource  string `json:"project_name_source"`
	RemoteLabel string `json:"project_remote_label"`
}

// getVillageProject reads the real Village project page API - the same
// contract the frontend project page renders from. The project identity is
// nested under the "project" key alongside the owner, transcripts, and
// collectives GetUserProject also returns.
func getVillageProject(t *testing.T, villageURL, apiKey, username, projectHash string) villageProject {
	t.Helper()
	status, body := villageAPIRequest(t, "GET", villageURL,
		fmt.Sprintf("/api/v1/users/%s/projects/%s", username, projectHash), apiKey, nil)
	if status != 200 {
		t.Fatalf("GET village project page for %s/%s: status = %d\n%s", username, projectHash, status, body)
	}
	var page struct {
		Project villageProject `json:"project"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("decode village project page response: %v\n%s", err, body)
	}
	return page.Project
}
