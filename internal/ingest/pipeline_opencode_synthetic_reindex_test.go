package ingest_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// openCodeSyntheticTaskText is the text OpenCode's task tool injects into the
// parent session when a background task finishes. A reader must never see it as
// something a person typed.
const openCodeSyntheticTaskText = `<task id="ses_ffe6cce48ffefzK8rzeIauasaI" state="completed"> ` +
	`<summary>Background task completed: Research serving runtimes</summary> ` +
	`<task_result>the runtimes report</task_result> </task>`

// writeOpenCodeTaskResultMessage writes the injected message and its one text
// part into an OpenCode storage tree. synthetic selects whether the stored part
// carries OpenCode's own synthetic marker, which is the only difference between
// a machine-authored injection and a message a person wrote.
func writeOpenCodeTaskResultMessage(t *testing.T, fs *testutil.MemFS, sessionID, msgID, partID string, synthetic bool) {
	t.Helper()
	// The message carries no content of its own, so the stored preview comes
	// from the injected text part and names the entry this test is about.
	addOpenCodeMessageNoContent(t, fs, sessionID, msgID, string(ingest.RoleUser))
	part := map[string]any{"text": openCodeSyntheticTaskText}
	if synthetic {
		part["synthetic"] = true
	}
	addOpenCodePartTyped(t, fs, msgID, partID, "text", part)
}

