package api_test

import (
	"context"
	_ "embed"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// stubDetailReader serves one canned snapshot through the production
// SnapshotReader contract, with or without generation support, so the wiring
// test proves which production path DetailPayload selects.
type stubDetailReader struct {
	snapshot  indexformat.ReadSnapshot
	err       error
	supported bool
}

func (s stubDetailReader) WithSessionSnapshot(_ context.Context, _ schema.SessionID, fn func(indexformat.ReadSnapshot) error) error {
	if s.err != nil {
		return s.err
	}
	return fn(s.snapshot)
}

func (s stubDetailReader) GenerationSnapshotsSupported() bool { return s.supported }

type stubDetailResolver struct {
	blobs    map[schema.SourceEntryRef][]byte
	corrupt  schema.SourceEntryRef
	resolved *int
}

func (s stubDetailResolver) ReadFullContent(_ context.Context, _ schema.SessionID, _ string, record indexformat.ContentRecord) ([]byte, error) {
	if s.resolved != nil {
		*s.resolved++
	}
	if record.Ref == s.corrupt {
		return nil, errors.New("stub resolver: managed content fails its integrity digest")
	}
	blob, ok := s.blobs[record.Ref]
	if !ok {
		return nil, errors.New("stub resolver: no captured blob")
	}
	return blob, nil
}

func detailWiringSnapshot(t *testing.T) (indexformat.ReadSnapshot, map[schema.SourceEntryRef][]byte) {
	t.Helper()
	sessionID := schema.SessionID("45454545-4545-4545-4545-454545454548")
	harness := schema.HarnessCodex
	entries := []schema.SessionEntry{
		{SessionID: sessionID, EntryIndex: 0, Harness: harness, EntryType: schema.EntryTypeText, Role: schema.RoleUser, SourceEntryRef: "e_wire_u"},
		{SessionID: sessionID, EntryIndex: 1, Harness: harness, EntryType: schema.EntryTypeText, Role: schema.RoleAssistant, SourceEntryRef: "e_wire_a"},
	}
	inputCount := int64(1)
	metadata := schema.UnifiedMetadata{
		SchemaVersion: 1,
		SessionID:     sessionID,
		ModelHarness:  harness,
		Stats:         schema.SessionStats{TurnCount: 2, InputSubmissionCount: &inputCount},
		Purpose:       schema.SessionPurposeInteraction,
	}
	session := schema.SessionDetailPayload{
		ID:                   string(sessionID),
		Harness:              harness,
		TurnCount:            2,
		InputSubmissionCount: &inputCount,
		Purpose:              schema.SessionPurposeInteraction,
	}
	blobs := map[schema.SourceEntryRef][]byte{
		"e_wire_u": []byte("wire input"),
		"e_wire_a": []byte("wire reply"),
	}
	snapshot := indexformat.ReadSnapshot{
		Session:      session,
		Metadata:     metadata,
		GenerationID: "g-wire",
		Completeness: indexformat.GenerationCompletenessComplete,
		IndexVersion: 2,
		Main:         indexformat.Partition{Entries: entries},
		Content: []indexformat.ContentRecord{
			{Ref: "e_wire_u", RelativeBlob: "c_wire_u.blob", ByteLength: 10, Digest: "wire"},
			{Ref: "e_wire_a", RelativeBlob: "c_wire_a.blob", ByteLength: 10, Digest: "wire"},
		},
	}
	return snapshot, blobs
}

func TestDetailPayloadSnapshotWiring(t *testing.T) {
	snapshot, blobs := detailWiringSnapshot(t)
	legacyCalls := 0
	legacy := func(context.Context, string) (*schema.SessionDetailPayload, error) {
		legacyCalls++
		return &schema.SessionDetailPayload{ID: "legacy"}, nil
	}

	payload, err := api.DetailPayloadWithReader(context.Background(), stubDetailReader{snapshot: snapshot, supported: true}, stubDetailResolver{blobs: blobs}, "45454545-4545-4545-4545-454545454548", legacy)
	if err != nil {
		t.Fatalf("snapshot detail: %v", err)
	}
	if legacyCalls != 0 {
		t.Fatal("snapshot path must not consult the legacy callback")
	}
	if len(payload.Turns) != 2 || payload.Turns[0].Content != "wire input" || payload.Turns[1].Content != "wire reply" {
		t.Fatalf("snapshot detail turns = %+v, want hydrated wire turns", payload.Turns)
	}
	if payload.InputSubmissionCount == nil || *payload.InputSubmissionCount != 1 {
		t.Fatalf("snapshot detail inputSubmissionCount = %v, want 1", payload.InputSubmissionCount)
	}

	legacySnapshot := snapshot
	legacySnapshot.IndexVersion = 1
	legacySnapshot.GenerationID = ""
	legacySnapshot.Main = indexformat.Partition{}
	legacySnapshot.Content = nil
	if _, err := api.DetailPayloadWithReader(context.Background(), stubDetailReader{snapshot: legacySnapshot, supported: true}, stubDetailResolver{blobs: blobs}, "45454545-4545-4545-4545-454545454548", legacy); err != nil {
		t.Fatalf("legacy snapshot: %v", err)
	}
	if legacyCalls != 1 {
		t.Fatalf("legacy snapshot must fall back to the legacy path once, got %d calls", legacyCalls)
	}

	corruptCalls := 0
	corruptLegacy := func(context.Context, string) (*schema.SessionDetailPayload, error) {
		corruptCalls++
		return &schema.SessionDetailPayload{ID: "legacy"}, nil
	}
	if _, err := api.DetailPayloadWithReader(context.Background(), stubDetailReader{snapshot: snapshot, supported: true}, stubDetailResolver{blobs: blobs, corrupt: "e_wire_u"}, "45454545-4545-4545-4545-454545454548", corruptLegacy); err == nil {
		t.Fatal("corrupt managed content must fail the detail read")
	}
	if corruptCalls != 0 {
		t.Fatal("corrupt managed content must never fall back to truncated legacy content")
	}

	if _, err := api.DetailPayloadWithReader(context.Background(), stubDetailReader{snapshot: snapshot}, stubDetailResolver{blobs: blobs}, "45454545-4545-4545-4545-454545454548", legacy); err != nil {
		t.Fatalf("unsupported reader: %v", err)
	}
	if legacyCalls != 2 {
		t.Fatalf("unsupported reader must use the legacy path, got %d legacy calls", legacyCalls)
	}
}

func TestDetailPayloadPreviewSplit(t *testing.T) {
	// Preview-only completeness serves an explicit bounded preview from the
	// same valid snapshot, never a managed-to-legacy fallback. Corruption
	// fails closed with no preview and no legacy.
	sessionID := schema.SessionID("45454545-4545-4545-4545-454545454548")
	harness := schema.HarnessCodex
	bounded := "bounded preview prose"
	previewEntries := []schema.SessionEntry{
		{SessionID: sessionID, EntryIndex: 0, Harness: harness, EntryType: schema.EntryTypeText, Role: schema.RoleUser, ContentPreview: &bounded},
	}
	previewSnapshot := indexformat.ReadSnapshot{
		Session: schema.SessionDetailPayload{
			ID: sessionID.String(), Harness: harness, TurnCount: 1,
			Purpose: schema.SessionPurposeInteraction,
		},
		Metadata: schema.UnifiedMetadata{
			SchemaVersion: 1, SessionID: sessionID, ModelHarness: harness,
			Stats:   schema.SessionStats{TurnCount: 1},
			Purpose: schema.SessionPurposeInteraction,
		},
		GenerationID: "g-preview",
		Completeness: indexformat.GenerationCompletenessIncompleteNew,
		IndexVersion: 2,
		Main:         indexformat.Partition{Entries: previewEntries},
	}
	previewCalls := 0
	previewLegacy := func(context.Context, string) (*schema.SessionDetailPayload, error) {
		previewCalls++
		return &schema.SessionDetailPayload{ID: "legacy"}, nil
	}
	preview, err := api.DetailPayloadWithReader(context.Background(), stubDetailReader{snapshot: previewSnapshot, supported: true}, stubDetailResolver{}, string(sessionID), previewLegacy)
	if err != nil {
		t.Fatalf("preview-only completeness must serve a bounded preview: %v", err)
	}
	if previewCalls != 0 {
		t.Fatal("preview-only completeness must not fall back to legacy content")
	}
	if preview.Diagnostics == nil || !preview.Diagnostics.Partial {
		t.Fatalf("preview must carry partial diagnostics: %+v", preview.Diagnostics)
	}
	if len(preview.Turns) != 1 || preview.Turns[0].Content != bounded {
		t.Fatalf("preview turns = %+v, want bounded preview prose", preview.Turns)
	}

	corruptEntries := []schema.SessionEntry{
		{SessionID: sessionID, EntryIndex: 0, Harness: harness, EntryType: schema.EntryTypeText, Role: schema.RoleUser, ContentPreview: &bounded},
		{SessionID: sessionID, EntryIndex: 0, Harness: harness, EntryType: schema.EntryTypeText, Role: schema.RoleAssistant, ContentPreview: &bounded},
	}
	corruptSnapshot := previewSnapshot
	corruptSnapshot.GenerationID = "g-corrupt"
	corruptSnapshot.Main = indexformat.Partition{Entries: corruptEntries}
	corruptCalls := 0
	corruptLegacy := func(context.Context, string) (*schema.SessionDetailPayload, error) {
		corruptCalls++
		return &schema.SessionDetailPayload{ID: "legacy"}, nil
	}
	if _, err := api.DetailPayloadWithReader(context.Background(), stubDetailReader{snapshot: corruptSnapshot, supported: true}, stubDetailResolver{}, string(sessionID), corruptLegacy); err == nil {
		t.Fatal("corrupt preview evidence must fail the detail read")
	}
	if corruptCalls != 0 {
		t.Fatal("corrupt preview evidence must never fall back to legacy content")
	}
}

// seedMountedSession inserts the retained session row a generation activation
// needs into a copy of the golden database.
func seedMountedSession(t *testing.T, dbPath, sid string) {
	t.Helper()
	conn, err := sqlite.OpenConn(dbPath, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	exec := func(query string, args ...any) {
		if err := sqlitex.Execute(conn, query, &sqlitex.ExecOptions{Args: args}); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT OR IGNORE INTO host_slugs(opaque_id, host_slug) VALUES('host-mnt','host-mnt')`)
	exec(`INSERT OR IGNORE INTO projects(project_hash, canonical_cwd, canonical_remote) VALUES('proj-mnt','/tmp/mnt','github.com/mnt/mnt')`)
	exec(`INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format, schema_version) VALUES(?, 'claude-code','model-mnt','host-mnt','proj-mnt',1,2,3,'/tmp/mnt/source.jsonl','jsonl',11)`, sid)
}

// mountedV2 builds a minimal self-contained V2 candidate: text entries with
// content previews and source refs, content records for each ref, and blobs
// carrying the same prose the entries preview.
func mountedV2(t *testing.T, sid schema.SessionID, genID, completeness string, texts []string) (indexformat.V2, map[schema.SourceEntryRef][]byte) {
	t.Helper()
	entries := make([]schema.SessionEntry, 0, len(texts))
	content := make([]indexformat.ContentRecord, 0, len(texts))
	blobs := make(map[schema.SourceEntryRef][]byte, len(texts))
	roles := []schema.Role{schema.RoleUser, schema.RoleAssistant}
	refs := []schema.SourceEntryRef{"e_mnt_u", "e_mnt_a"}
	for i, text := range texts {
		ref := refs[i%len(refs)]
		preview := text
		role := roles[i%len(roles)]
		entries = append(entries, schema.SessionEntry{
			SessionID: sid, EntryIndex: i, Harness: schema.Harness("claude-code"),
			Role: role, EntryType: schema.EntryTypeText,
			ContentPreview: &preview, SourceEntryRef: ref,
		})
		content = append(content, indexformat.ContentRecord{Ref: ref})
		blobs[ref] = []byte(text)
	}
	var inputCount *int64
	if indexformat.GenerationCompleteness(completeness) == indexformat.GenerationCompletenessComplete {
		c := int64(1)
		inputCount = &c
	}
	generation := indexformat.Generation{
		ID:           genID,
		Completeness: indexformat.GenerationCompleteness(completeness),
		Metadata: schema.UnifiedMetadata{
			SchemaVersion: ingest.CurrentSchemaVersion,
			SessionID:     sid,
			ModelHarness:  schema.Harness("claude-code"),
			Stats:         schema.SessionStats{TurnCount: len(entries), InputSubmissionCount: inputCount},
		},
		Main:                 indexformat.Partition{Entries: entries},
		Content:              content,
		SourceEvidenceDigest: "mmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmm",
		TitleRefs:            []schema.SourceEntryRef{entries[0].SourceEntryRef},
	}
	return indexformat.V2{Generation: generation}, blobs
}

// activateMountedCandidate assesses and activates one V2 candidate through the
// real store, returning the committed generation identifier.
func activateMountedCandidate(t *testing.T, db *store.Store, sid schema.SessionID, genID, completeness string, texts []string) {
	t.Helper()
	v2, blobs := mountedV2(t, sid, genID, completeness, texts)
	assessment, err := ingest.AssessCapture(ingest.CaptureFacts{
		Harness: ingest.Harness("claude-code"), Result: v2,
		Policy: ingest.CaptureFreshCandidate, Authoritative: true,
	})
	if err != nil {
		t.Fatalf("mounted assessment: %v", err)
	}
	capture, err := assessment.ContentCapture(ingest.ContentSourceNewIngest, ingest.TranscriptOriginFile, 1)
	if err != nil {
		t.Fatalf("mounted capture: %v", err)
	}
	outcome, err := db.ActivateNativeGeneration(t.Context(), ingest.NativeGenerationActivation{
		Generation: v2, Blobs: blobs,
		IndexerVersion: 1, IndexedAtMs: 1, ContentCapture: capture,
	})
	if err != nil || outcome.Disposition != ingest.ActivationCommittedNow {
		t.Fatalf("mounted activation: outcome=%+v err=%v", outcome, err)
	}
}

// TestDetailPayloadWithReaderRealStore rounds one complete and one preview-only
// generation through the real store into the mounted DetailPayloadWithReader
// path: the complete generation serves hydrated full turns, the preview-only
// generation serves the explicit bounded preview with partial diagnostics, and
// neither consults the legacy callback.
func TestDetailPayloadWithReaderRealStore(t *testing.T) {
	ctx := context.Background()
	completeID := "45454545-4545-4545-4545-454545454550"
	previewID := "45454545-4545-4545-4545-454545454551"

	openMountedStore := func(t *testing.T, sid string) *store.Store {
		t.Helper()
		dbPath := storetest.CopyGoldenDB(t)
		seedMountedSession(t, dbPath, sid)
		root := filepath.Join(t.TempDir(), "artifacts")
		artifacts, err := store.NewOSGenerationArtifactStore(root)
		if err != nil {
			t.Fatal(err)
		}
		locks, err := store.NewFileSessionLocker(root)
		if err != nil {
			t.Fatal(err)
		}
		db, err := store.Open(dbPath, store.WithIndexFormats(store.V2IndexFormat()), store.WithGenerationArtifacts(artifacts, locks))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db
	}

	t.Run("complete", func(t *testing.T) {
		db := openMountedStore(t, completeID)
		sid := schema.SessionID(completeID)
		activateMountedCandidate(t, db, sid, "gen-mnt-complete", "complete",
			[]string{"mounted full prose one", "mounted full prose two"})
		legacyCalls := 0
		legacy := func(context.Context, string) (*schema.SessionDetailPayload, error) {
			legacyCalls++
			return &schema.SessionDetailPayload{ID: "legacy"}, nil
		}
		payload, err := api.DetailPayloadWithReader(ctx, db, db, completeID, legacy)
		if err != nil {
			t.Fatalf("mounted complete detail: %v", err)
		}
		if legacyCalls != 0 {
			t.Fatal("mounted complete detail must not consult the legacy callback")
		}
		if len(payload.Turns) != 2 || payload.Turns[0].Content != "mounted full prose one" || payload.Turns[1].Content != "mounted full prose two" {
			t.Fatalf("mounted complete turns = %+v, want hydrated full prose", payload.Turns)
		}
		if payload.Diagnostics != nil && payload.Diagnostics.Partial {
			t.Fatalf("mounted complete detail must not carry partial diagnostics: %+v", payload.Diagnostics)
		}
	})

	t.Run("preview", func(t *testing.T) {
		db := openMountedStore(t, previewID)
		sid := schema.SessionID(previewID)
		activateMountedCandidate(t, db, sid, "gen-mnt-preview", "incomplete_new",
			[]string{"mounted preview prose"})
		legacyCalls := 0
		legacy := func(context.Context, string) (*schema.SessionDetailPayload, error) {
			legacyCalls++
			return &schema.SessionDetailPayload{ID: "legacy"}, nil
		}
		payload, err := api.DetailPayloadWithReader(ctx, db, db, previewID, legacy)
		if err != nil {
			t.Fatalf("mounted preview detail: %v", err)
		}
		if legacyCalls != 0 {
			t.Fatal("mounted preview detail must not fall back to legacy content")
		}
		if payload.Diagnostics == nil || !payload.Diagnostics.Partial {
			t.Fatalf("mounted preview must carry partial diagnostics: %+v", payload.Diagnostics)
		}
		if len(payload.Turns) != 1 || payload.Turns[0].Content != "mounted preview prose" {
			t.Fatalf("mounted preview turns = %+v, want bounded preview prose", payload.Turns)
		}
	})
}

// stubLinkProvider exercises the non-store subscription path: it offers exactly
// one session through the preserved SessionByID conversion.
type stubLinkProvider struct {
	api.DataProvider
	session *ingest.Session
}

func (s stubLinkProvider) SessionByID(_ context.Context, _ string) (*ingest.Session, error) {
	return s.session, nil
}

func TestSessionDetailSubscriptionKeepsLegacyProviderPath(t *testing.T) {
	session := &ingest.Session{
		ID:       schema.SessionID("45454545-4545-4545-4545-454545454548"),
		Harness:  defaults.HarnessClaudeCode,
		Turns:    []ingest.Turn{{Index: 0, Role: ingest.RoleUser, Content: "hello", EntryType: schema.EntryTypeText}},
		Metadata: ingest.SessionMetadata{TurnCount: 1},
	}
	detail, err := api.SessionDetailReadForProvider(context.Background(), stubLinkProvider{session: session}, string(session.ID))
	if err != nil {
		t.Fatalf("legacy subscription detail: %v", err)
	}
	if len(detail.Turns) != 1 || detail.Turns[0].Content != "hello" {
		t.Fatalf("legacy subscription detail turns = %+v", detail.Turns)
	}
}

func TestResolveStoredTargetsIgnoresSelection(t *testing.T) {
	listedID := "71111111-1111-4111-8111-111111111111"
	hiddenID := "72222222-2222-4222-8222-222222222222"
	missingID := "73333333-3333-4333-8333-333333333333"
	projectHash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	db := openTestStore(t)
	listedClone := filepath.Join(t.TempDir(), "listed-clone")
	hiddenClone := filepath.Join(t.TempDir(), "hidden-clone")
	if err := os.MkdirAll(listedClone, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(hiddenClone, 0o755); err != nil {
		t.Fatal(err)
	}
	listed := makeStoreEntry(t, listedID, projectHash, "github.com-example", defaults.HarnessClaudeCode, 1705276800000, 1000, 500, "listed-project", 10, 5, 60000)
	listed.Metadata.Git.Worktree = &listedClone
	hidden := makeStoreEntry(t, hiddenID, projectHash, "github.com-example", defaults.HarnessClaudeCode, 1705276860000, 2000, 800, "hidden-project", 20, 8, 120000)
	hidden.Metadata.Git.Worktree = &hiddenClone
	if err := db.InsertSessions(t.Context(), []ingest.StoreEntry{listed, hidden}); err != nil {
		t.Fatalf("seed stored-target sessions: %v", err)
	}
	api.MarkStoredSessionsIndexed(t, db)

	policy, err := sessionvisibility.New(config.SelectionConfig{
		Mode: config.SelectionModeSelected,
		Harnesses: map[string]config.SelectionHarnessConfig{
			"claude-code": {Projects: []config.ProjectSelection{{ClonePaths: []string{listedClone}}}},
		},
	})
	if err != nil {
		t.Fatalf("build stored-target selection policy: %v", err)
	}
	provider := api.NewStoreDataProvider(db, policy)

	discovery, err := provider.SessionSummaries(t.Context())
	if err != nil {
		t.Fatalf("discovery summaries: %v", err)
	}
	for _, summary := range discovery {
		if summary.ID == hiddenID {
			t.Fatal("unselected session must stay out of discovery lists")
		}
	}

	targets, err := provider.ResolveStoredTargets(t.Context(), []string{listedID, hiddenID, missingID})
	if err != nil {
		t.Fatalf("resolve stored targets: %v", err)
	}
	byID := make(map[string]api.StoredTarget, len(targets))
	for _, target := range targets {
		byID[target.ID] = target
	}
	if !byID[listedID].Found || byID[listedID].Summary == nil {
		t.Fatal("listed session must resolve as a stored target")
	}
	if !byID[hiddenID].Found || byID[hiddenID].Summary == nil || byID[hiddenID].Summary.ID != hiddenID {
		t.Fatal("stored-but-unselected session must resolve as a stored target: selection is not access")
	}
	if byID[missingID].Found || byID[missingID].Summary != nil {
		t.Fatal("missing session must resolve to an explicit unavailable target")
	}
	if len(targets) != 3 {
		t.Fatalf("resolved targets = %d, want 3 in request order", len(targets))
	}
	for i, want := range []string{listedID, hiddenID, missingID} {
		if targets[i].ID != want {
			t.Fatalf("target %d = %q, want %q in request order", i, targets[i].ID, want)
		}
	}
}
