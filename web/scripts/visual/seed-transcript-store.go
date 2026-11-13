//go:build ignore

// Seed a synthetic multi-segment transcript into a fresh temporary data
// directory so the mounted capture can open the production route. No real
// transcript text, session id, or personal path is used.
//
//	go run ./web/scripts/visual/seed-transcript-store.go
//
// The seeder creates the data directory itself, under TMPDIR, and prints its
// path as JSON. It accepts no directory to write into: a supplied -data-dir or
// a positional path is refused, so an accidental real data directory can never
// be opened, migrated or modified. The caller starts the binary with the
// printed dataDir and removes that directory when its server has stopped.
package main

import (
	"context"
	"encoding/json"
	"flag"
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
	sessionID         = "11111111-1111-4111-8111-111111111111"
	projectHash       = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	projectName       = "synthetic-large"
	hostSlug          = "github.com-synthetic"
	firstActionParent = 31
	lastActionParent  = 51
	// 80 entries fold to 69 turns: the 11 tool children ride their parents.
	// 5 of those turns are system rows and 4 are tool rows, so 60 are
	// conversation turns.
	foldedTurns       = 69
	conversationTurns = 60
)

// Top-level rows that are turns but not conversation turns. They sit on both
// sides of the middle segments, so a segment's conversation turn position is
// never its row index and the mounted captions can be told from turn ids.
var (
	systemRows = map[int]bool{4: true, 14: true, 24: true, 60: true, 70: true}
	toolRows   = map[int]bool{8: true, 18: true, 64: true, 74: true}
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "seed transcript store failed: %v\n", err)
		os.Exit(1)
	}
}

