package api

import (
	"encoding/json"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
)

// TestBuildSyncGroupedRowStartTimeUTC pins the grouped sync chooser's wire
// startTime to a canonical UTC instant through the real buildSyncGroupedRow
// construction (not a marshaled mock). time.UnixMilli returns the machine's
// LOCAL time, so without an explicit .UTC() the chooser row would render a
// zone offset that the published @peasant-labs/schema decoder rejects.
func TestBuildSyncGroupedRowStartTimeUTC(t *testing.T) {
	t.Parallel()
	const (
		projectHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		startMs     = 1_700_000_000_000
		wantUTC     = "2023-11-14T22:13:20Z"
	)
	row := ingest.PushSessionRow{
		SessionID:    "aaaaaaaa-1111-4111-8111-111111111111",
		ModelHarness: "claude-code",
		ModelID:      "claude-opus-4-6",
		HostSlug:     "utc-host",
		ProjectHash:  projectHash,
		ProjectName:  "utc-project",
		StartMs:      startMs,
		DurationMs:   60_000,
		TokensTotal:  100,
		TurnCount:    5,
		ToolCalls:    2,
	}
	ev := store.GroupingEvidenceRow{SessionID: row.SessionID}

	summary, _, err := buildSyncGroupedRow(row, "pushable", ev, "preview")
	if err != nil {
		t.Fatalf("buildSyncGroupedRow: %v", err)
	}

	raw, err := json.Marshal(summary)
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}
	var wire struct {
		StartTime string `json:"startTime"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal summary wire: %v", err)
	}
	if wire.StartTime != wantUTC {
		t.Errorf("wire startTime = %q, want canonical UTC %q (a local-zone offset fails the schema decoder)", wire.StartTime, wantUTC)
	}
}