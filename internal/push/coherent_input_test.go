package push_test

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"path/filepath"
)

type changedCandidateOriginStore struct{ *store.Store }

func (s *changedCandidateOriginStore) AllPushableSessions(ctx context.Context) ([]ingest.PushSessionRow, error) {
	rows, err := s.Store.AllPushableSessions(ctx)
	for i := range rows {
		rows[i].SessionOrigin = sessionorigin.Agent.String()
	}
	return rows, err
}

var _ push.PipelineStore = (*changedCandidateOriginStore)(nil)

func TestPushRequiresCoherentDatabaseInput(t *testing.T) {
	db := storetest.Open(t)
	fs := testutil.NewMemFS()
	sid := ingest.SessionID(testutil.TestSessionUUID)
	seedMemFS(t, fs, testutil.TestHostSlug, string(sid), defaults.HarnessClaudeCode)
	path := ingest.SessionMetadataPath("/sync", testutil.TestHostSlug, string(sid), "")
	raw, err := fs.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var meta ingest.UnifiedMetadata
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	body := strings.Repeat("retained full content ", defaults.ContentPreviewLimit)
	data, err := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": body}})
	if err != nil {
		t.Fatal(err)
	}
	storetest.SeedManagedInput(t, db, fs, "/sync", meta, data)
	fullIndexer := ingest.NewClaudeIndexer(fs, ingest.WithClaudeFullContent(true))
	fullEntries, err := fullIndexer.IndexTranscriptBytes(t.Context(), ingest.DiscoveredSession{SessionID: sid, Harness: meta.ModelHarness}, data)
	if err != nil {
		t.Fatal(err)
	}
	testutil.SeedReadyPublication(t, db, &meta, fullEntries)
	if err := db.UpdateOriginState(t.Context(), sid, sessionorigin.User.String(), 1); err != nil {
		t.Fatal(err)
	}
	transport := &testutil.StubPublisher{}
	run := func(dry bool) *push.PushResult {
		t.Helper()
		pipeline, err := push.NewPipeline(&changedCandidateOriginStore{Store: db}, transport, baseCreds(), baseTestConfig(), fs,
			push.PipelineConfig{Force: true, DryRun: dry}, &testutil.NoopRedactor{}, &bytes.Buffer{})
		if err != nil {
			t.Fatal(err)
		}
		result, err := pipeline.Run(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	if result := run(false); result.New != 1 || result.Errors != 0 {
		t.Fatalf("coherent publication failed: %+v sessions=%+v", result, result.Sessions)
	}
	var content schema.TranscriptContent
	if err := json.Unmarshal(transport.Calls[0].TranscriptBody, &content); err != nil {
		t.Fatal(err)
	}
	if content.SessionDetail == nil || content.SessionDetail.SessionOrigin != schema.SessionOrigin(sessionorigin.User) ||
		len(content.SessionDetail.Turns) == 0 || content.SessionDetail.Turns[0].Content != body {
		t.Fatal("publication did not use full captured content and the snapshot's stored origin")
	}
	before, err := db.Publication(t.Context(), baseCreds().VillageURL, baseCreds().UserID, schema.ProjectHash(meta.Project.Hash), string(sid))
	if err != nil || before == nil {
		t.Fatalf("coherent publication receipt missing: %v", err)
	}
	// A file-only rewrite of the saved pair does not replace the authoritative
	// database capture. It must not prevent a forecast of the stored content.
	// Write the replacement pair straight to disk, the state a file-only tool
	// would leave; no database row is recorded.
	replacementTranscript := append(data, '\n')
	meta.ContentHash = schema.ComputeTranscriptHash(replacementTranscript)
	meta.MetadataHash = schema.ComputeMetadataHash(&meta)
	raw, err = json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := ingest.NewManagedArtifact(raw, replacementTranscript)
	if err != nil {
		t.Fatal(err)
	}
	sessionDir := filepath.Dir(path)
	if err := fs.WriteFile(filepath.Join(sessionDir, string(sid)+"--transcript."+string(replacement.Metadata.Source.Format)), replacement.Transcript, defaults.PrivateFilePerm); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile(path, replacement.MetadataJSON, defaults.PrivateFilePerm); err != nil {
		t.Fatal(err)
	}
	if result := run(true); result.Errors != 0 || result.Updated == 0 {
		t.Fatalf("sidecar-only change blocked authoritative database forecast: %+v", result)
	}
	// An uncertified database replacement, however, invalidates completeness.
	if err := db.IndexSessionEntries(t.Context(), sid, fullEntries); err != nil {
		t.Fatal(err)
	}
	negotiations, uploads := transport.SchemaVersionCalls, len(transport.Calls)
	if result := run(false); result.Errors != 1 || result.New != 0 || result.Updated != 0 {
		t.Fatalf("uncertified database input passed real preflight: %+v", result)
	}
	if result := run(true); result.Errors != 1 || result.New != 0 || result.Updated != 0 {
		t.Fatalf("uncertified database input received a successful forecast: %+v", result)
	}
	after, err := db.Publication(t.Context(), baseCreds().VillageURL, baseCreds().UserID, schema.ProjectHash(meta.Project.Hash), string(sid))
	if err != nil || !reflect.DeepEqual(before, after) || transport.SchemaVersionCalls != negotiations || len(transport.Calls) != uploads {
		t.Fatalf("refused input negotiated, uploaded, or changed its receipt: %v", err)
	}
}