// run seeds one synthetic transcript into a fresh temporary data directory.
//
// It takes no directory to write into. os.MkdirTemp creates the directory atomically with a random
// name under TMPDIR; because no supplied path is ever opened, an accidental real data directory
// cannot be opened, migrated or modified. A failed run keeps the directory it created and reports
// it: without a handle to the directory, a path cannot prove it is still the one this run made, so
// deleting it could remove a replacement.
func run() (err error) {
	dataDir := flag.String("data-dir", "", "not accepted: the seeder creates its own temporary data directory")
	flag.Parse()
	if *dataDir != "" {
		return fmt.Errorf("-data-dir is not accepted, but %q was given. The seeder creates a fresh temporary data directory itself and never opens a path it is given, so a real store cannot be opened, migrated or modified. Nothing was created or written. Run it without -data-dir and start the binary with the dataDir it prints", *dataDir)
	}
	if flag.NArg() > 0 {
		return fmt.Errorf("positional arguments are not accepted, but %q was given. The seeder creates a fresh temporary data directory itself and never opens a path it is given, so a real store cannot be opened, migrated or modified. Nothing was created or written. Run it without a path and start the binary with the dataDir it prints", flag.Args())
	}
	root, err := os.MkdirTemp("", "peasant-transcript-segments-")
	if err != nil {
		return fmt.Errorf("the seeder could not create its temporary data directory under %q: %w. Nothing was opened or written. Check that TMPDIR names a writable directory, or unset it to use the system temporary directory", os.TempDir(), err)
	}
	defer func() {
		if err != nil {
			err = fmt.Errorf("%w. The temporary data directory %s was kept; remove it if you need the space", err, root)
		}
	}()
	dbPath := string(defaults.ResolveDBFilePathWith(root))
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		return fmt.Errorf("the temporary data directory %q was created but its store directory %q could not be made: %w. Nothing was opened or written", root, filepath.Dir(dbPath), err)
	}
	db, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("the temporary data directory %q was created but its store could not be opened at %s: %w", root, dbPath, err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = db.Close()
		}
	}()
	ctx := context.Background()
	sid, err := ingest.NewSessionID(sessionID)
	if err != nil {
		return fmt.Errorf("the synthetic session id could not be built: %w", err)
	}
	hash, err := ingest.NewProjectHash(projectHash)
	if err != nil {
		return fmt.Errorf("the synthetic project hash could not be built: %w", err)
	}
	host, err := ingest.NewHostSlug(hostSlug)
	if err != nil {
		return fmt.Errorf("the synthetic host slug could not be built: %w", err)
	}
	model, err := ingest.NewModelID("claude-opus-4-6")
	if err != nil {
		return fmt.Errorf("the synthetic model id could not be built: %w", err)
	}
	source, err := ingest.NewResolvedPath("/tmp/synthetic-large/session.jsonl")
	if err != nil {
		return fmt.Errorf("the synthetic source path could not be built: %w", err)
	}
	const start = int64(1_700_000_000_000)
	measured := int64(conversationTurns)
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
				TurnCount:             foldedTurns,
				ConversationTurnCount: &measured,
				ToolCallCount:         11,
				DurationMs:            60_000,
				TokensIn:              1000,
				TokensOut:             500,
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
		return fmt.Errorf("the seeded store could not record the synthetic session: %w", err)
	}
	// Entry indexes remain contiguous and the final entry is a parent, so each
	// child is inside the span a segment read can return. The middle action run
	// has multiple wordless assistant turns, each with a tool child. Their bulk
	// forces the run across segment boundaries without huge painted content.
	entries := make([]schema.SessionEntry, 0, 80)
	// Spaces retain their wire size but collapse in normal HTML whitespace, so
	// the seed does not exceed the browser's maximum scrollable paint height.
	page := strings.Repeat(" ", 400_000)
	for turn := 0; turn < 80; turn++ {
		timestamp := start + int64(turn)*1000
		if turn >= firstActionParent+1 && turn <= lastActionParent+1 && turn%2 == 0 {
			parent := turn - 1
			id := fmt.Sprintf("synthetic-tool-%d", turn)
			name := "Bash"
			input := fmt.Sprintf(`{"command":"echo segment-action-%d"}`, turn)
			output := fmt.Sprintf("segment-action-%d complete", turn)
			entries = append(entries, schema.SessionEntry{
				SessionID: sid, EntryIndex: turn, Harness: schema.HarnessClaudeCode,
				EntryType: schema.EntryTypeToolUse, Role: schema.RoleAssistant,
				TimestampMs: &timestamp, Depth: 1, ParentIndex: &parent,
				ToolCallID: &id, ToolNamesCSV: &name, ToolInput: &input, ToolOutput: &output,
			})
			continue
		}
		if systemRows[turn] {
			content := fmt.Sprintf("synthetic system notice %d: the context was compacted.", turn)
			entries = append(entries, schema.SessionEntry{
				SessionID: sid, EntryIndex: turn, Harness: schema.HarnessClaudeCode,
				EntryType: schema.EntryTypeSystem, Role: schema.RoleSystem,
				TimestampMs: &timestamp, ContentPreview: &content, Depth: 0,
			})
			continue
		}
		if toolRows[turn] {
			// A reply that holds a tool call and no prose: its own turn, typed
			// tool_use, and not a conversation turn.
			id := fmt.Sprintf("synthetic-lookup-%d", turn)
			name := "Read"
			input := fmt.Sprintf(`{"file_path":"/tmp/synthetic-large/notes-%d.md"}`, turn)
			output := fmt.Sprintf("synthetic notes %d", turn)
			entries = append(entries, schema.SessionEntry{
				SessionID: sid, EntryIndex: turn, Harness: schema.HarnessClaudeCode,
				EntryType: schema.EntryTypeToolUse, Role: schema.RoleAssistant,
				TimestampMs: &timestamp, Depth: 0,
				ToolCallID: &id, ToolNamesCSV: &name, ToolInput: &input, ToolOutput: &output,
			})
			continue
		}
		role := schema.RoleUser
		content := fmt.Sprintf("synthetic user turn %d. the reader can page from here.", turn)
		if turn%2 == 1 && turn != 53 {
			role = schema.RoleAssistant
			content = fmt.Sprintf("synthetic assistant turn %d.%s end of turn.", turn, page)
			if turn >= firstActionParent+2 && turn <= lastActionParent {
				content = page
			}
		}
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
		return fmt.Errorf("the seeded store could not record the transcript entries: %w", err)
	}
	if err := db.Close(); err != nil {
		return fmt.Errorf("the seeded store could not be closed: %w", err)
	}
	closed = true
	encoded, err := json.Marshal(map[string]any{
		"dataDir":           root,
		"sessionId":         sessionID,
		"projectHash":       projectHash,
		"projectName":       projectName,
		"actionParents":     []int{firstActionParent, lastActionParent},
		"turns":             foldedTurns,
		"conversationTurns": conversationTurns,
	})
	if err != nil {
		return fmt.Errorf("the seed result could not be encoded: %w", err)
	}
	fmt.Println(string(encoded))
	return nil
}
