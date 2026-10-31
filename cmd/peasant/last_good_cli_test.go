package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/githooks"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// This file pins the CLI half of the explicit last-good exit: the flag exists on
// both surfaces, the push refuses it together with the hook flags, the consent
// warning is printed before the upload, a generated hook never carries it, and
// the export and push commands serve the last-good authority only when the flag
// is set — never the newer incomplete attempt. Every case seeds the coexistence
// shape (a last-good authority with a newer latest attempt), so a flag that
// stops reaching the selected read turns these tests red.

// seedLatestAttemptPreview keeps one incomplete preview attempt as the latest attempt over the session
// seedUploadableSession wrote, through the production activation the ingest
// pipeline uses. The store is opened with the same managed-generation writer the
// push command uses, rooted at the config's output base path, and the candidate
// carries the session's OWN recorded metadata so the activation guard sees the
// identity the row holds.
func seedLatestAttemptPreview(t *testing.T, dir, sessionID, previewText string) {
	t.Helper()
	seedLatestAttemptPreviewAtRoot(t, dir, filepath.Join(dir, "peasant-sync"), sessionID, previewText)
}

func seedLatestAttemptPreviewAtRoot(t *testing.T, dir, outputRoot, sessionID, previewText string) {
	t.Helper()
	dbPath := string(defaults.ResolveDBFilePathWith(dir))
	db, err := openPreparedStore(t, dbPath, mustGenerationStoreOptions(t, outputRoot)...)
	if err != nil {
		t.Fatalf("open generation store: %v", err)
	}
	defer db.Close()
	ctx := t.Context()
	id, err := ingest.NewSessionID(sessionID)
	if err != nil {
		t.Fatalf("session id: %v", err)
	}
	bundle, err := db.LoadPublicationInput(ctx, id)
	if err != nil {
		t.Fatalf("load the session's own metadata: %v", err)
	}
	metadata := bundle.Metadata
	if metadata.Stats.InputSubmissionCount == nil {
		// A complete generation has measured its submissions; the stored V1
		// capture may omit the count, and the preview candidate must still be a
		// valid complete generation.
		submissions := int64(0)
		metadata.Stats.InputSubmissionCount = &submissions
	}
	ref := schema.SourceEntryRef("latest-attempt-preview-entry")
	entry := schema.SessionEntry{
		SessionID: schema.SessionID(sessionID), EntryIndex: 0, Harness: metadata.ModelHarness,
		EntryType: schema.EntryTypeText, Role: schema.RoleUser, SourceEntryRef: ref, ContentPreview: &previewText,
	}
	generation := indexformat.V2{Generation: indexformat.Generation{
		ID:                   "gen-cli-latest-attempt",
		Completeness:         indexformat.GenerationCompletenessComplete,
		Metadata:             metadata,
		Main:                 indexformat.Partition{Entries: []schema.SessionEntry{entry}},
		Content:              []indexformat.ContentRecord{{Ref: ref}},
		SourceEvidenceDigest: strings.Repeat("a", 64),
	}}
	versions := testutil.HarvesterVersionsForSeed(t, metadata.ModelHarness)
	state, err := db.ReadIndexState(ctx, id)
	if err != nil {
		t.Fatalf("capture index state: %v", err)
	}
	identity := strings.Repeat("e", 64)
	// Real managed ingests already record the consumed pair and producer revision;
	// preserve those facts rather than claiming the synthetic V1 seed's identity.
	if state.ArtifactHash != nil {
		identity = *state.ArtifactHash
	}
	if state.IndexerVersion > versions.IndexerVersion {
		versions.IndexerVersion = state.IndexerVersion
	}
	outcome, err := db.ActivateGeneration(ctx, store.GenerationActivation{
		Generation:       generation,
		Blobs:            map[schema.SourceEntryRef][]byte{ref: []byte(previewText)},
		IndexerVersion:   versions.IndexerVersion,
		IndexedAtMs:      2,
		ExpectedState:    state,
		ArtifactIdentity: &identity,
		ContentCapture: ingest.SessionContentCaptureWrite{
			Status:           ingest.ContentCaptureIncomplete,
			SourceAuthority:  ingest.ContentSourceNewIngest,
			TranscriptOrigin: ingest.TranscriptOriginFile,
			CaptureFormat:    ingest.ContentCaptureFormatPreviewOnly,
			FailureCode:      ingest.ContentCaptureSourceRecordsOmitted,
			FailureMessage:   "oversized source records were omitted without a stand-in",
			CapturedAtMs:     2,
		},
	})
	if err != nil {
		t.Fatalf("retain latest attempt: %v", err)
	}
	if outcome.Disposition != ingest.ActivationLatestAttemptPreview {
		t.Fatalf("latest-attempt disposition = %v, want ActivationLatestAttemptPreview", outcome.Disposition)
	}
}

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
// local work. Both combinations are stated explicitly, one per check, so the
// case set is not an inline table.
func TestPushCmd_LastGoodRefusedWithHookFlags(t *testing.T) {
	t.Parallel()

	t.Run("auto-publish-event", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeTestCredentials(t, dir)
		stdout, stderr, err := executePushCmdSeparate(t, dir,
			[]string{"--non-interactive", "--last-good", "--require-auto-publish-rule", "--auto-publish-event", "post-commit"})
		assertLastGoodHookRefusal(t, "auto-publish-event", stdout, stderr, err)
	})

	t.Run("require-auto-publish-rule", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeTestCredentials(t, dir)
		stdout, stderr, err := executePushCmdSeparate(t, dir,
			[]string{"--non-interactive", "--last-good", "--require-auto-publish-rule"})
		assertLastGoodHookRefusal(t, "require-auto-publish-rule", stdout, stderr, err)
	})
}