// TestPipeline_ReindexReclassifiesAnInjectedOpenCodeTaskResult proves the
// user-visible repair on the real projection path: a session already stored with
// the injected background-task result standing as one of the user's own turns is
// reclassified to a system entry when it is re-indexed from OpenCode's storage.
//
// It drives the REAL pipeline with the REAL OpenCode indexer and the REAL store,
// twice. The first run stores the session while the part carries no synthetic
// marker, so the message is genuinely stored as a user turn - the state every
// already-ingested session is in. The stored part is then replaced with the row
// OpenCode really writes, which carries synthetic: true, and a --reindex --force
// run re-extracts from that storage. Asserting on entries read back from the
// store, rather than on the indexer's return value, is what proves the stale user
// row is replaced rather than merely shadowed.
//
// Mutation proof: dropping the Synthetic read from inspectOpenCodeSemanticParts
// leaves the re-indexed entry at role=user, entry_type=text, and the second phase
// fails.
func TestPipeline_ReindexReclassifiesAnInjectedOpenCodeTaskResult(t *testing.T) {
	mfs := testutil.NewMemFS()
	const projectHash = "openprojecthash"
	const msgID = "msg_task_result"
	const partID = "prt_task_result"

	session := setupOpenCodeFixture(t, mfs, testutil.TestSessionUUID, projectHash)
	// A real user turn precedes the injection, so the assertion below can name
	// which entry changed and prove the other one did not.
	addOpenCodeMessage(t, mfs, testutil.TestSessionUUID, "msg_person", string(ingest.RoleUser), 10, 0)
	addOpenCodePart(t, mfs, "msg_person", "prt_person")
	writeOpenCodeTaskResultMessage(t, mfs, testutil.TestSessionUUID, msgID, partID, false)

	sid := session.SessionID
	meta := makeMinimalMeta(t, string(sid))
	meta.Source.FilePath = session.SourcePath.String()
	meta.Source.Format = schema.SourceFormat(ingest.SourceFormatJSON)
	meta.ModelHarness = schema.Harness(defaults.HarnessOpenCode)
	session.ModTime = time.Now().Add(-1 * time.Hour)

	database, err := store.Open(t.TempDir() + "/peasant.db")
	if err != nil {
		t.Fatalf("open the analytics store: %v", err)
	}
	defer database.Close()

	sources := map[ingest.Harness]ingest.SourceConfig{
		defaults.HarnessOpenCode: {Enabled: true, Paths: []ingest.ResolvedPath{session.OriginalRoot}},
	}
	adapters := map[ingest.Harness]ingest.AdapterFactory{
		defaults.HarnessOpenCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{sid: meta},
		),
	}
	indexers := ingest.WithIndexers(map[ingest.Harness]ingest.TranscriptIndexer{
		defaults.HarnessOpenCode: ingest.NewOpenCodeIndexer(mfs),
	})

	ingestConfig := makePipelineConfig(testOutputDir)
	ingestConfig.Sources = sources
	pipeline, err := ingest.NewPipeline(mfs, testutil.DefaultGitResolver(), adapters, ingestConfig,
		indexers, ingest.WithStore(database), ingest.WithMetricsStore(database))
	if err != nil {
		t.Fatalf("build the ingest pipeline: %v", err)
	}
	if _, err := pipeline.Run(context.Background()); err != nil {
		t.Fatalf("run the first ingest: %v", err)
	}

	before := findOpenCodeEntryByPreview(t, database, sid, openCodeSyntheticTaskText)
	// Non-vacuity: the second phase can only prove a reclassification if the
	// first phase really left the injected message standing as a user turn.
	if before.Role != ingest.RoleUser || before.EntryType != ingest.EntryTypeText {
		t.Fatalf("the already-stored session must hold the injected result as a user text turn for this test to say "+
			"anything; got role=%q entry_type=%q", before.Role, before.EntryType)
	}

	// OpenCode's storage carries the marker. Re-index reads it from there.
	writeOpenCodeTaskResultMessage(t, mfs, testutil.TestSessionUUID, msgID, partID, true)

	reindexConfig := makePipelineConfig(testOutputDir)
	reindexConfig.Sources = sources
	reindexConfig.Reindex = true
	reindexConfig.Force = true
	reindexer, err := ingest.NewPipeline(mfs, testutil.DefaultGitResolver(), adapters, reindexConfig,
		indexers, ingest.WithStore(database), ingest.WithMetricsStore(database))
	if err != nil {
		t.Fatalf("build the reindex pipeline: %v", err)
	}
	if _, err := reindexer.Run(context.Background()); err != nil {
		t.Fatalf("run the reindex: %v", err)
	}

	after := findOpenCodeEntryByPreview(t, database, sid, openCodeSyntheticTaskText)
	if after.Role != ingest.RoleSystem || after.EntryType != ingest.EntryTypeSystem {
		t.Errorf("after re-indexing, the injected background-task result is stored as role=%q entry_type=%q, want "+
			"role=%q entry_type=%q; a reader still sees the harness's own XML as a turn they typed",
			after.Role, after.EntryType, ingest.RoleSystem, ingest.EntryTypeSystem)
	}
	person := findOpenCodeEntryByPreview(t, database, sid, "Hello from user")
	if person.Role != ingest.RoleUser {
		t.Errorf("the person's own message is stored as role=%q after re-indexing, want %q; the reclassification must "+
			"reach the injected message only", person.Role, ingest.RoleUser)
	}
}

// findOpenCodeEntryByPreview reads the stored entries back and returns the one
// depth-0 message entry whose preview holds needle. Reading from the store is
// what makes the assertion about what a reader is served.
func findOpenCodeEntryByPreview(t *testing.T, database *store.Store, sid ingest.SessionID, needle string) schema.SessionEntry {
	t.Helper()
	entries, err := database.ListEntries(context.Background(), sid)
	if err != nil {
		t.Fatalf("read the stored entries: %v", err)
	}
	var found []schema.SessionEntry
	for _, entry := range entries {
		if entry.Depth != 0 || entry.ContentPreview == nil {
			continue
		}
		if strings.Contains(*entry.ContentPreview, needle) {
			found = append(found, entry)
		}
	}
	if len(found) != 1 {
		for _, entry := range entries {
			preview := ""
			if entry.ContentPreview != nil {
				preview = *entry.ContentPreview
			}
			t.Logf("entry index=%d depth=%d role=%s type=%s preview=%q", entry.EntryIndex, entry.Depth, entry.Role, entry.EntryType, preview)
		}
		t.Fatalf("the store holds %d depth-0 entries previewing %q, want exactly 1", len(found), needle)
	}
	return found[0]
}
