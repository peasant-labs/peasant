package main

import (
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/peasant-labs/peasant/internal/collectiveobservations"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
)

// doorSecret is planted in an indexed entry's content, which is the field this
// whole path exists to protect.
const doorSecret = "sk-ant-api03-CLIDOORKEY000000000000x"

func TestStoredSessionEntriesPublishedPreviewUsesFullCapture(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const sessionID = "cccc3333-cccc-4ccc-8ccc-cccccccccccc"
	seedUploadableSession(t, dir, sessionID)
	full := strings.Repeat("safe text ", 300) + "FULL-PREVIEW-TAIL " + doorSecret
	seedEntryCarrying(t, dir, sessionID, full)
	db, err := openPreparedStore(t, string(defaults.ResolveDBFilePathWith(dir)))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	redactor, err := redact.NewRedactor(redact.Standard, nil, redact.XDGPaths{})
	if err != nil {
		t.Fatal(err)
	}
	preview := push.NewPublishedTurns(storedSessionEntries(t.Context(), db), redactor)
	published, err := preview(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	turns := published.Turns
	if len(turns) != 1 || !strings.Contains(turns[0].Content, "FULL-PREVIEW-TAIL") || strings.Contains(turns[0].Content, doorSecret) || !strings.Contains(turns[0].Content, "ANTHROPIC_KEY") {
		t.Fatal("publication preview lost full tail or late redaction")
	}
	// A verified complete capture is the whole session, so this preview must not
	// carry the partial line. The seeded session is the mounted producer of that
	// state, read through the real store.
	if published.PartialNotice {
		t.Fatal("a complete capture reached the pane labelled partial, which would put the partial line on every session")
	}
	// Replacing the capture with a legacy preview must NOT silence the same
	// mounted reader. A capture that can no longer publish is still readable, and
	// refusing here removed the transcript from exactly the sessions a user opens
	// the previewer to inspect. Publication readiness is enforced at the publish
	// action instead.
	//
	// What must never change is the redaction above: this reader is the only thing
	// between a recorded secret and the screen, and the assertion that proves it
	// runs on the full capture, where there is content to inspect. Asserting the
	// absence of the secret again here would pass on an empty result and prove
	// nothing; the bounded-projection content belongs to the available-content
	// store read.
	if err := db.IndexSessionEntries(t.Context(), ingest.SessionID(sessionID), nil); err != nil {
		t.Fatal(err)
	}
	legacy, err := preview(sessionID)
	if err != nil {
		t.Fatalf("the preview refused a session it must still be able to show: %v", err)
	}
	// The capture that replaced it can no longer prove the whole session, so the
	// pane is told to say the preview is partial.
	if !legacy.PartialNotice {
		t.Fatal("a capture that can no longer prove the whole session was offered to the pane as complete")
	}
}

// TestStoredSessionEntriesPublishedPreviewServesTheLatestAttempt proves the
// share-wizard source read over the coexistence shape: a last-good authority
// with a newer incomplete attempt kept as its latest attempt. The wizard preview
// must render the latest attempt's own text and report the partial state, never
// the last-good authority's text.
func TestStoredSessionEntriesPublishedPreviewServesTheLatestAttempt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const sessionID = "cccc4444-cccc-4ccc-8ccc-cccccccccccc"
	seedUploadableSession(t, dir, sessionID)
	seedEntryCarrying(t, dir, sessionID, "the last-good authority body")
	seedLatestAttemptPreview(t, dir, sessionID, "the latest attempt carries newer prose")

	db, err := openPreparedStore(t, string(defaults.ResolveDBFilePathWith(dir)), mustGenerationStoreOptions(t, filepath.Join(dir, "peasant-sync"))...)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	stored, err := storedSessionEntries(t.Context(), db)(sessionID)
	if err != nil {
		t.Fatalf("read the stored entries: %v", err)
	}
	if !stored.PartialNotice {
		t.Fatal("a kept latest attempt must reach the wizard preview labelled partial")
	}
	joined := ""
	for _, entry := range stored.Entries {
		if entry.ContentPreview != nil {
			joined += *entry.ContentPreview
		}
	}
	if !strings.Contains(joined, "the latest attempt carries newer prose") {
		t.Fatalf("the wizard source read served %q, want the latest attempt's own text", joined)
	}
	if strings.Contains(joined, "the last-good authority body") {
		t.Fatalf("the wizard source read served the last-good authority's text: %q", joined)
	}

	redactor, err := redact.NewRedactor(redact.Standard, nil, redact.XDGPaths{})
	if err != nil {
		t.Fatal(err)
	}
	published, err := push.NewPublishedTurns(storedSessionEntries(t.Context(), db), redactor)(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !published.PartialNotice {
		t.Fatal("the published turns must carry the partial notice for a kept latest attempt")
	}
	content := ""
	for _, turn := range published.Turns {
		content += turn.Content
	}
	if !strings.Contains(content, "the latest attempt carries newer prose") {
		t.Fatalf("the published turns served %q, want the latest attempt's own text", content)
	}
	if strings.Contains(content, "the last-good authority body") {
		t.Fatalf("the published turns served the last-good authority's text: %q", content)
	}
}

// capturedPublish is every part of one real multipart publish, keyed by form
// name, as the village received it off the socket.
type capturedPublish struct {
	mu    sync.Mutex
	parts map[string]string
}

// TestPushCmd_TheCLIDoorGivesThePipelineARedactor pins the OUTERMOST link of the
// production push path: that `peasant village push` hands push.NewPipeline a
// redactor at all.
//
// Everything inside that link was already guarded and none of it reaches this.
// TestPipeline_PublishedBodyIsRedacted builds its own pipeline and hands it its
// own redactor, so it pins what the pipeline does with one it is GIVEN; the
// forbidden-source-text corpus forbids a nil at the inner call site and is not
// scoped to cmd/peasant at all. Measured on this tree before this test existed:
// passing a nil redactor here left all 37 packages green, and every push would
// have published metadata, entries and transcript content as recorded while the
// record still printed "redacted at standard on upload".
//
// It asserts BEHAVIOUR over the real HTTP request rather than the spelling of
// the call, because a source guard is satisfied by a nil that arrives through a
// variable - which is the edit that produced the defect it would be guarding.
// Every part of the multipart body is swept, not the one named "metadata",
// because a publish carries the transcript text in more than one of them.
func TestPushCmd_TheCLIDoorGivesThePipelineARedactor(t *testing.T) {
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
	const sessionID = "dddd4444-dddd-4ddd-8ddd-dddddddddddd"
	cfgPath := seedUploadableSession(t, dir, sessionID)
	seedEntryCarrying(t, dir, sessionID, "here is the key "+doorSecret+" thanks")

	stdout, stderr, err := executePushCmdSeparate(t, dir,
		[]string{"--non-interactive", "--quiet", "--config=" + cfgPath})
	if err != nil {
		t.Fatalf("push: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	db, err := openPreparedStore(t, string(defaults.ResolveDBFilePathWith(dir)))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	observations := collectiveobservations.New(collectiveobservations.Dependencies{Store: db, ConfigHome: dir})
	history, err := observations.History(t.Context(), schema.LocalContributionHistoryQuery{Kind: schema.LocalContributionEvidenceReceipts, Page: 1, Limit: 1})
	if err != nil || history.Total != 1 || len(history.Receipts) != 1 {
		t.Fatalf("mounted CLI did not record its actual confirmed effect: %+v %v", history, err)
	}
	input, err := db.LoadPublicationInput(t.Context(), ingest.SessionID(sessionID))
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := db.Publication(t.Context(), village.URL, testutil.VillageFixtureAccountID.String(), input.ReceiptProjectHash, sessionID)
	if err != nil || terminal == nil {
		t.Fatalf("authoritative receipt missing: %+v %v", terminal, err)
	}
	if observed := history.Receipts[0]; observed.Operation != schema.CollectiveOperationPublish || observed.TranscriptID != terminal.Receipt.TranscriptID || observed.Outcome != schema.LocalConfirmedContributionPublished || observed.CollectiveID != nil || observed.SourceReceiptID == nil || *observed.SourceReceiptID != string(terminal.Receipt.RequestOperationFingerprint) {
		t.Fatalf("mounted CLI publication evidence is not its actual upload: %+v", observed)
	}

	parts := captured.snapshot()
	if len(parts) == 0 {
		t.Fatalf("the village received no publish, so this test cannot say anything about what the door sends.\n"+
			"stdout: %s\nstderr: %s", stdout, stderr)
	}
	sawContent := false
	for name, body := range parts {
		// Non-vacuity keyed on the REDACTED FORM OF THE PLANT, not on a marker.
		//
		// This looked for "contentPreview" or "sessionDetail". sessionDetail is
		// present in EVERY publish, including one carrying no entries at all - and
		// pipeline.go degrades to nil entries when ListEntries errors, so a
		// publish with nothing to leak is production-plausible, not hypothetical.
		// The sweep below would then have passed over a body that could not have
		// failed it.
		//
		// The placeholder can only appear if the planted content reached the wire
		// AND was redacted, so it proves the subject ran.
		if strings.Contains(body, "ANTHROPIC_KEY") {
			sawContent = true
		}
		if strings.Contains(body, doorSecret) {
			t.Errorf("the %q part of the real publish carries the planted secret VERBATIM. `peasant village push` built "+
				"its pipeline without a redactor, so every push publishes as recorded while the record printed to the "+
				"user says it was redacted at standard.\n%s:\n%s", name, name, body)
		}
	}
	// Non-vacuity: a publish that carried no transcript content at all would
	// satisfy the sweep above by having nothing to leak.
	if !sawContent {
		t.Errorf("no part of the captured publish carries a redacted placeholder, so the planted content never reached "+
			"the wire and the sweep above ran over a body that could not have leaked. A publish with no entries is "+
			"production-plausible - ListEntries degrades to nil on error - so this is the difference between the guard "+
			"passing and the guard running. parts: %v", partNames(parts))
	}
}

func (c *capturedPublish) record(t *testing.T, r *http.Request) {
	t.Helper()
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		defer c.mu.Unlock()
		c.parts["body"] = string(body)
		return
	}
	reader := multipart.NewReader(r.Body, params["boundary"])
	c.mu.Lock()
	defer c.mu.Unlock()
	for {
		part, partErr := reader.NextPart()
		if partErr != nil {
			return
		}
		data, _ := io.ReadAll(part)
		name := part.FormName()
		if name == "" {
			name = part.FileName()
		}
		c.parts[name] = string(data)
	}
}

func (c *capturedPublish) snapshot() map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]string, len(c.parts))
	for k, v := range c.parts {
		out[k] = v
	}
	return out
}

func partNames(parts map[string]string) []string {
	names := make([]string, 0, len(parts))
	for name := range parts {
		names = append(names, name)
	}
	return names
}

// seedEntryCarrying indexes one real session entry whose content carries the
// planted text, through the production writer the ingest pipeline uses.
func seedEntryCarrying(t *testing.T, dir, sessionID, content string) {
	t.Helper()
	dbPath := string(defaults.ResolveDBFilePathWith(dir))
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := openPreparedStore(t, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	preview := content
	input, err := db.LoadPublicationInput(t.Context(), ingest.SessionID(sessionID))
	if err != nil {
		t.Fatal(err)
	}
	testutil.SeedReadyPublication(t, db, &input.Metadata, []schema.SessionEntry{{
		SessionID:      schema.SessionID(sessionID),
		EntryIndex:     1,
		Depth:          0,
		Role:           schema.RoleUser,
		Harness:        schema.Harness(defaults.HarnessClaudeCode),
		EntryType:      schema.EntryTypeText,
		ContentPreview: &preview,
	}})
}
