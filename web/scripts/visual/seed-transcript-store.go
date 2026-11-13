//go:build ignore

// Seed a synthetic multi-segment transcript into a throwaway data directory
// so the mounted capture can open the production route. No real transcript
// text, session id, or personal path is used.
//
//	go run ./web/scripts/visual/seed-transcript-store.go -data-dir <dir>
//
// The data directory is the one the capture's server sandbox gives the binary
// as --data-dir. It must be an absolute path that does not exist yet: the
// seeder creates the directory it writes to and refuses anything already at
// the path, so an accidental real data directory is never opened or modified.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
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

// run seeds one synthetic transcript into a data directory this run creates and owns.
//
// It refuses a path that already exists, in any form, before a store is opened: the exclusive
// create of the data directory is the claim, so a store this run did not create is never opened,
// migrated or written. The capture passes the data directory of a fresh server sandbox, which the
// sandbox has not created yet. Whatever this run creates is removed again when seeding fails.
func run() (err error) {
	dataDir := flag.String("data-dir", "", "absolute path of a data directory this run creates and owns")
	flag.Parse()
	root := filepath.Clean(*dataDir)
	if !filepath.IsAbs(root) {
		return fmt.Errorf("-data-dir must name an absolute path that does not exist yet, but it is %q. The seeder creates the data directory it writes to, so nothing was created or written. Pass the data directory of a fresh server sandbox, for example -data-dir /tmp/peasant-transcript-segments-XXXX/data", *dataDir)
	}
	claim, err := claimDataDir(root)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			claim.discard()
		}
	}()
	dbPath := string(defaults.ResolveDBFilePathWith(root))
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		return fmt.Errorf("the new data directory %q was created but its store directory %q could not be made: %w. Nothing was opened or written, and the directory this run created is removed", root, filepath.Dir(dbPath), err)
	}
	db, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("the new data directory %q was created but its store could not be opened at %s: %w. Nothing was kept, and the directory this run created is removed", root, dbPath, err)
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
		return seedFailure("building the synthetic session id", root, err)
	}
	hash, err := ingest.NewProjectHash(projectHash)
	if err != nil {
		return seedFailure("building the synthetic project hash", root, err)
	}
	host, err := ingest.NewHostSlug(hostSlug)
	if err != nil {
		return seedFailure("building the synthetic host slug", root, err)
	}
	model, err := ingest.NewModelID("claude-opus-4-6")
	if err != nil {
		return seedFailure("building the synthetic model id", root, err)
	}
	source, err := ingest.NewResolvedPath("/tmp/synthetic-large/session.jsonl")
	if err != nil {
		return seedFailure("building the synthetic source path", root, err)
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
		return seedFailure("writing the session row", root, err)
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
		return seedFailure("writing the transcript entries", root, err)
	}
	if err := db.Close(); err != nil {
		return seedFailure("closing the seeded store", root, err)
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
		return seedFailure("encoding the result", root, err)
	}
	fmt.Println(string(encoded))
	return nil
}

// dataDirClaim is a data directory this run created, with the identity of the directory it created.
type dataDirClaim struct {
	root string
	info fs.FileInfo
}

// claimDataDir creates root as a directory this run owns and returns the claim.
//
// The create is the claim: os.Mkdir fails when anything already exists at the path, including a
// directory another process made between the inspection and the create, so two runs cannot both
// believe they own one data directory. A path that already exists, in any form, is refused before
// any store is opened; a symlink is reported as a symlink, and a file or directory keeps its bytes.
func claimDataDir(root string) (dataDirClaim, error) {
	if root == string(filepath.Separator) {
		return dataDirClaim{}, fmt.Errorf("-data-dir must name the data directory of a fresh server sandbox, not the filesystem root %q. Nothing was created or written; pass a disposable directory such as /tmp/peasant-transcript-segments-XXXX/data", root)
	}
	existing, statErr := os.Lstat(root)
	if statErr == nil {
		kind := "a file"
		switch {
		case existing.Mode()&os.ModeSymlink != 0:
			kind = "a symlink"
		case existing.IsDir():
			kind = "a directory"
		}
		return dataDirClaim{}, fmt.Errorf("-data-dir %q already exists as %s. The seeder only writes to a data directory it creates itself, so an existing store is never opened, migrated or modified, and nothing was written. Pass a path that does not exist yet, such as the data directory of a fresh server sandbox; remove %q first only if it holds disposable fixture state", root, kind, root)
	}
	if !errors.Is(statErr, fs.ErrNotExist) {
		return dataDirClaim{}, fmt.Errorf("-data-dir %q could not be inspected: %w. Nothing was created or written. Check the path and its parent directories' permissions", root, statErr)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return dataDirClaim{}, fmt.Errorf("-data-dir %q appeared while this run was claiming it: another process created it between the inspection and the exclusive create that is the claim. Nothing was opened or written. Run again with a path that does not exist, or remove %q first if it holds disposable fixture state", root, root)
		}
		return dataDirClaim{}, fmt.Errorf("-data-dir %q could not be created: %w. Nothing was written; the parent directory must already exist. Pass the data directory of a fresh server sandbox, for example -data-dir /tmp/peasant-transcript-segments-XXXX/data", root, err)
	}
	created, err := os.Lstat(root)
	if err != nil || !created.IsDir() || created.Mode()&os.ModeSymlink != 0 {
		_ = os.RemoveAll(root)
		return dataDirClaim{}, fmt.Errorf("-data-dir %q was created but is no longer a plain directory this run owns, so nothing was opened or written and the path was removed. Run again with a path that does not exist", root)
	}
	return dataDirClaim{root: root, info: created}, nil
}

// discard removes the directory this run created, and only that directory: a path another process
// has since replaced with a different file or directory is left alone.
func (c dataDirClaim) discard() {
	now, err := os.Lstat(c.root)
	if err != nil || !isSamePlainDir(c.info, now) {
		return
	}
	_ = os.RemoveAll(c.root)
}

// isSamePlainDir reports whether now is the plain directory that info describes, rather than a
// symlink or a different file or directory another process put at the same path.
func isSamePlainDir(info, now fs.FileInfo) bool {
	if now.Mode()&os.ModeSymlink != 0 {
		return false
	}
	if !now.IsDir() {
		return false
	}
	return os.SameFile(info, now)
}

// seedFailure names the step that failed and says what was, and was not, kept.
func seedFailure(step, root string, err error) error {
	return fmt.Errorf("the synthetic transcript could not be seeded in %s: %w. The data directory this run created (%s) is removed; nothing outside it was written", step, err, root)
}
