package defaults

import (
	"time"

	"github.com/peasant-labs/schema"
)

// TranscriptInitialReadBytes is the soft initial transcript read budget.
const TranscriptInitialReadBytes int64 = 256 << 10

// TranscriptSegmentBudgetBytes is the target byte size of one transcript
// segment. A segment fills with whole turns until the next turn would push it
// past this size; that turn starts the next segment, so a segment ends on a
// complete entry and turn boundary. The one exception is a single turn larger
// than the budget, which is alone in its segment and bounded per field.
//
// The value is the contract's: every server cuts with the same budget, so it is
// defined from the schema constant and never restated here.
const TranscriptSegmentBudgetBytes int = schema.TranscriptSegmentBudgetBytes

// TranscriptSegmentDirectoryCapacity is the number of segment directories one
// producer keeps, keyed by session and store revision, in LRU order. The
// directory holds refs only, never turn content, so eight entries are tens of
// kilobytes rather than transcripts.
const TranscriptSegmentDirectoryCapacity = 8

// TranscriptSegmentCutBuildBudget is the in-memory cut target for the 32 MiB
// synthetic fixture with an empty producer cache. It pins the cut itself (turn
// projection, measurement, and segmentation) without a store pass, so it is
// stable enough to assert on a shared runner.
const TranscriptSegmentCutBuildBudget = 400 * time.Millisecond

// TranscriptSegmentColdBuildBudget is the ratified cold-build target for one
// segment directory on the 32 MiB synthetic fixture, measured through the full
// store content pass. The pass reads and verifies the stored transcript, so the
// measurement is reported beside this target and warns when it is exceeded; it
// is not a hard pass/fail on a shared runner.
const TranscriptSegmentColdBuildBudget = 600 * time.Millisecond

// TranscriptSegmentColdBuildCeiling is the fail-closed guard for the store-path
// measurement. It exists to catch a catastrophic regression (a full-session
// fold per page, a lost index, an unbounded read), not to police normal runner
// contention: the recorded local range is roughly 0.43-0.51 s.
const TranscriptSegmentColdBuildCeiling = 2 * time.Second

// TranscriptWindowSpanDefault is the design number of assistant turns a window
// collects on each side of its anchor. The three span values are the
// contract's, defined from the schema constants.
const TranscriptWindowSpanDefault = schema.TranscriptWindowSpanDefault

// TranscriptWindowSpanMaximum is the widest span a caller may request. The
// parameter exists to narrow a window for a smaller context; it cannot widen the
// design value.
const TranscriptWindowSpanMaximum = schema.TranscriptWindowSpanMaximum

// TranscriptWindowSpanMinimum is the narrowest span a caller may request.
const TranscriptWindowSpanMinimum = schema.TranscriptWindowSpanMinimum

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

// PushRedactionInputCapBytes is the largest entries or transcript content
// document the publication redaction seams materialize. Each seam holds the
// document several times over (raw, decoded, re-encoded and read back), so it
// stays below the contract document cap as a memory guard. Because it is also
// below ServedDetailDocumentBudgetBytes, a session peasant commits can exceed
// it; publication refuses that session before redaction as too large to
// publish, naming its size and this cap.
const PushRedactionInputCapBytes int = 64 << 20

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