// assertLastGoodHookRefusal checks the one refusal both hook-flag combinations
// must produce.
func assertLastGoodHookRefusal(t *testing.T, flag string, stdout, stderr string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("--last-good with --%s must be refused; stdout=%s stderr=%s", flag, stdout, stderr)
	}
	for _, want := range []string{"--last-good", "nothing was uploaded"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must state %q; got: %v", want, err)
		}
	}
	if strings.Contains(stdout, "Pushing to") {
		t.Errorf("nothing may be uploaded when the combination was refused; stdout: %s", stdout)
	}
}

// TestPushWizardEnabledSkipsForLastGood pins the extracted wizard gate: an
// ordinary interactive run opens the wizard, --non-interactive disables it, and
// --last-good disables it too (the wizard previews the newer attempt while the
// flag publishes the last-good authority). The flag does NOT imply
// --non-interactive, so the public-consent prompt still applies.
func TestPushWizardEnabledSkipsForLastGood(t *testing.T) {
	t.Parallel()
	if !pushWizardEnabled(false, false, true, false, false, false) {
		t.Fatal("an ordinary interactive run must open the push wizard")
	}
	if pushWizardEnabled(false, false, true, true, false, false) {
		t.Fatal("--non-interactive must disable the push wizard")
	}
	if pushWizardEnabled(false, false, true, false, true, false) {
		t.Fatal("--last-good must disable the push wizard: the wizard previews the newer attempt while the flag publishes the last-good authority")
	}
}

