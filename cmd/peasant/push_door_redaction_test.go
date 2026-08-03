package main

import (
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

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// doorSecret is planted in an indexed entry's content, which is the field this
// whole path exists to protect.
const doorSecret = "sk-ant-api03-CLIDOORKEY000000000000x"

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
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	preview := content
	if err := db.IndexSessionEntries(t.Context(), ingest.SessionID(sessionID), []schema.SessionEntry{{
		SessionID:      schema.SessionID(sessionID),
		EntryIndex:     1,
		Depth:          0,
		Role:           schema.RoleUser,
		Harness:        schema.Harness(defaults.HarnessClaudeCode),
		EntryType:      schema.EntryTypeText,
		ContentPreview: &preview,
	}}); err != nil {
		t.Fatalf("index entries: %v", err)
	}
}
