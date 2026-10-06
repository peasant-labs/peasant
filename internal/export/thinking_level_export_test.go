package export_test

import (
	"context"
	_ "embed"
	"errors"
	"testing"

	"github.com/peasant-labs/peasant/internal/export"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/transcript"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/thinking_level_export.yaml
var thinkingLevelExportYAML []byte

func TestExportSessionThinkingLevel(t *testing.T) {
	var fixture struct {
		Cases []struct {
			Name    string                  `yaml:"name"`
			Level   schema.ThinkingLevel    `yaml:"level"`
			Raw     schema.ThinkingLevelRaw `yaml:"raw"`
			History []struct {
				Level schema.ThinkingLevel    `yaml:"level"`
				Raw   schema.ThinkingLevelRaw `yaml:"raw"`
			} `yaml:"history"`
		} `yaml:"cases"`
		RequiredNames []string `yaml:"requiredNames"`
	}
	if err := testutil.DecodeFixtureYAML(thinkingLevelExportYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.RequiredNames) == 0 {
		t.Fatal("thinking level export fixture declares no requiredNames inventory; name every case the fixture must retain")
	}
	names := make([]string, len(fixture.Cases))
	for i, tc := range fixture.Cases {
		names[i] = tc.Name
	}
	if err := testutil.ValidateRequiredNames(testutil.RequiredNamesManifest{RequiredNames: fixture.RequiredNames}, names, "thinking level export"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			ctx := context.Background()
			db := storetest.Open(t)
			sid := schema.SessionID(testutil.TestSessionUUID)
			ingested := int64(3)
			meta := &schema.UnifiedMetadata{SessionID: sid, ModelHarness: ingest.HarnessClaudeCode, HostSlug: testutil.TestHostSlug, Project: schema.ProjectContext{Hash: "thinking-project", Name: "fixture"}, Timestamp: schema.TimestampInfo{Start: 1, End: 2, Ingested: &ingested}, ThinkingLevel: tc.Level, ThinkingLevelRaw: tc.Raw}
			for _, change := range tc.History {
				meta.ThinkingLevelHistory = append(meta.ThinkingLevelHistory, schema.ThinkingLevelChange{Level: change.Level, Raw: change.Raw})
			}
			meta.Source = schema.SourceInfo{FilePath: "/fixture.jsonl", Format: schema.SourceFormatJSONL}
			if err := db.InsertSessions(ctx, []ingest.StoreEntry{{Metadata: meta}}); err != nil {
				t.Fatal(err)
			}
			content := "answer without a turn-level observation"
			entries := []schema.SessionEntry{{SessionID: sid, EntryIndex: 1, Harness: ingest.HarnessClaudeCode, Role: schema.RoleAssistant, EntryType: schema.EntryTypeText, ContentPreview: &content}}
			if err := testutil.WriteFullEntries(ctx, db, sid, entries); err != nil {
				t.Fatal(err)
			}
			snapshot, blobs := exportWiringSnapshot()
			snapshot.IndexVersion = 1
			snapshot.Metadata.ThinkingLevel, snapshot.Metadata.ThinkingLevelRaw = tc.Level, tc.Raw
			snapshot.Metadata.ThinkingLevelHistory = meta.ThinkingLevelHistory
			if _, err := export.ExportSnapshotPayload(ctx, stubExportReader{snapshot: snapshot, supported: true}, stubExportResolver{blobs: blobs}, sid); !errors.Is(err, transcript.ErrLegacySnapshot) {
				t.Fatalf("V1 snapshot error=%v, want ErrLegacySnapshot", err)
			}
			payload, err := export.ExportSession(ctx, db, testutil.NewMemFS(), string(sid))
			if err != nil {
				t.Fatal(err)
			}
			if payload.ThinkingLevel != tc.Level || payload.ThinkingLevelRaw != tc.Raw {
				t.Fatalf("exported seed=(%q,%q), want (%q,%q)", payload.ThinkingLevel, payload.ThinkingLevelRaw, tc.Level, tc.Raw)
			}
			if len(payload.ThinkingLevelHistory) != len(tc.History) {
				t.Fatalf("exported history=%+v, want %+v", payload.ThinkingLevelHistory, tc.History)
			}
			for i, change := range payload.ThinkingLevelHistory {
				if change.Level != tc.History[i].Level || change.Raw != tc.History[i].Raw {
					t.Fatalf("exported history[%d]=(%q,%q), want %+v", i, change.Level, change.Raw, tc.History[i])
				}
			}
			if len(payload.Turns) != len(entries) || payload.Turns[0].ThinkingLevel != "" || payload.Turns[0].ThinkingLevelRaw != "" {
				t.Fatalf("invented per-turn evidence: %+v", payload.Turns)
			}
		})
	}
}
