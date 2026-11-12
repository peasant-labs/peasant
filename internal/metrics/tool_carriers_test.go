package metrics_test

import (
	_ "embed"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/tool_carriers.yaml
var toolCarriersYAML []byte

func TestComputeClaudeToolCarriers(t *testing.T) {
	t.Parallel()
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name    string `yaml:"name"`
			Entries []struct {
				Index      int            `yaml:"index"`
				Harness    schema.Harness `yaml:"harness"`
				Role       schema.Role    `yaml:"role"`
				Depth      int            `yaml:"depth"`
				Parent     *int           `yaml:"parent"`
				EntryType  string         `yaml:"type"`
				HasToolUse bool           `yaml:"hasToolUse"`
				Tools      string         `yaml:"tools"`
				Call       string         `yaml:"call"`
			} `yaml:"entries"`
			Tools     int `yaml:"tools"`
			Subagents int `yaml:"subagents"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(toolCarriersYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, c := range fixture.Cases {
		if c.Name == "" || seen[c.Name] {
			t.Fatalf("invalid tool-carrier fixture %q", c.Name)
		}
		seen[c.Name] = true
		t.Run(c.Name, func(t *testing.T) {
			sid := mustSessionID(t, testutil.TestSessionUUID)
			entries := make([]schema.SessionEntry, len(c.Entries))
			for i, source := range c.Entries {
				entry := schema.SessionEntry{SessionID: sid, EntryIndex: source.Index, Harness: source.Harness, Role: source.Role, Depth: source.Depth, ParentIndex: source.Parent, EntryType: ingest.EntryTypeText}
				if source.EntryType != "" {
					switch source.EntryType {
					case "text":
						entry.EntryType = ingest.EntryTypeText
					case "tool_use":
						entry.EntryType = ingest.EntryTypeToolUse
					default:
						t.Fatalf("entry %d names unknown type %q; a carrier is text, a call is tool_use", source.Index, source.EntryType)
					}
				}
				if source.HasToolUse {
					entry.HasToolUse = true
				}
				if source.Tools != "" {
					tools := source.Tools
					entry.ToolNamesCSV = &tools
					entry.HasToolUse = true
					entry.EntryType = ingest.EntryTypeToolUse
				}
				if source.Call != "" {
					call := source.Call
					entry.ToolCallID = &call
				}
				if entry.Harness == ingest.HarnessPi {
					// Pi rows carry their typed evidence marker; without it
					// the metrics read refuses the row before counting.
					extra, err := ingest.EncodePiExtra(ingest.PiExtra{Kind: ingest.PiExtraState, Harness: ingest.HarnessPi})
					if err != nil {
						t.Fatalf("encode pi evidence: %v", err)
					}
					entry.Extra = extra
				}
				entries[i] = entry
			}
			result := runMetrics(t, sid, entries)
			if result.ToolCalls == nil || *result.ToolCalls != c.Tools {
				t.Errorf("tool calls=%v, want %d", result.ToolCalls, c.Tools)
			}
			if result.SubagentCount == nil || *result.SubagentCount != c.Subagents {
				t.Errorf("subagents=%v, want %d", result.SubagentCount, c.Subagents)
			}
		})
	}
	if err := testutil.RequireFixtureNames("tool carriers", "case", fixture.RequiredNames, seen); err != nil {
		t.Fatal(err)
	}
}
