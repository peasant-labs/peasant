package store_test

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/schema"
)

// TestSessionTitlesBulkReadsOnlyStoredTitles pins the bounded title read the
// grouped sync page uses: one statement per batch, a stored title present under
// its session id, and a session with no stored title absent from the map rather
// than an empty string.
func TestSessionTitlesBulkReadsOnlyStoredTitles(t *testing.T) {
	t.Parallel()
	db := storetest.Open(t)
	ctx := context.Background()
	projectHash := schema.ProjectHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	ingested := int64(3000)
	for _, id := range []string{"s-title", "s-untitled"} {
		if err := db.InsertSessions(ctx, []ingest.StoreEntry{{
			Metadata: &schema.UnifiedMetadata{
				SchemaVersion: ingest.CurrentSchemaVersion,
				SessionID:     schema.SessionID(id),
				ModelHarness:  defaults.HarnessCodex,
				Model:         schema.ModelID("fixture-model"),
				HostSlug:      schema.HostSlug("fixture-host"),
				Project: schema.ProjectContext{
					Hash:     projectHash,
					Name:     "fixture-project",
					FilePath: "/fixture/project",
				},
				Timestamp: schema.TimestampInfo{Start: 1000, End: 2000, Ingested: &ingested},
				Source:    schema.SourceInfo{FilePath: "/fixture/session.jsonl", Format: schema.SourceFormatJSONL},
				Stats:     schema.SessionStats{TurnCount: 1},
			},
			Session: ingest.DiscoveredSession{Origin: sessionorigin.User},
		}}); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	const title = "refactor the ingest pipeline"
	if err := db.SaveMetrics(ctx, &ingest.SessionMetrics{
		SessionID: "s-title",
		QualityMetrics: schema.QualityMetrics{
			TitleGenerated: ptrString(title),
		},
	}); err != nil {
		t.Fatalf("save title: %v", err)
	}

	titles, err := db.SessionTitlesBulk(ctx, []string{"s-title", "s-untitled", "s-missing"})
	if err != nil {
		t.Fatalf("SessionTitlesBulk: %v", err)
	}
	if got := titles["s-title"]; got != title {
		t.Fatalf("s-title title = %q, want %q", got, title)
	}
	if _, present := titles["s-untitled"]; present {
		t.Fatal("a session with no stored title must be absent from the map")
	}
	if _, present := titles["s-missing"]; present {
		t.Fatal("an unknown session must be absent from the map")
	}
}

func ptrString(value string) *string { return &value }
