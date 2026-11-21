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

// The raw session preview reads a discovered session in place, from the
// transcript its harness wrote, through internal/sourcepreview. The capture
// itself takes every bound as an option, so each surface states its own. The
// values below are the WEB preview's: the kickstart page's server-owned capture
// passes them. The terminal preview does not inherit them; it keeps its own
// options, stated where its adapter builds them.
//
// Why a cumulative source cap exists at all: the file capture holds the loaded
// prefix and re-parses all of it on every read, because the canonical
// projection is not resumable. Its retained memory and its re-parse work both
// grow with the prefix, so the web capture caps the prefix. Paging alone would
// not need it: a segment is the contract's budget and a frame is bounded below.
//
// The source cap is set by the transient peak of one re-parse, which must stay
// at or under 128 MiB. A re-parse briefly needs about 5.5 times the prefix it
// parses (the live expansion factor BenchmarkCaptureWebHeap in
// internal/sourcepreview measures with the collector running often): 16 MiB
// peaked at about 92 MiB, 24 MiB at about 132 MiB. So the cap is 16 MiB.
const (
	// KickstartWebPreviewSourceCapBytes is the cumulative source bytes one web
	// capture may load. Three reads at most: 256 KiB, then 8 MiB at a time.
	KickstartWebPreviewSourceCapBytes int64 = 16 << 20
	// KickstartWebPreviewSerializedCapBytes is the most encoded segment bytes
	// one web capture sends: the most one preview pane holds. It matches the
	// source cap, so neither cap cuts a capture the other would let continue
	// by much.
	KickstartWebPreviewSerializedCapBytes int64 = 16 << 20
	// KickstartWebPreviewTurnCap is the most turns one web capture emits. It
	// bounds the capture's turn digests and the browser's view model.
	KickstartWebPreviewTurnCap = 20_000
	// KickstartWebPreviewFrameCapBytes is the largest encoded preview frame:
	// one segment plus the header and directory, half the client's 8 MiB frame
	// cap.
	KickstartWebPreviewFrameCapBytes = 4 << 20
	// KickstartWebPreviewLiveCaptures is how many web captures may be live at
	// once across the server; it sets the retained-memory ceiling.
	KickstartWebPreviewLiveCaptures = 2
	// KickstartWebPreviewParseWorkers is how many captures may re-parse at
	// once, so only one transient re-parse peak exists at a time.
	KickstartWebPreviewParseWorkers = 1
	// KickstartWebPreviewIdle frees a web capture no one has asked for more of.
	KickstartWebPreviewIdle = 5 * time.Minute
)

// KickstartTerminalPreviewSourceCapBytes is the cumulative source bytes one
// terminal kickstart preview may load before it stops with the cut notice. It
// is the terminal's own bound, set apart from the web preview's so either can
// change alone: the terminal capture holds the loaded prefix and re-parses it
// on every read just as the web capture does, so its memory and re-parse work
// grow with the prefix. The terminal sets no turn cap.
const KickstartTerminalPreviewSourceCapBytes int64 = 32 << 20

// The capture benchmark reads a 32 MiB synthetic Claude transcript to the web
// source cap. The targets warn; the ceiling fails closed and exists to
// catch a catastrophic regression (a re-parse per turn, an unbounded read),
// not runner contention.
const (
	SourcePreviewFirstSegmentBudget = 250 * time.Millisecond
	SourcePreviewFullCaptureBudget  = 6 * time.Second
	SourcePreviewFullCaptureCeiling = 30 * time.Second
	// SourcePreviewPeakHeapCeilingBytes fails the capture benchmark when the
	// sampled heap (default collector settings, garbage included) passes it.
	// It catches a catastrophic regression; the 128 MiB rule on the live
	// transient is what sets the web source cap above.
	SourcePreviewPeakHeapCeilingBytes int64 = 512 << 20
)
