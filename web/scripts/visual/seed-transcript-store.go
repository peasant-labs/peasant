//go:build ignore

// Seed a synthetic multi-segment transcript into a throwaway data directory
// so the mounted capture can open the production route. No real transcript
// text, session id, or personal path is used.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

const (
	sessionID   = "11111111-1111-4111-8111-111111111111"
	projectHash = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	projectName = "synthetic-large"
	hostSlug    = "github.com-synthetic"
)

func main() {
	root, err := os.MkdirTemp("", "peasant-transcript-segments-")
	if err != nil {
		fail(err)
	}
	dbPath := string(defaults.ResolveDBFilePathWith(root))
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		fail(err)
	}
	db, err := store.Open(dbPath)
	if err != nil {
		fail(err)
	}
	ctx := context.Background()
	sid, err := ingest.NewSessionID(sessionID)
	if err != nil {
		fail(err)
	}
	hash, err := ingest.NewProjectHash(projectHash)
	if err != nil {
		fail(err)
	}
	host, err := ingest.NewHostSlug(hostSlug)
	if err != nil {
		fail(err)
	}
	model, err := ingest.NewModelID("claude-opus-4-6")
	if err != nil {
		fail(err)
	}
	source, err := ingest.NewResolvedPath("/tmp/synthetic-large/session.jsonl")
	if err != nil {
		fail(err)
	}
	const start = int64(1_700_000_000_000)
	ingested := start + 120_000
	entry := ingest.StoreEntry{
		Metadata: &ingest.UnifiedMetadata{
			SchemaVersion: ingest.CurrentSchemaVersion,
			SessionID:     sid,
			ModelHarness:  defaults.HarnessClaudeCode,
			Model:         model,
			HostSlug:      host,
			Timestamp: ingest.TimestampInfo{
				Start:    start,
				End:      start + 60_000,
				Ingested: &ingested,
			},
			Source: ingest.SourceInfo{FilePath: string(source), Format: ingest.SourceFormatJSONL},
			Project: ingest.ProjectInfo{
				Hash:     hash,
				Name:     projectName,
				FilePath: "/tmp/synthetic-large",
			},
			Stats: ingest.StatsInfo{
				TurnCount:     80,
				ToolCallCount: 0,
				DurationMs:    60_000,
				TokensIn:      1000,
				TokensOut:     500,
			},
			Version:     "2.1.14",
			Subagents:   []ingest.SubagentRef{},
			Diagnostics: ingest.DiagnosticsInfo{Warnings: []ingest.DiagnosticEntry{}},
		},
		Session: ingest.DiscoveredSession{
			SessionID:    sid,
			Harness:      defaults.HarnessClaudeCode,
			SourcePath:   source,
			SourceFormat: ingest.SourceFormatJSONL,
		},
	}
	if err := db.InsertSessions(ctx, []ingest.StoreEntry{entry}); err != nil {
		fail(err)
	}
	// Depth-0 indexes are the turn numbers the header and the boundary captions
	// both print. A tool row between them, or one parked past the last turn,
	// either opens a gap the captions cannot explain or falls outside the span
	// a segment read is allowed to return. The bulk lives in the turn text, so
	// the cut still crosses the segment budget and the read can reproduce it.
	entries := make([]schema.SessionEntry, 0, 80)
	// Spaces retain their wire size but collapse in normal HTML whitespace, so
	// the seed does not exceed the browser's maximum scrollable paint height.
	page := strings.Repeat(" ", 400_000)
	for turn := 0; turn < 80; turn++ {
		role := schema.RoleUser
		content := fmt.Sprintf("synthetic user turn %d. the reader can page from here.", turn)
		if turn%2 == 1 {
			role = schema.RoleAssistant
			content = fmt.Sprintf("synthetic assistant turn %d.%s end of turn.", turn, page)
		}
		timestamp := start + int64(turn)*1000
		entries = append(entries, schema.SessionEntry{
			SessionID:      sid,
			EntryIndex:     turn,
			Harness:        schema.HarnessClaudeCode,
			EntryType:      schema.EntryTypeText,
			Role:           role,
			TimestampMs:    &timestamp,
			ContentPreview: &content,
			Depth:          0,
		})
	}
	if err := testutil.WriteFullEntries(ctx, db, sid, entries); err != nil {
		fail(err)
	}
	if err := db.Close(); err != nil {
		fail(err)
	}
	encoded, err := json.Marshal(map[string]string{
		"dataDir":     root,
		"sessionId":   sessionID,
		"projectHash": projectHash,
		"projectName": projectName,
	})
	if err != nil {
		fail(err)
	}
	fmt.Println(string(encoded))
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "seed transcript store failed: %v\n", err)
	os.Exit(1)
}
