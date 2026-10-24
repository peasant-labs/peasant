package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// TestPushCmd_LastGoodOnATTYDoesNotEnterTheWizard pins the production call site
// of the --last-good wizard gate, which the predicate test alone cannot observe:
// forcing the gate's lastGood argument to a literal false leaves the whole
// package green, because every other last-good test runs without a terminal on
// stdin.
//
// The flag's contract is that it "skips the push wizard" on a hand-run
// interactive push. The wizard previews the session's available
// (latest-attempt) content while the flag publishes the last-good authority,
// and a session with a latest attempt is not even selectable in the wizard,
// so an interactive --last-good that reopened the wizard would make the
// documented exit unreachable for the very sessions the flag exists for.
//
// This drives the real command over a pseudo-terminal (the same harness the
// mounted prune-consent tests use) with the coexistence shape seeded, and
// asserts the run uploaded the last-good authority without entering the wizard.
// The quit key is queued on the terminal before the run: a run that wrongly
// opens the wizard cancels on it immediately and uploads nothing, which the
// upload assertion below catches. A correct run never reads stdin here at all:
// the private-visibility config keeps the public-consent prompt out of the path.
//
// The gate's last two arguments are symmetric under swap (!a && !b), so no
// behavioral test can distinguish a swapped pair; this pin's job is to catch a
// dropped or zeroed argument.
//
// NOT parallel: this test swaps the process-global os.Stdin onto the
// pseudo-terminal, and no parallel test may observe that swap.
func TestPushCmd_LastGoodOnATTYDoesNotEnterTheWizard(t *testing.T) {
	captured := &capturedPublish{parts: map[string]string{}}
	village := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/auth/me" {
			_ = json.NewEncoder(w).Encode(schema.VillageUser{ID: testutil.VillageFixtureAccountID})
			return
		}
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
	const sessionID = "aaaa7777-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	const authorityText = "the last-good authority body"
	const previewText = "the newer incomplete attempt body"
	cfgPath := seedUploadableSession(t, dir, sessionID)
	seedEntryCarrying(t, dir, sessionID, authorityText)
	seedLatestAttemptPreview(t, dir, sessionID, previewText)

	master, terminal := openTestTerminal(t)
	if _, err := master.WriteString("q\n"); err != nil {
		t.Fatalf("queue the wizard's quit key on the pseudo-terminal: %v", err)
	}
	oldStdin := os.Stdin
	os.Stdin = terminal
	t.Cleanup(func() { os.Stdin = oldStdin })

	stdout, stderr, err := executePushCmdSeparate(t, dir,
		[]string{"--config=" + cfgPath, "--last-good"})
	if err != nil {
		t.Fatalf("village push --last-good on a terminal: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	parts := captured.snapshot()
	if len(parts) == 0 {
		t.Fatalf("--last-good on a terminal uploaded nothing: the run entered the push wizard (and cancelled on the "+
			"queued quit key) instead of publishing the last-good authority. The flag's contract is that it skips the "+
			"wizard.\nstdout: %s\nstderr: %s", stdout, stderr)
	}
	uploaded := strings.Join(mapValues(parts), "\n")
	if !strings.Contains(uploaded, authorityText) {
		t.Errorf("the --last-good push did not upload the last-good authority content; parts: %v", partNames(parts))
	}
	if strings.Contains(uploaded, previewText) {
		t.Errorf("the --last-good push uploaded the newer incomplete attempt; parts: %v", partNames(parts))
	}
}
