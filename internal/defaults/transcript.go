package defaults

// TranscriptInitialReadBytes is the soft initial transcript read budget.
const TranscriptInitialReadBytes int64 = 256 << 10

// TranscriptContinuationReadBytes is the soft body and continuation read budget.
// Each boundary measures its own payload bytes; oversized entries remain whole.
const TranscriptContinuationReadBytes int64 = 8 << 20
