package ingest

// unknownJSONLTraversal tracks the captured physical stream before projection.
// Every physical line slot advances the record coordinate, including blank and
// omitted-record slots the scanner passes. Each relevant typed content block is
// then visited depth-first, whether it is known or retained. Opaque unknown
// records/blocks are single nodes: their internals are not guessed to be blocks.
// The fixed opaque reference identifies this session's one JSONL source stream,
// not a native entry or a path; reindexing the same stream keeps its identity.
type unknownJSONLTraversal struct {
	lastLine     int
	nextPosition int64
}

func (t *unknownJSONLTraversal) record(line int) *UnknownPublicPosition {
	if t == nil {
		return nil
	} // Validation-only passes do not persist evidence.
	t.nextPosition += int64(line - t.lastLine)
	t.lastLine = line
	return &UnknownPublicPosition{SourceRef: "source-0", RecordIndex: int64(line - 1), Position: t.nextPosition - 1}
}

func (t *unknownJSONLTraversal) block(record *UnknownPublicPosition) *UnknownPublicPosition {
	if t == nil {
		return nil
	}
	position := &UnknownPublicPosition{SourceRef: record.SourceRef, RecordIndex: record.RecordIndex, Position: t.nextPosition}
	t.nextPosition++
	return position
}
