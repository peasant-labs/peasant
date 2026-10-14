package ingest_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
)

type codexDeliveryFixtureCase struct {
	Records     string   `yaml:"records"`
	Content     []string `yaml:"content"`
	Retained    []string `yaml:"retained"`
	Redacted    []string `yaml:"redacted"`
	Submissions int64    `yaml:"submissions"`
	Turns       int      `yaml:"turns"`
	Refusal     string   `yaml:"refusal"`
}

func TestCodexDeliveryCandidateFixtures(t *testing.T) {
	for _, fixture := range loadCodexHistoryProjectionFixture(t).Cases {
		if fixture.Delivery == nil {
			continue
		}
		row := fixture.Delivery
		t.Run(fixture.Name, func(t *testing.T) {
			const sid = "11111111-1111-4111-8111-111111111111"
			data := "{\"type\":\"session_meta\",\"payload\":{\"id\":\"" + sid + "\",\"history_mode\":\"legacy\",\"thread_source\":\"user\"}}\n" + row.Records
			source := &fixtureCodexSource{authority: ingest.CodexSourceAuthority{StableThreadID: sid, Kind: ingest.CodexAuthorityDetachedFile, CurrentPointer: "current", PhysicalSourceID: "synthetic", HistoryMode: json.RawMessage(`"legacy"`)}, sources: map[string][]byte{"current": []byte(data)}}
			session := ingest.DiscoveredSession{SessionID: sid, Harness: ingest.HarnessCodex}
			history, err := ingest.CaptureCodexHistory(t.Context(), source, session, ingest.NewCodexRefRegistry(func(i int) schema.SourceEntryRef { return schema.SourceEntryRef(fmt.Sprintf("e_%d", i+1)) }))
			if err != nil {
				t.Fatal(err)
			}
			candidate, err := ingest.BuildCodexCandidate(ingest.CodexCandidateInput{History: history, Base: schema.UnifiedMetadata{SchemaVersion: ingest.CurrentSchemaVersion, SessionID: sid, ModelHarness: ingest.HarnessCodex}, GenerationID: "delivery"})
			if row.Refusal != "" {
				if err == nil {
					t.Fatal("malformed item authorized a candidate")
				}
				if !strings.Contains(err.Error(), row.Refusal) ||
					!strings.Contains(err.Error(), "source line 2") ||
					!strings.Contains(err.Error(), "item kind") {
					t.Fatalf("refusal = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			generation := candidate.V2.Generation
			if err := generation.Validate(); err != nil {
				t.Fatal(err)
			}
			var content, retained, redacted []string
			engine, err := redact.NewRedactor(redact.Standard, nil, redact.XDGPaths{})
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range generation.Main.Entries {
				if !ingest.IsRetainedUnknownCarrier(entry) {
					if entry.ContentPreview != nil {
						content = append(content, *entry.ContentPreview)
					}
					if entry.ToolCallID != nil || entry.ParentEntryID != nil {
						t.Fatal("delivery invented a tool parent")
					}
				}
				evidence, err := ingest.RetainedUnknownOf(entry)
				if err != nil {
					t.Fatal(err)
				}
				for _, record := range evidence {
					if record.Position.JSONPointer != "/payload/item/delivery" {
						t.Fatalf("retained pointer = %s", record.Position.JSONPointer)
					}
					retained = append(retained, string(record.Payload))
					value, err := ingest.RedactRetainedJSON(string(record.Payload), engine, 0)
					if err != nil {
						t.Fatal(err)
					}
					redacted = append(redacted, value)
				}
			}
			if !slices.Equal(content, row.Content) || !slices.Equal(retained, row.Retained) {
				t.Fatalf("content/evidence = %q / %q, want %q / %q", content, retained, row.Content, row.Retained)
			}
			if row.Redacted != nil && !slices.Equal(redacted, row.Redacted) {
				t.Fatalf("redacted delivery = %q, want %q", redacted, row.Redacted)
			}
			stats := generation.Metadata.Stats
			if stats.InputSubmissionCount == nil || *stats.InputSubmissionCount != row.Submissions || stats.TurnCount != row.Turns {
				t.Fatalf("counts = %+v", stats)
			}
			for _, relationship := range generation.Metadata.Relationships {
				if relationship.TargetLocalID != nil {
					t.Fatalf("delivery fabricated graph edge: %+v", relationship)
				}
			}
		})
	}
}
