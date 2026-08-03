package ingest

// IndexerRegistryOptions configures NewIndexerRegistry.
type IndexerRegistryOptions struct {
	// FullContent disables ContentPreview truncation on every indexer that
	// supports the option (Claude, OpenCode, Codex). Real ingest wiring
	// leaves this false — the DB deliberately stores a bounded preview
	// (defaults.ContentPreviewLimit) to keep session_entries rows sane.
	// FullContent: true is for RE-INDEXING an already-ingested session's
	// source transcript to recover its real content (transcript.
	// BuildContentOverlay / export.ExportSession) — never for a normal
	// ingest run.
	FullContent bool
}

// NewIndexerRegistry returns the canonical harness → TranscriptIndexer map.
// This is the ONE place that maps a harness to its indexer constructor.
// Every consumer — the real ingest pipeline wiring (cmd_kickstart.go,
// cmd_harvest.go) and the full-content re-index path (transcript.
// BuildContentOverlay) — must build its registry through this function
// rather than hand-writing the map, so the two purposes (bounded ingest vs.
// full re-index) can never drift into a THIRD hand-copy that silently omits
// a harness or dispatches on the wrong thing. That exact divergence produced
// three separate regressions in one session before this consolidation:
// a hand-copy in export.ExportSession dispatching by SourceFormat instead
// of harness (Codex, sharing "jsonl" with Claude Code, silently got the
// wrong indexer), and two independently-drifted ingest-time copies (one
// missing WithCursorFullDepth).
//
// Cursor has no FullContent knob at all — its indexer
// (internal/ingest/cursor.go) always truncates at defaults.
// ContentPreviewLimit regardless of opts.FullContent; recovering its full
// content needs its own indexer change, tracked as a separate follow-up.
func NewIndexerRegistry(fs FileSystem, opts IndexerRegistryOptions) map[Harness]TranscriptIndexer {
	return map[Harness]TranscriptIndexer{
		HarnessClaudeCode: NewClaudeIndexer(fs, WithClaudeFullDepth(true), WithClaudeFullContent(opts.FullContent)),
		HarnessOpenCode:   NewOpenCodeIndexer(fs, WithOpenCodeFullDepth(true), WithOpenCodeFullContent(opts.FullContent)),
		HarnessCodex:      NewCodexIndexer(fs, WithCodexFullContent(opts.FullContent)),
		HarnessCursor:     NewCursorIndexer(fs, WithCursorFullDepth(true)),
		HarnessStrike:     NewStrikeIndexer(fs, WithStrikeFullContent(opts.FullContent)),
	}
}
