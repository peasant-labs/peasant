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

func TestPushRequiresCoherentRetainedInput(t *testing.T) {
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
	// A file-only publication advances retained content without advancing its
	// SQL mirror/index. Both real preflight and dry-run must refuse that pair.
	publisher, err := ingest.NewArtifactPublisher(fs, "/sync", ingest.ArtifactPublisherOptions{})
	if err != nil {
		t.Fatal(err)
	}
	observation, err := publisher.Observe(t.Context(), ingest.DiscoveredSession{SessionID: sid, Harness: meta.ModelHarness}, path)
	if err != nil {
		t.Fatal(err)
	}
	meta.ContentHash = ""
	meta.MetadataHash = ""
	raw, err = json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := ingest.NewManagedArtifact(raw, append(data, '\n'))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.Publish(t.Context(), ingest.ArtifactPublication{Artifact: replacement, Observation: observation}); err != nil {
		t.Fatal(err)
	}
	negotiations, uploads := transport.SchemaVersionCalls, len(transport.Calls)
	if result := run(false); result.Errors != 1 || result.New != 0 || result.Updated != 0 {
		t.Fatalf("unmirrored input passed real preflight: %+v", result)
	}
	if result := run(true); result.Errors != 1 || result.New != 0 || result.Updated != 0 {
		t.Fatalf("unmirrored input received a successful forecast: %+v", result)
	}
	after, err := db.Publication(t.Context(), baseCreds().VillageURL, baseCreds().UserID, schema.ProjectHash(meta.Project.Hash), string(sid))
	if err != nil || !reflect.DeepEqual(before, after) || transport.SchemaVersionCalls != negotiations || len(transport.Calls) != uploads {
		t.Fatalf("refused input negotiated, uploaded, or changed its receipt: %v", err)
	}
}
