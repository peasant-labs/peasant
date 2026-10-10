package defaults

// TranscriptInitialReadBytes is the soft initial transcript read budget.
const TranscriptInitialReadBytes int64 = 256 << 10

// TranscriptContinuationReadBytes is the soft body and continuation read budget.
// Each boundary measures its own payload bytes; oversized entries remain whole.
const TranscriptContinuationReadBytes int64 = 8 << 20

// SessionDetailDocumentCapBytes mirrors the wire contract's session_detail and
// transcript document policy (schema sessionDetailRawPolicy and
// transcriptRawPolicy, both unexported there). Peasant may not exceed it on any
// served path, so schema-bound checks derive from it rather than repeating the
// literal. Callers may impose their own smaller budgets.
const SessionDetailDocumentCapBytes int = 128 << 20

// PushTranscriptDocumentCapBytes aligns the publication redaction output scan
// with the contract document cap stated once above. It is an alias, not a
// second literal, so the publication scan cannot drift from the served cap.
const PushTranscriptDocumentCapBytes int = SessionDetailDocumentCapBytes

// PushMetadataDocumentCapBytes mirrors schema's authoritative publish metadata
// raw-document policy across mapping, redaction output and preflight input.
// Like the transcript publication scan, it aliases the contract document cap
// stated once above rather than repeating the value.
const PushMetadataDocumentCapBytes int = SessionDetailDocumentCapBytes

// ServedDetailDocumentMarginBytes is the headroom peasant keeps below the
// contract cap for transcript-envelope overhead and redaction growth.
// Publication checks redacted output against the same contract document cap.
const ServedDetailDocumentMarginBytes int = 1 << 20

// ServedDetailDocumentBudgetBytes is the encoded size a served session detail is
// held to. A payload at this budget decodes through the contract's raw scanner.
const ServedDetailDocumentBudgetBytes int = SessionDetailDocumentCapBytes - ServedDetailDocumentMarginBytes

// ServedTextFieldBudgetBytes is the per-field display bound for one served tool
// result, tool argument string or turn body. It is the bound a single oversized
// record meets; the document budget bounds the session as a whole and may lower
// this one further when many fields are large at once.
const ServedTextFieldBudgetBytes int = 1 << 20