// TestPushCmd_LastGoodPrintsConsentWarningAndUploads drives the coexistence
// shape through `peasant village push`: the default run refuses the session and
// names the coexistence reason, while --last-good prints the consent warning and
// uploads the last-good authority — never the newer incomplete attempt.
func TestPushCmd_LastGoodPrintsConsentWarningAndUploads(t *testing.T) {
	t.Parallel()
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
	const sessionID = "eeee5555-eeee-4eee-8eee-eeeeeeeeeeee"
	const authorityText = "the last-good authority body"
	const previewText = "the newer incomplete attempt body"
	cfgPath := seedUploadableSession(t, dir, sessionID)
	seedEntryCarrying(t, dir, sessionID, authorityText)
	seedLatestAttemptPreview(t, dir, sessionID, previewText)

	// The default push refuses the session: the newer attempt must never be
	// published as the session, and the refusal names the way out.
	stdout, stderr, err := executePushCmdSeparate(t, dir,
		[]string{"--non-interactive", "--config=" + cfgPath})
	if err != nil {
		t.Fatalf("default push: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	if len(captured.snapshot()) != 0 {
		t.Fatalf("the default push uploaded a session it must refuse; parts: %v", captured.snapshot())
	}
	// The run's per-session refusal reason reaches stdout in the error summary
	// (the coexistence reason, never the generic needs-ingest text).
	if !strings.Contains(stdout, "incomplete preview") {
		t.Errorf("the default push did not report the coexistence reason; stdout: %s\nstderr: %s", stdout, stderr)
	}

	stdout, stderr, err = executePushCmdSeparate(t, dir,
		[]string{"--non-interactive", "--config=" + cfgPath, "--last-good"})
	if err != nil {
		t.Fatalf("push --last-good: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	if !strings.Contains(stderr, "--last-good publishes the last-good full transcript") {
		t.Errorf("the explicit consent warning is missing from stderr; stderr: %s", stderr)
	}
	parts := captured.snapshot()
	if len(parts) == 0 {
		t.Fatalf("--last-good uploaded nothing to the village; stdout: %s\nstderr: %s", stdout, stderr)
	}
	uploaded := strings.Join(mapValues(parts), "\n")
	if !strings.Contains(uploaded, authorityText) {
		t.Errorf("the --last-good push did not upload the last-good authority content; parts: %v", partNames(parts))
	}
	if strings.Contains(uploaded, previewText) {
		t.Errorf("the --last-good push uploaded the newer incomplete attempt; parts: %v", partNames(parts))
	}
}

// TestExportCmd_LastGoodServesAuthorityOverLatestAttempt drives the
// coexistence shape through `peasant export sessions`: the default export
// refuses the session and writes no file, while --last-good writes the last-good
// authority and never the newer incomplete attempt.
func TestExportCmd_LastGoodServesAuthorityOverLatestAttempt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const sessionID = "ffff6666-ffff-4fff-8fff-ffffffffffff"
	const authorityText = "the last-good authority body"
	const previewText = "the newer incomplete attempt body"
	seedUploadableSession(t, dir, sessionID)
	seedEntryCarrying(t, dir, sessionID, authorityText)
	seedLatestAttemptPreview(t, dir, sessionID, previewText)
	outDir := filepath.Join(dir, "exported")

	if _, err := executeWithDataDir(t, BuildExportCommand(), dir, []string{
		"sessions", "--session", sessionID, "--output-dir", outDir,
	}); err == nil {
		t.Fatal("the default export must refuse a session that keeps a newer incomplete attempt")
	}
	if _, err := os.Stat(filepath.Join(outDir, sessionID+".json")); !os.IsNotExist(err) {
		t.Fatalf("the default export wrote a file for a session it must refuse (stat err=%v)", err)
	}

	if _, err := executeWithDataDir(t, BuildExportCommand(), dir, []string{
		"sessions", "--session", sessionID, "--output-dir", outDir, "--last-good",
	}); err != nil {
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
	if !strings.Contains(string(written), authorityText) {
		t.Fatalf("the exported transcript does not carry the last-good authority content: %s", written)
	}
	if strings.Contains(string(written), previewText) {
		t.Fatalf("the exported transcript carries the newer incomplete attempt: %s", written)
	}
}

// mapValues returns every captured part value.
func mapValues(parts map[string]string) []string {
	values := make([]string, 0, len(parts))
	for _, value := range parts {
		values = append(values, value)
	}
	return values
}
