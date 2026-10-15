package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/githooks"
	"github.com/peasant-labs/peasant/internal/testutil"
)

// This file pins the CLI half of the explicit last-good exit: the flag exists on
// both surfaces, the push refuses it together with the hook flags, the consent
// warning is printed before the upload, a generated hook never carries it, and
// the export writes the last-good transcript for a session whose authority is
// the preserved certificate read.

// TestLastGoodIsNeverWrittenIntoAHookFile pins the second half of the automatic
// publication guarantee: a managed hook's generated command never carries
// --last-good, so an automatic publication can never send the last-good
// transcript in place of a newer incomplete attempt. The same render is asserted
// to carry the hook flags, so the check runs over the real rule-installed
// command rather than an empty one.
func TestLastGoodIsNeverWrittenIntoAHookFile(t *testing.T) {
	t.Parallel()
	binding := githooks.Binding{
		RequireAutoPublishRule: true,
		AutoPublishEvent:       githooks.EventPostCommit,
	}
	renders := map[string]string{
		"command line": githooks.CommandLine(binding),
		"argv":         strings.Join(githooks.RepositoryArgv("/repo", binding), " "),
	}
	for name, rendered := range renders {
		if !strings.Contains(rendered, "--require-auto-publish-rule") {
			t.Fatalf("%s render %q does not carry the hook flag, so this check would pass vacuously", name, rendered)
		}
		if strings.Contains(rendered, "--last-good") {
			t.Fatalf("%s render carries --last-good: %q; a hook must never publish the last-good transcript automatically", name, rendered)
		}
	}
}

// TestPushCmd_LastGoodRefusedWithHookFlags pins the refusal: --last-good is a
// hand-run choice and is refused together with either hook flag, before any
// local work.
func TestPushCmd_LastGoodRefusedWithHookFlags(t *testing.T) {
	t.Parallel()
	combinations := map[string][]string{
		"auto-publish-event":        {"--non-interactive", "--last-good", "--require-auto-publish-rule", "--auto-publish-event", "post-commit"},
		"require-auto-publish-rule": {"--non-interactive", "--last-good", "--require-auto-publish-rule"},
	}
	for name, args := range combinations {
		args := args
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeTestCredentials(t, dir)
			stdout, stderr, err := executePushCmdSeparate(t, dir, args)
			if err == nil {
				t.Fatalf("--last-good with %s must be refused; stdout=%s stderr=%s", name, stdout, stderr)
			}
			for _, want := range []string{"--last-good", "nothing was uploaded"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal must state %q; got: %v", want, err)
				}
			}
			if strings.Contains(stdout, "Pushing to") {
				t.Errorf("nothing may be uploaded when the combination was refused; stdout: %s", stdout)
			}
		})
	}
}

// TestPushCmd_LastGoodPrintsConsentWarningAndUploads drives a real --last-good
// push to completion against a stubbed village: the run succeeds, the explicit
// consent warning is on stderr, and the transcript actually reached the village.
func TestPushCmd_LastGoodPrintsConsentWarningAndUploads(t *testing.T) {
	t.Parallel()
	captured := &capturedPublish{parts: map[string]string{}}
	village := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !strings.Contains(r.URL.Path, "/transcripts/publish") {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		captured.record(t, r)
		receipt, err := testutil.AuthoritativePublishReceipt([]byte(captured.snapshot()["metadata"]), true)
		if err != nil {
			t.Errorf("build authoritative receipt: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(receipt)
	}))
	t.Cleanup(village.Close)

	dir := t.TempDir()
	writeTestCredentialsFor(t, dir, village.URL)
	const sessionID = "eeee5555-eeee-4eee-8eee-eeeeeeeeeeee"
	cfgPath := seedUploadableSession(t, dir, sessionID)
	seedEntryCarrying(t, dir, sessionID, "the last-good transcript body")

	stdout, stderr, err := executePushCmdSeparate(t, dir,
		[]string{"--non-interactive", "--config=" + cfgPath, "--last-good"})
	if err != nil {
		t.Fatalf("push --last-good: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	if !strings.Contains(stderr, "--last-good publishes the last-good full transcript") {
		t.Errorf("the explicit consent warning is missing from stderr; stderr: %s", stderr)
	}
	if len(captured.snapshot()) == 0 {
		t.Fatalf("--last-good uploaded nothing to the village; stdout: %s\nstderr: %s", stdout, stderr)
	}
}

// TestExportCmd_LastGoodExportsCertificateAuthority drives `peasant export
// sessions --last-good` for a session whose authority is the preserved
// certificate read and proves the transcript lands on disk. The default export
// refuses such a session only when a newer attempt is kept beside it; this case
// pins the flag's plumbing through the selected export seam.
func TestExportCmd_LastGoodExportsCertificateAuthority(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const sessionID = "ffff6666-ffff-4fff-8fff-ffffffffffff"
	seedUploadableSession(t, dir, sessionID)
	seedEntryCarrying(t, dir, sessionID, "the last-good certificate body")
	outDir := filepath.Join(dir, "exported")

	_, err := executeWithDataDir(t, BuildExportCommand(), dir, []string{
		"sessions", "--session", sessionID, "--output-dir", outDir, "--last-good",
	})
	if err != nil {
		t.Fatalf("export sessions --last-good: %v", err)
	}
	written, err := os.ReadFile(filepath.Join(outDir, sessionID+".json"))
	if err != nil {
		t.Fatalf("the export wrote no transcript: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(written, &payload); err != nil {
		t.Fatalf("exported transcript is not JSON: %v", err)
	}
	if !strings.Contains(string(written), "the last-good certificate body") {
		t.Fatalf("the exported transcript does not carry the recorded content: %s", written)
	}
}
