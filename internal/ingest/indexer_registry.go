package ingest

// Indexer is the composed surface every indexer NewIndexerRegistry registers
// provides: the compatibility reads every consumer dispatches on, plus the
// strict capture entry points ingest uses for authoritative content. The
// registry's declared output carries it, so a consumer takes both surfaces from
// one value without a runtime type assertion.
//
// The pipeline's injection seam stays compatibility-typed on purpose (see
// WithIndexers): a deliberately non-authoritative double must remain
// non-authoritative, so the composed surface belongs to the registry OUTPUT,
// not to the seam.
type Indexer interface {
	TranscriptIndexer
	AuthoritativeTranscriptIndexer
}

// IndexerRegistryOptions configures NewIndexerRegistry.
type IndexerRegistryOptions struct {
	// FullContent disables preview truncation for compatibility callers.
	// Authoritative capture always retains full text, independent of this flag;
	// the store derives bounded previews when it commits capture content.
	FullContent bool

	// MaxRecordBytes overrides the per-record read limit every JSONL indexer
	// applies. Zero, the production value, means defaults.MaxJSONLRecordBytes.
	// A test injects a small limit here to exercise the over-limit path
	// without building a record of production size; nothing global changes,
	// so parallel tests do not interfere.
	MaxRecordBytes int
}

// NewIndexerRegistry returns the canonical harness → Indexer map. Both ingest
// and compatibility readers use this mapping so that format aliases cannot
// select the wrong harness parser. Every value carries the composed surface:
// the compatibility reads AND the strict AuthoritativeTranscriptIndexer capture
// entry points, so callers need no runtime assertion. Ingest uses the strict
// capability and lets the store normalize previews independently from durable
// full content.
func NewIndexerRegistry(fs FileSystem, opts IndexerRegistryOptions) map[Harness]Indexer {
	return map[Harness]Indexer{
		HarnessPi:         NewPiIndexer(fs, WithPiFullContent(opts.FullContent)),
		HarnessClaudeCode: NewClaudeIndexer(fs, WithClaudeFullDepth(true), WithClaudeFullContent(opts.FullContent), WithClaudeMaxRecordBytes(opts.MaxRecordBytes)),
		HarnessOpenCode:   NewOpenCodeIndexer(fs, WithOpenCodeFullDepth(true), WithOpenCodeFullContent(opts.FullContent)),
		HarnessCodex:      NewCodexIndexer(fs, WithCodexFullContent(opts.FullContent), WithCodexMaxRecordBytes(opts.MaxRecordBytes)),
		HarnessCursor:     NewCursorIndexer(fs, WithCursorFullDepth(true), WithCursorFullContent(opts.FullContent), WithCursorMaxRecordBytes(opts.MaxRecordBytes)),
		HarnessStrike:     NewStrikeIndexer(fs, WithStrikeFullContent(opts.FullContent), WithStrikeMaxRecordBytes(opts.MaxRecordBytes)),
	}
}
