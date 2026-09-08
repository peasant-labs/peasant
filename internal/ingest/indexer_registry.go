package ingest

// IndexerRegistryOptions configures NewIndexerRegistry.
type IndexerRegistryOptions struct {
	// FullContent disables preview truncation for compatibility callers.
	// Authoritative capture always retains full text, independent of this flag;
	// the store derives bounded previews when it commits capture content.
	FullContent bool
}

// NewIndexerRegistry returns the canonical harness → TranscriptIndexer map.
// Both ingest and compatibility readers use this mapping so that format aliases
// cannot select the wrong harness parser. Each registered indexer also provides
// AuthoritativeTranscriptIndexer; ingest uses that strict capability and lets
// the store normalize previews independently from durable full content.
func NewIndexerRegistry(fs FileSystem, opts IndexerRegistryOptions) map[Harness]TranscriptIndexer {
	return map[Harness]TranscriptIndexer{
		HarnessClaudeCode: NewClaudeIndexer(fs, WithClaudeFullDepth(true), WithClaudeFullContent(opts.FullContent)),
		HarnessOpenCode:   NewOpenCodeIndexer(fs, WithOpenCodeFullDepth(true), WithOpenCodeFullContent(opts.FullContent)),
		HarnessCodex:      NewCodexIndexer(fs, WithCodexFullContent(opts.FullContent)),
		HarnessCursor:     NewCursorIndexer(fs, WithCursorFullDepth(true), WithCursorFullContent(opts.FullContent)),
		HarnessStrike:     NewStrikeIndexer(fs, WithStrikeFullContent(opts.FullContent)),
	}
}
