package defaults

// TranscriptInitialReadBytes is the soft initial transcript read budget.
const TranscriptInitialReadBytes int64 = 256 << 10

// TranscriptContinuationReadBytes is the soft body and continuation read budget.
// Each boundary measures its own payload bytes; oversized entries remain whole.
const TranscriptContinuationReadBytes int64 = 8 << 20

// SessionDetailDocumentCapBytes mirrors the wire contract's session_detail and
// transcript document policy (schema sessionDetailRawPolicy and
// transcriptRawPolicy, both unexported there). Peasant may not exceed it on any
// served path, so it is stated ONCE here and every peasant-side check derives
// from it rather than repeating the literal.
const SessionDetailDocumentCapBytes int = 8 << 20

// ServedDetailDocumentMarginBytes is the headroom peasant keeps below the
// contract cap. The publication path wraps the same payload in a transcript
// envelope and then redacts it, and a redaction rewrites values in place and can
// grow them; the margin absorbs both without a second cap.
const ServedDetailDocumentMarginBytes int = 1 << 20

// ServedDetailDocumentBudgetBytes is the encoded size a served session detail is
// held to. A payload at this budget decodes through the contract's raw scanner.
const ServedDetailDocumentBudgetBytes int = SessionDetailDocumentCapBytes - ServedDetailDocumentMarginBytes

// ServedTextFieldBudgetBytes is the per-field display bound for one served tool
// result, tool argument string or turn body. It is the bound a single oversized
// record meets; the document budget bounds the session as a whole and may lower
// this one further when many fields are large at once.
const ServedTextFieldBudgetBytes int = 1 << 20
