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
			Name  string                  `yaml:"name"`
			Level schema.ThinkingLevel    `yaml:"level"`
			Raw   schema.ThinkingLevelRaw `yaml:"raw"`
		} `yaml:"cases"`
		RequiredNames []string `yaml:"requiredNames"`
	}
	if err := testutil.DecodeFixtureYAML(thinkingLevelExportYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, tc := range fixture.Cases {
		if tc.Name == "" || seen[tc.Name] {
			t.Fatalf("empty or duplicate case %q", tc.Name)
		}
		seen[tc.Name] = true
		t.Run(tc.Name, func(t *testing.T) {
			ctx := context.Background()
			db := storetest.Open(t)
			sid := schema.SessionID(testutil.TestSessionUUID)
			ingested := int64(3)
			meta := &schema.UnifiedMetadata{SessionID: sid, ModelHarness: ingest.HarnessClaudeCode, HostSlug: testutil.TestHostSlug, Project: schema.ProjectContext{Hash: "thinking-project", Name: "fixture"}, Timestamp: schema.TimestampInfo{Start: 1, End: 2, Ingested: &ingested}, ThinkingLevel: tc.Level, ThinkingLevelRaw: tc.Raw}
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
			if len(payload.Turns) != len(entries) || payload.Turns[0].ThinkingLevel != "" || payload.Turns[0].ThinkingLevelRaw != "" {
				t.Fatalf("invented per-turn evidence: %+v", payload.Turns)
			}
		})
	}
	if len(fixture.RequiredNames) == 0 {
		t.Fatal("missing required-name inventory")
	}
	for _, name := range fixture.RequiredNames {
		if !seen[name] {
			t.Fatalf("missing required case %q", name)
		}
	}
}
