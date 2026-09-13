package ingest

// Codex current-history capture and replay.
//
// This file owns the read-only Codex source authority, the bounded ordered
// segment capture, and the deterministic replay that turns captured native
// records into an adapter-private node graph for the native provenance
// classifier. It does not classify blocks, does not allocate durable
// generation rows, and never writes the native source.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/peasant-labs/peasant/internal/codexstate"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/schema"
)

// CodexSourceAuthorityKind names how the one current source was selected
// before any discovery diff. It is a local closed enum; only the native
// current pointer and a detached file are recognized.
type CodexSourceAuthorityKind string

const (
	// CodexAuthorityNativeCurrentPointer is a pointer read from the native
	// source abstraction: the live-writer pointer when the source exposes one,
	// otherwise the native current rollout pointer.
	CodexAuthorityNativeCurrentPointer CodexSourceAuthorityKind = "native_current_pointer"
	// CodexAuthorityDetachedFile is the fallback used only when no pointer
	// authority exists. A single detached candidate is usable with
	// unresolved-incarnation evidence; several conflicting candidates are not
	// selected by newest mtime.
	CodexAuthorityDetachedFile CodexSourceAuthorityKind = "detached_file"
)

// AllCodexSourceAuthorityKinds is the exact closed set of authority kinds.
var AllCodexSourceAuthorityKinds = []CodexSourceAuthorityKind{
	CodexAuthorityNativeCurrentPointer,
	CodexAuthorityDetachedFile,
}

// IsValid reports whether the value is a member of the closed set.
func (k CodexSourceAuthorityKind) IsValid() bool {
	for _, known := range AllCodexSourceAuthorityKinds {
		if k == known {
			return true
		}
	}
	return false
}

// NewCodexSourceAuthorityKind validates a raw authority kind at an input
// boundary.
func NewCodexSourceAuthorityKind(raw string) (CodexSourceAuthorityKind, error) {
	k := CodexSourceAuthorityKind(raw)
	if !k.IsValid() {
		return "", fmt.Errorf("ingest.NewCodexSourceAuthorityKind: value %q is outside the closed authority-kind set %v; the current Codex source cannot be interpreted; use a published authority kind", raw, AllCodexSourceAuthorityKinds)
	}
	return k, nil
}

// CodexHistoryMode is the recognized native history-replay mode. Only an
// omitted mode defaults to legacy; a null, unknown or malformed mode is
// unsupported and never selects the legacy reducer.
type CodexHistoryMode string

const (
	CodexHistoryModeLegacy      CodexHistoryMode = "legacy"
	CodexHistoryModePaginated   CodexHistoryMode = "paginated"
	CodexHistoryModeUnsupported CodexHistoryMode = "unsupported"
)

// AllCodexHistoryModes is the exact closed set of resolved history modes.
var AllCodexHistoryModes = []CodexHistoryMode{
	CodexHistoryModeLegacy,
	CodexHistoryModePaginated,
	CodexHistoryModeUnsupported,
}

// IsValid reports whether the value is a member of the closed set.
func (m CodexHistoryMode) IsValid() bool {
	for _, known := range AllCodexHistoryModes {
		if m == known {
			return true
		}
	}
	return false
}

// resolveCodexHistoryMode resolves the native history_mode evidence. An absent
// value (nil or empty raw JSON) defaults to legacy. Explicit "legacy" and
// "paginated" resolve to themselves. Anything else, including JSON null and
// non-string values, resolves to unsupported.
func resolveCodexHistoryMode(raw json.RawMessage) CodexHistoryMode {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return CodexHistoryModeLegacy
	}
	if trimmed == "null" {
		return CodexHistoryModeUnsupported
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return CodexHistoryModeUnsupported
	}
	switch CodexHistoryMode(value) {
	case CodexHistoryModeLegacy:
		return CodexHistoryModeLegacy
	case CodexHistoryModePaginated:
		return CodexHistoryModePaginated
	default:
		return CodexHistoryModeUnsupported
	}
}

// CodexOwnership is the native ownership evidence a captured node carries.
// It is not a presentation role and never changes the node's raw native type.
type CodexOwnership string

const (
	CodexOwnershipOwn              CodexOwnership = "own"
	CodexOwnershipInherited        CodexOwnership = "inherited"
	CodexOwnershipUncertainEarlier CodexOwnership = "uncertain_earlier_history"
	CodexOwnershipReverted         CodexOwnership = "reverted"
	CodexOwnershipExcluded         CodexOwnership = "excluded"
)

// CodexCorrelationKind names one native correlation the replay proved.
type CodexCorrelationKind string

const (
	// CodexCorrelationPairedResponseEvent pairs one accepted event with its
	// response item so the input is represented once.
	CodexCorrelationPairedResponseEvent CodexCorrelationKind = "paired_response_event"
	// CodexCorrelationRepeatedItemCompleted records a repeated ItemCompleted
	// that updates one native item instead of adding a duplicate.
	CodexCorrelationRepeatedItemCompleted CodexCorrelationKind = "repeated_item_completed"
	// CodexCorrelationSameThreadRollover records that a surviving native ID
	// was aliased onto the same opaque ref across a physical rollover.
	CodexCorrelationSameThreadRollover CodexCorrelationKind = "same_thread_rollover"
)

// CodexCapturedNode is one captured native content node. Ref is the opaque
// stable source-entry ref allocated through the injected registry; NativeKey is
// the bounded local identity the registry keys on. Ordinal is the valid
// decoded native ordinal checkpoint of the record (never a physical line
// position); LineIndex is the physical line position for byte accounting.
// Payload is the captured raw record payload bytes and Metadata is the
// history-envelope metadata recorded beside the payload; together they let the
// native provenance classifier work without reopening the mutable source.
type CodexCapturedNode struct {
	NativeKey        string
	Ref              schema.SourceEntryRef
	SegmentOrdinal   int
	Ordinal          int64
	LineIndex        int64
	ByteStart        int64
	ByteEndExclusive int64
	EnvelopeType     string
	NativeType       string
	NativeRole       string
	ItemID           string
	CallID           string
	TurnID           string
	Ownership        CodexOwnership
	Payload          []byte
	Metadata         json.RawMessage
}

// CodexCapturedCorrelation is one proven native correlation. Refs names the
// captured nodes the correlation ties together.
type CodexCapturedCorrelation struct {
	Kind            CodexCorrelationKind
	ItemID          string
	EventOrdinal    *int64
	ResponseOrdinal *int64
	Refs            []schema.SourceEntryRef
}

// CodexCapturedRecord is one captured native record inside a captured
// segment, including records that produced no node. DecodedOrdinal is the
// valid decoded native ordinal; it is nil for partial, malformed and unknown
// records, which advance the byte checkpoint only and are never assigned a
// native ordinal by line number. Payload holds the raw record payload bytes
// and Metadata the history-envelope metadata recorded beside the payload.
type CodexCapturedRecord struct {
	DecodedOrdinal   *int64
	LineIndex        int64
	ByteStart        int64
	ByteEndExclusive int64
	EnvelopeType     string
	Payload          []byte
	Metadata         json.RawMessage
	Partial          bool
	Malformed        bool
}

// CodexCapturedSegment is one ordered captured segment with its bounded
// decoded bytes and per-record evidence. Data holds ONLY the decoded bytes
// inside the segment's native bounds, so the classifier sees every captured
// segment payload without reopening mutable native sources.
type CodexCapturedSegment struct {
	Ordinal    int
	Descriptor CodexReference
	Data       []byte
	Records    []CodexCapturedRecord
}

// CodexReference is one ordered bounded dependency of the current source. The
// reference graph is resolved oldest-to-newest before the current pointer.
// HistoryKind is the native dependency kind: "before" or "through". A
// through dependency requires a completed target; a running target leaves
// the coverage unproven and the capture incomplete.
type CodexReference struct {
	LogicalSessionID        *SessionID
	Pointer                 string
	PhysicalSourceID        string
	Mode                    CodexHistoryMode
	HistoryKind             string
	ThroughCompleted        bool
	Coordinates             indexformat.SegmentCoordinates
	Inclusion               indexformat.SegmentInclusion
	CopyBoundary            *int64
	OriginalOwnershipProven bool
}

// CodexSourceAuthority is the selected read-only authority for one Codex
// thread. HistoryMode is the raw native value: the capture resolves it so an
// omitted value and a null value stay distinguishable. CopyBoundary,
// HistoryBaseThreadID, RootID and References are derived from the native
// session_meta envelope by the production source; a test double supplies the
// same derived shape, never a competing derivation.
type CodexSourceAuthority struct {
	StableThreadID   string
	Kind             CodexSourceAuthorityKind
	CurrentPointer   string
	PhysicalSourceID string
	HistoryMode      json.RawMessage
	RootID           string
	ParentID         string
	ForkSourceID     string
	// HistoryBaseThreadID is the native history_base.thread_id rollout
	// identity when the current session_meta records one. It is a physical
	// rollout identity, never automatically a logical edge.
	HistoryBaseThreadID string
	// CopyBoundary is the native copied-creation boundary S of the current
	// incarnation. Records before S are inherited only when native ownership is
	// proven; otherwise they are migrated/uncertain earlier history.
	CopyBoundary            *int64
	OriginalOwnershipProven bool
	// References are the ordered bounded dependencies, oldest first.
	References []CodexReference
	// DerivationDiagnostics records non-fatal native-envelope observations
	// made while deriving this authority (malformed optional bounds,
	// malformed history envelopes). The replay merges them into the captured
	// diagnostics; derivation never invents a bound it could not decode.
	DerivationDiagnostics []DiagnosticEntry
	// DerivationIncomplete marks that the native envelope could not prove a
	// bound the replay needs (malformed history envelope or malformed copy
	// boundary). The capture replays the proven records and reports
	// incomplete_new; the caller retains last-good state.
	DerivationIncomplete bool
}

// CodexReadOnlySource is the read-only access the capture path uses. A
// production implementation reads the native pointer and bounded bytes without
// starting a Codex process. Tests inject a deterministic source that returns
// the bytes the fixture describes.
type CodexReadOnlySource interface {
	// ResolveCodexAuthority selects the stable session_meta.id and exactly one
	// current pointer before any discovery diff. It never falls back to an older
	// rollout when the authoritative current source is missing.
	ResolveCodexAuthority(ctx context.Context, session DiscoveredSession) (CodexSourceAuthority, error)
	// ReadCodexSource returns the bounded decoded bytes for one source pointer.
	ReadCodexSource(ctx context.Context, pointer string) ([]byte, error)
}

// CodexRefAllocator allocates one opaque source-entry ref. The index is the
// number of refs the registry has already allocated, so a deterministic
// allocator can reproduce fixture refs exactly.
type CodexRefAllocator func(index int) schema.SourceEntryRef

// CodexRefRegistry maps a bounded native key to the one stable opaque ref for
// it. A key seen before (a surviving native ID across a physical rollover)
// resolves to its original ref instead of re-keying by path.
type CodexRefRegistry struct {
	mu       sync.Mutex
	byKey    map[string]schema.SourceEntryRef
	allocate CodexRefAllocator
}

// NewCodexRefRegistry creates a registry. A nil allocator uses the production
// random 128-bit allocation.
func NewCodexRefRegistry(allocate CodexRefAllocator) *CodexRefRegistry {
	if allocate == nil {
		allocate = defaultCodexRefAllocator
	}
	return &CodexRefRegistry{byKey: map[string]schema.SourceEntryRef{}, allocate: allocate}
}

// RefFor resolves the stable ref for one native key, allocating on first use.
// reused reports that the key was already known, which is the same-thread
// rollover evidence the replay records.
func (r *CodexRefRegistry) RefFor(nativeKey string) (schema.SourceEntryRef, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ref, ok := r.byKey[nativeKey]; ok {
		return ref, true, nil
	}
	ref := r.allocate(len(r.byKey))
	if ref == "" {
		return "", false, fmt.Errorf("ingest.CodexRefRegistry.RefFor: allocator returned an empty source entry ref for native key %q; the captured block cannot be addressed; supply a valid opaque ref", nativeKey)
	}
	if err := ref.Validate(); err != nil {
		return "", false, fmt.Errorf("ingest.CodexRefRegistry.RefFor: allocator returned an invalid source entry ref for native key %q: %w", nativeKey, err)
	}
	r.byKey[nativeKey] = ref
	return ref, false, nil
}

var codexRefEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// defaultCodexRefAllocator allocates a random 128-bit opaque ref, never a hash
// of native content.
func defaultCodexRefAllocator(int) schema.SourceEntryRef {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// crypto/rand failure is fatal for identity stability; fall back to a
		// deterministic digest so a caller still gets a bounded, unique ref.
		digest := sha256.Sum256([]byte(fmt.Sprintf("codex-ref-%d", raw)))
		copy(raw[:], digest[:16])
	}
	return schema.SourceEntryRef("e_" + strings.ToLower(codexRefEncoding.EncodeToString(raw[:])))
}

// CodexCapturedHistory is the adapter-private capture handed to the native
// provenance classifier. It contains the captured native node graph, the
// ordered segment evidence, the proven correlations and completeness.
// CapturedSegments carries the verified graph plus every captured
// segment/node payload and adjacent envelope metadata, so the classifier
// never reopens the mutable native sources. RawBytes is the bounded decoded
// prefix of the authoritative current source, retained for the entry path.
type CodexCapturedHistory struct {
	StableThreadID   string
	AuthorityKind    CodexSourceAuthorityKind
	Pointer          string
	PhysicalSourceID string
	Mode             CodexHistoryMode
	Completeness     indexformat.GenerationCompleteness
	Nodes            []CodexCapturedNode
	Segments         []indexformat.ContextSegment
	CapturedSegments []CodexCapturedSegment
	Correlations     []CodexCapturedCorrelation
	MainRefs         []schema.SourceEntryRef
	InheritedRefs    []schema.SourceEntryRef
	EarlierRefs      []schema.SourceEntryRef
	RevertedRefs     []schema.SourceEntryRef
	CheckpointRefs   []schema.SourceEntryRef
	RawBytes         []byte
	Fingerprint      string
	Diagnostics      []DiagnosticEntry
}

// CodexSourceChangedError reports that the authoritative source kept changing
// across the bounded capture attempts. The caller retains the last good
// generation and versions and retries on the next normal run.
type CodexSourceChangedError struct {
	StableThreadID string
	Attempts       int
}

func (e *CodexSourceChangedError) Error() string {
	return fmt.Sprintf("ingest.CaptureCodexHistory: Codex source for thread %q changed during every one of %d bounded capture attempts: the native writer kept moving the current pointer, bytes or completeness; no activation was attempted, the last good generation and versions were retained, and the next normal run retries", e.StableThreadID, e.Attempts)
}

// maxCodexCaptureAttempts is the bounded retry budget for one capture.
const maxCodexCaptureAttempts = 3

// CodexAuthoritativeSession is a discovered Codex session re-identified by the
// native authority before discovery identity/diff. Session carries the stable
// session_meta.id as SessionID and the authoritative current source as
// SourcePath; Authority is the resolved pointer/mode/boundary evidence.
type CodexAuthoritativeSession struct {
	Session   DiscoveredSession
	Authority CodexSourceAuthority
}

// ResolveCodexAuthoritativeSession resolves the read-only native current
// authority before any discovery identity/diff and returns the discovered
// session re-pointed at the authoritative current source. It is the pre-diff
// selection seam a discovery/diff caller uses: the stable session_meta.id
// supersedes the filename-derived identity and the native current pointer
// supersedes the discovered path. A missing or ambiguous current source is an
// error; it is never replaced by an older rollout.
func ResolveCodexAuthoritativeSession(ctx context.Context, source CodexReadOnlySource, session DiscoveredSession) (CodexAuthoritativeSession, error) {
	if source == nil {
		return CodexAuthoritativeSession{}, fmt.Errorf("ingest.ResolveCodexAuthoritativeSession: no read-only source was supplied for session %s; the current Codex source cannot be selected before diff; register a Codex source and retry", session.SessionID)
	}
	authority, err := source.ResolveCodexAuthority(ctx, session)
	if err != nil {
		return CodexAuthoritativeSession{}, err
	}
	identified := session
	if authority.StableThreadID != "" {
		if stableID, idErr := NewSessionID(authority.StableThreadID); idErr == nil {
			identified.SessionID = stableID
		}
	}
	if authority.CurrentPointer != "" {
		identified.SourcePath = ResolvedPath(authority.CurrentPointer)
	}
	return CodexAuthoritativeSession{Session: identified, Authority: authority}, nil
}

// CaptureCodexHistory resolves the authority, reads the bounded ordered
// segments, replays the native records, and returns the captured history. A
// missing authoritative source is returned as an error; it is never replaced
// by an older rollout.
func CaptureCodexHistory(ctx context.Context, source CodexReadOnlySource, session DiscoveredSession, registry *CodexRefRegistry) (CodexCapturedHistory, error) {
	if source == nil {
		return CodexCapturedHistory{}, fmt.Errorf("ingest.CaptureCodexHistory: no read-only source was supplied for session %s; no source can be read; register a Codex source and retry", session.SessionID)
	}
	authority, err := source.ResolveCodexAuthority(ctx, session)
	if err != nil {
		return CodexCapturedHistory{}, err
	}
	if registry == nil {
		registry = NewCodexRefRegistry(nil)
	}
	return replayCodexHistory(ctx, source, authority, registry)
}

// CaptureCodexHistoryWithRetry captures and then rechecks the full relevant
// fingerprint, retrying the whole capture up to the bounded budget. After the
// budget it returns the last capture with a CodexSourceChangedError so the
// caller can retain the last good generation.
func CaptureCodexHistoryWithRetry(ctx context.Context, source CodexReadOnlySource, session DiscoveredSession, registry *CodexRefRegistry) (CodexCapturedHistory, error) {
	if registry == nil {
		registry = NewCodexRefRegistry(nil)
	}
	var last CodexCapturedHistory
	for attempt := 1; attempt <= maxCodexCaptureAttempts; attempt++ {
		captured, err := CaptureCodexHistory(ctx, source, session, registry)
		if err != nil {
			return CodexCapturedHistory{}, err
		}
		stable, err := codexCaptureIsStable(ctx, source, session, captured)
		if err != nil {
			return CodexCapturedHistory{}, err
		}
		if stable {
			return captured, nil
		}
		last = captured
	}
	return last, &CodexSourceChangedError{StableThreadID: last.StableThreadID, Attempts: maxCodexCaptureAttempts}
}

// codexCaptureIsStable re-resolves the authority and re-reads the current
// bounded prefix, then compares the capture fingerprint. A changed pointer,
// byte prefix, completeness or referenced prefix makes the capture unstable.
func codexCaptureIsStable(ctx context.Context, source CodexReadOnlySource, session DiscoveredSession, captured CodexCapturedHistory) (bool, error) {
	fresh, err := CaptureCodexHistory(ctx, source, session, NewCodexRefRegistry(nil))
	if err != nil {
		return false, err
	}
	return fresh.Fingerprint == captured.Fingerprint, nil
}

// codexAuthoritySignature is the canonical, comparable signature of the
// selected authority. It never carries a raw private path.
func codexAuthoritySignature(authority CodexSourceAuthority) string {
	var b strings.Builder
	writeLengthPrefixed(&b, string(HarnessCodex))
	writeLengthPrefixed(&b, authority.StableThreadID)
	writeLengthPrefixed(&b, string(authority.Kind))
	writeLengthPrefixed(&b, authority.CurrentPointer)
	writeLengthPrefixed(&b, authority.PhysicalSourceID)
	writeLengthPrefixed(&b, strings.TrimSpace(string(authority.HistoryMode)))
	writeLengthPrefixed(&b, authority.RootID)
	writeLengthPrefixed(&b, authority.ParentID)
	writeLengthPrefixed(&b, authority.ForkSourceID)
	writeLengthPrefixed(&b, authority.HistoryBaseThreadID)
	writeLengthPrefixed(&b, codexOptionalInt64(authority.CopyBoundary))
	writeLengthPrefixed(&b, fmt.Sprintf("%t", authority.OriginalOwnershipProven))
	for _, ref := range authority.References {
		writeLengthPrefixed(&b, ref.Pointer)
		writeLengthPrefixed(&b, ref.PhysicalSourceID)
		writeLengthPrefixed(&b, string(ref.Mode))
		writeLengthPrefixed(&b, ref.HistoryKind)
		writeLengthPrefixed(&b, fmt.Sprintf("%t", ref.ThroughCompleted))
		writeLengthPrefixed(&b, string(ref.Inclusion))
		writeLengthPrefixed(&b, string(ref.Coordinates.Kind))
		writeLengthPrefixed(&b, codexOptionalInt64(ref.Coordinates.Start))
		writeLengthPrefixed(&b, codexOptionalInt64(ref.Coordinates.EndExclusive))
		writeLengthPrefixed(&b, codexOptionalInt64(ref.Coordinates.DecodedByteStart))
		writeLengthPrefixed(&b, codexOptionalInt64(ref.Coordinates.DecodedByteEndExclusive))
		writeLengthPrefixed(&b, codexOptionalInt64(ref.CopyBoundary))
		writeLengthPrefixed(&b, fmt.Sprintf("%t", ref.OriginalOwnershipProven))
	}
	return b.String()
}

// codexFingerprint is the SHA-256 of canonical length-prefixed local fields:
// the authority signature, the completeness, the decoded prefix length and
// digest, the valid decoded record/ordinal checkpoint and partial-tail
// evidence per segment, and the ordered per-segment identities and bounded
// byte digests. Parent whole-file mtime, size and digest after the child
// cutoff are NOT inputs.
func codexFingerprint(authority CodexSourceAuthority, completeness indexformat.GenerationCompleteness, segments []codexDecodedSegment) string {
	hash := sha256.New()
	writeHashedLengthPrefixed(hash, codexAuthoritySignature(authority))
	writeHashedLengthPrefixed(hash, string(completeness))
	for _, segment := range segments {
		writeHashedLengthPrefixed(hash, segment.descriptor.PhysicalSourceID)
		writeHashedLengthPrefixed(hash, string(segment.descriptor.Mode))
		writeHashedLengthPrefixed(hash, string(segment.descriptor.Coordinates.Kind))
		writeHashedLengthPrefixed(hash, codexOptionalInt64(segment.descriptor.Coordinates.Start))
		writeHashedLengthPrefixed(hash, codexOptionalInt64(segment.descriptor.Coordinates.EndExclusive))
		checkpoint := segment.boundedCheckpoint()
		writeHashedLengthPrefixed(hash, fmt.Sprintf("%d", checkpoint.validRecords))
		writeHashedLengthPrefixed(hash, codexOptionalInt64(checkpoint.maxOrdinalPtr()))
		bounded := segment.boundedData()
		writeHashedLengthPrefixed(hash, fmt.Sprintf("%d", len(bounded)))
		digest := sha256.Sum256(bounded)
		writeHashedLengthPrefixed(hash, hex.EncodeToString(digest[:]))
		tail := segment.partialTail()
		writeHashedLengthPrefixed(hash, fmt.Sprintf("%d", len(tail)))
		tailDigest := sha256.Sum256(tail)
		writeHashedLengthPrefixed(hash, hex.EncodeToString(tailDigest[:]))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func writeLengthPrefixed(b *strings.Builder, value string) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	b.Write(length[:])
	b.WriteString(value)
}

func writeHashedLengthPrefixed(h interface{ Write([]byte) (int, error) }, value string) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = h.Write(length[:])
	_, _ = h.Write([]byte(value))
}

func codexOptionalInt64(value *int64) string {
	if value == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%d", *value)
}

// codexPhysicalSourceID encodes a physical source identity for local capture
// evidence. It is a bounded opaque digest, never the raw private path.
func codexPhysicalSourceID(path string) string {
	digest := sha256.Sum256([]byte(filepath.Clean(path)))
	return "src_" + hex.EncodeToString(digest[:8])
}

// codexHeaderLimit bounds the header peek used to read session_meta. A Codex
// session_meta line is small; the injected conversation content that can be
// large lives in response_item records, not the header.
const codexHeaderLimit = 1 << 20

// CodexCurrentMissingError reports that the authoritative current Codex
// source is missing. It is never replaced by an older rollout: the caller
// retains the last good generation and retries when the source returns.
type CodexCurrentMissingError struct {
	StableThreadID string
	SourceRef      string
	// Cause is the sanitized filesystem classification (not found,
	// permission denied, unreadable). It never carries a raw path.
	Cause string
}

func (e *CodexCurrentMissingError) Error() string {
	cause := e.Cause
	if cause == "" {
		cause = "the native source is not found"
	}
	return fmt.Sprintf("ingest.CodexFileSource.ResolveCodexAuthority: the authoritative current Codex source for thread %q (source %s) is unavailable (%s) at the resolve-authority-before-diff step; no older rollout was substituted and the last good generation is retained; restore the native current rollout and rerun harvest", e.StableThreadID, e.SourceRef, cause)
}

// CodexAuthorityConflictError reports competing detached candidates for one
// stable thread. Selection by newest mtime is refused; the caller keeps one
// logical record in last-good/unresolved state until native evidence (an
// authoritative replacement pointer) resolves the conflict.
type CodexAuthorityConflictError struct {
	StableThreadID string
	Candidates     int
	Refs           []string
}

func (e *CodexAuthorityConflictError) Error() string {
	return fmt.Sprintf("ingest.CodexFileSource.ResolveCodexAuthority: %d competing detached Codex sources claim thread %q (sources %s); no candidate was selected by file age and no session was invented; the logical record stays last-good/unresolved until an authoritative replacement pointer resolves the conflict", e.Candidates, e.StableThreadID, strings.Join(e.Refs, ","))
}

// CodexIncompleteCaptureError reports that the verified capture is
// incomplete (missing reference proof, unsupported mode, misaligned
// checkpoint). No replacement result is produced: the caller retains the
// last good generation and versions. The diagnostics name the missing
// proof without raw private paths.
type CodexIncompleteCaptureError struct {
	StableThreadID string
	Completeness   indexformat.GenerationCompleteness
	Diagnostics    []DiagnosticEntry
}

func (e *CodexIncompleteCaptureError) Error() string {
	kinds := make([]string, 0, len(e.Diagnostics))
	for _, diagnostic := range e.Diagnostics {
		kinds = append(kinds, diagnostic.ErrorType)
	}
	return fmt.Sprintf("ingest.CodexIndexer.captureCurrentSource: the verified Codex capture for thread %q is %s (%s); no replacement result was produced and the last good generation and versions were retained; repair the native history proof and retry harvest", e.StableThreadID, e.Completeness, strings.Join(kinds, ","))
}

// codexSanitizedFSCause classifies a filesystem failure without its raw path
// or raw error text, so diagnostics never leak private source locations.
func codexSanitizedFSCause(err error) string {
	if os.IsNotExist(err) {
		return "the native source is not found"
	}
	if os.IsPermission(err) {
		return "the native source could not be read (permission denied)"
	}
	return "the native source could not be read"
}

// codexAuthorityReadFailure builds the actionable source-boundary error for a
// failed authority or segment read. It names the module/function, the failed
// step, the sanitized reason, the caller effect and the safe recovery; it
// never carries a raw private path or a wrapped OS error.
func codexAuthorityReadFailure(op, stableID, sourceRef, step, reason string) error {
	return fmt.Errorf("ingest.CodexFileSource.%s: %s for thread %q (source %s) at the %s step failed because %s; the capture is incomplete, no older rollout was substituted and no prior snapshot was overwritten; restore the native source and rerun harvest", op, step, stableID, sourceRef, step, reason)
}

// CodexNativePointerRecord is the native current-rollout pointer storage record
// for one stable thread: the authoritative current rollout path the native
// database records and the raw history mode it stores beside it. Both are
// native evidence; neither is a deployment-managed document.
type CodexNativePointerRecord struct {
	Pointer     string
	HistoryMode json.RawMessage
}

// CodexNativePointerStore reads the native current-rollout pointer storage
// read-only. The production implementation opens the native Codex state
// database; a test may supply a real temporary database. It never writes the
// storage and never starts a native process.
type CodexNativePointerStore interface {
	// NativeCurrentRollout returns the current rollout path the native store
	// records for one stable thread. found is false when the native store holds
	// no row for the thread, which is the only case that permits detached-file
	// fallback.
	NativeCurrentRollout(ctx context.Context, stableThreadID string) (CodexNativePointerRecord, bool, error)
}

// CodexSQLitePointerStore reads the native current-rollout pointer from the
// read-only native Codex state database through the dedicated native-state
// reader. It performs no SQLite access itself, so the ingest package keeps its
// fixed, statically attributable SQL inventory.
type CodexSQLitePointerStore struct {
	store *codexstate.PointerStore
}

// NewCodexSQLitePointerStore creates a read-only native state-database pointer
// store over the given database path.
func NewCodexSQLitePointerStore(path string) *CodexSQLitePointerStore {
	return &CodexSQLitePointerStore{store: codexstate.NewPointerStore(path)}
}

var _ CodexNativePointerStore = (*CodexSQLitePointerStore)(nil)

// NativeCurrentRollout queries the native threads table read-only for the one
// current rollout path of a stable thread and projects it into the capture's
// raw-history-mode shape.
func (s *CodexSQLitePointerStore) NativeCurrentRollout(ctx context.Context, stableThreadID string) (CodexNativePointerRecord, bool, error) {
	record, found, err := s.store.CurrentRollout(ctx, stableThreadID)
	if err != nil {
		return CodexNativePointerRecord{}, false, err
	}
	projected := CodexNativePointerRecord{Pointer: record.Pointer}
	if record.HistoryMode != "" {
		if encoded, marshalErr := json.Marshal(record.HistoryMode); marshalErr == nil {
			projected.HistoryMode = encoded
		}
	}
	return projected, found, nil
}

// CodexFileSourceOption configures a CodexFileSource.
type CodexFileSourceOption func(*CodexFileSource)

// WithCodexNativePointerStore injects the native current-rollout pointer store.
// A production caller leaves it unset: the source then derives the native state
// database from the session's native sessions tree. It is injectable so a test
// can drive a real temporary database without a global.
func WithCodexNativePointerStore(store CodexNativePointerStore) CodexFileSourceOption {
	return func(s *CodexFileSource) { s.pointerStore = store }
}

// CodexFileSource is the production read-only source. It resolves the stable
// session_meta.id and exactly one current pointer before any diff, in the
// ratified order: a native live-writer pointer when the read-only source
// exposes one, then the native SQLite current-rollout pointer, then detached
// file discovery only when no pointer authority exists. It derives the raw
// history mode, fork/parent/root/base evidence, the copied-creation boundary
// and the ordered native history references from native session_meta
// envelopes, refuses competing candidates instead of guessing by lexical or
// file-age order, and never writes the native source or starts a native
// process.
type CodexFileSource struct {
	fs           FileSystem
	pointerStore CodexNativePointerStore
}

// NewCodexFileSource creates the production read-only Codex source over fs.
func NewCodexFileSource(fs FileSystem, opts ...CodexFileSourceOption) *CodexFileSource {
	source := &CodexFileSource{fs: fs}
	for _, opt := range opts {
		opt(source)
	}
	return source
}

var _ CodexReadOnlySource = (*CodexFileSource)(nil)

// maxCodexHistoryDepth bounds recursive native history-reference resolution.
// maxCodexHistorySegments bounds the ordered decoded segments of one capture.
const (
	maxCodexHistoryDepth    = 8
	maxCodexHistorySegments = 32
)

// ResolveCodexAuthority selects the stable session_meta.id and the one current
// pointer before any diff. The ratified order is a native live-writer pointer
// when the read-only source exposes one, then the native SQLite current-rollout
// pointer, then detached file discovery only when no pointer authority exists.
// Exactly one detached candidate for the thread is usable and several
// conflicting candidates are refused. A paginated current source that is
// missing is an error; it is never replaced by an older rollout.
func (s *CodexFileSource) ResolveCodexAuthority(ctx context.Context, session DiscoveredSession) (CodexSourceAuthority, error) {
	if err := ctx.Err(); err != nil {
		return CodexSourceAuthority{}, fmt.Errorf("ingest.CodexFileSource.ResolveCodexAuthority: the Codex capture for session %s was cancelled before the current source could be resolved; no source was read and no state changed; retry the harvest", session.SessionID)
	}
	authority, handled, pointerErr := s.resolveNativeAuthority(ctx, session)
	if handled {
		return authority, pointerErr
	}
	detached, err := s.resolveDetachedAuthority(ctx, session)
	if err != nil {
		return CodexSourceAuthority{}, err
	}
	if pointerErr != nil {
		detached.DerivationIncomplete = true
		detached.DerivationDiagnostics = append(detached.DerivationDiagnostics, DiagnosticEntry{
			ErrorType:   "codex_native_pointer_unreadable",
			Location:    fmt.Sprintf("thread %s", detached.StableThreadID),
			Message:     "the native current-rollout pointer storage could not be read; the capture fell back to detached-file authority and no older rollout was preferred",
			Remediation: "Repair the native Codex state database and rerun; the capture stays incomplete until the native pointer authority proves.",
		})
	}
	return detached, nil
}

// resolveNativeAuthority resolves the native SQLite current-rollout pointer for
// the discovered session. handled is true when a native pointer was read and
// the returned authority or error is final; handled is false when no pointer
// authority exists (or it could not be read), which permits detached fallback.
func (s *CodexFileSource) resolveNativeAuthority(ctx context.Context, session DiscoveredSession) (CodexSourceAuthority, bool, error) {
	store := s.nativePointerStoreFor(session)
	if store == nil {
		return CodexSourceAuthority{}, false, nil
	}
	key := session.SessionID.String()
	record, found, err := store.NativeCurrentRollout(ctx, key)
	if err != nil {
		return CodexSourceAuthority{}, false, err
	}
	if !found {
		return CodexSourceAuthority{}, false, nil
	}
	pointer := filepath.Clean(record.Pointer)
	header, headerErr := codexReadHeader(s.fs, pointer, codexHeaderLimit)
	if headerErr != nil {
		return CodexSourceAuthority{}, true, &CodexCurrentMissingError{
			StableThreadID: key,
			SourceRef:      codexPhysicalSourceID(pointer),
			Cause:          codexSanitizedFSCause(headerErr),
		}
	}
	meta, hasMeta := parseCodexNativeAuthority(header)
	stableID := key
	if hasMeta && meta.ID != "" {
		stableID = meta.ID
	}
	authority := s.deriveAuthority(stableID, CodexAuthorityNativeCurrentPointer, pointer, meta, hasMeta)
	if stableID != key {
		authority.DerivationIncomplete = true
		authority.DerivationDiagnostics = append(authority.DerivationDiagnostics, DiagnosticEntry{
			ErrorType:   "codex_native_pointer_identity_mismatch",
			Location:    fmt.Sprintf("thread %s", key),
			Message:     "the native current-rollout row names a rollout whose session_meta.id differs from the requested thread; the native pointer was used and the mismatch is recorded as incomplete instead of silently re-identifying the thread",
			Remediation: "Repair the native current-rollout row and rerun; the capture stays incomplete and the last good generation is retained.",
		})
	}
	if len(bytes.TrimSpace(authority.HistoryMode)) == 0 && len(bytes.TrimSpace(record.HistoryMode)) > 0 {
		authority.HistoryMode = record.HistoryMode
	}
	if err := s.resolveDerivedReferences(ctx, &authority, meta, 0, newCodexReferenceResolution(pointer)); err != nil {
		return CodexSourceAuthority{}, true, err
	}
	return authority, true, nil
}

// nativePointerStoreFor returns the native current-rollout pointer store for a
// discovered session. An injected store wins; otherwise the native
// state_<n>.sqlite beside the session's sessions tree is opened read-only. A
// layout without a native sessions tree, or one with no state database, has no
// pointer authority and returns nil.
func (s *CodexFileSource) nativePointerStoreFor(session DiscoveredSession) CodexNativePointerStore {
	if s.pointerStore != nil {
		return s.pointerStore
	}
	sessionsRoot := codexSessionsRoot(session.SourcePath.String())
	if filepath.Base(sessionsRoot) != "sessions" {
		return nil
	}
	home := filepath.Dir(sessionsRoot)
	if path := codexNativeStateDatabasePath(s.fs, home); path != "" {
		return NewCodexSQLitePointerStore(path)
	}
	return nil
}

// codexNativeStateDatabasePath picks the highest-versioned state_<n>.sqlite in
// the native Codex home. Discovery is read-only; a missing directory or no
// candidate returns "".
func codexNativeStateDatabasePath(fs FileSystem, home string) string {
	entries, err := fs.ReadDir(home)
	if err != nil {
		return ""
	}
	bestVersion := -1
	bestPath := ""
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, "state_") || !strings.HasSuffix(name, ".sqlite") {
			continue
		}
		digits := strings.TrimSuffix(strings.TrimPrefix(name, "state_"), ".sqlite")
		if !isDecimalDigits(digits) {
			continue
		}
		version, convErr := strconv.Atoi(digits)
		if convErr != nil {
			continue
		}
		if version > bestVersion {
			bestVersion = version
			bestPath = filepath.Join(home, name)
		}
	}
	return bestPath
}

// resolveDetachedAuthority derives the authority from the session's own
// rollout file and refuses competing same-thread candidates.
func (s *CodexFileSource) resolveDetachedAuthority(ctx context.Context, session DiscoveredSession) (CodexSourceAuthority, error) {
	if err := ctx.Err(); err != nil {
		return CodexSourceAuthority{}, fmt.Errorf("ingest.CodexFileSource.ResolveCodexAuthority: the Codex capture for session %s was cancelled before the detached authority could be resolved; no source was read and no state changed; retry the harvest", session.SessionID)
	}
	pointer := session.SourcePath.String()
	header, err := codexReadHeader(s.fs, pointer, codexHeaderLimit)
	if err != nil {
		return CodexSourceAuthority{}, &CodexCurrentMissingError{
			StableThreadID: session.SessionID.String(),
			SourceRef:      codexPhysicalSourceID(pointer),
			Cause:          codexSanitizedFSCause(err),
		}
	}
	meta, hasMeta := parseCodexNativeAuthority(header)
	stableID := session.SessionID.String()
	if hasMeta && meta.ID != "" {
		stableID = meta.ID
	}
	if conflicts := s.scanThreadCandidates(ctx, pointer, stableID); len(conflicts) > 1 {
		refs := make([]string, 0, len(conflicts))
		for _, candidate := range conflicts {
			refs = append(refs, codexPhysicalSourceID(candidate))
		}
		return CodexSourceAuthority{}, &CodexAuthorityConflictError{
			StableThreadID: stableID,
			Candidates:     len(conflicts),
			Refs:           refs,
		}
	}
	authority := s.deriveAuthority(stableID, CodexAuthorityDetachedFile, pointer, meta, hasMeta)
	if err := s.resolveDerivedReferences(ctx, &authority, meta, 0, newCodexReferenceResolution(pointer)); err != nil {
		return CodexSourceAuthority{}, err
	}
	return authority, nil
}

// deriveAuthority projects a parsed native envelope into the authority shape.
// Bounds the envelope proves stay; bounds it cannot prove stay absent.
func (s *CodexFileSource) deriveAuthority(stableID string, kind CodexSourceAuthorityKind, pointer string, meta codexNativeSessionMeta, hasMeta bool) CodexSourceAuthority {
	authority := CodexSourceAuthority{
		StableThreadID:   stableID,
		Kind:             kind,
		CurrentPointer:   pointer,
		PhysicalSourceID: codexPhysicalSourceID(pointer),
		HistoryMode:      metaHistoryModeRaw(meta, hasMeta),
	}
	if !hasMeta {
		return authority
	}
	authority.ForkSourceID = meta.ForkedFromID
	authority.ParentID = meta.nativeParentThreadID()
	if meta.RootSessionID != "" && meta.RootSessionID != stableID {
		authority.RootID = meta.RootSessionID
	}
	if meta.HistoryBaseThreadID != "" {
		authority.HistoryBaseThreadID = meta.HistoryBaseThreadID
	}
	if meta.CopyBoundary != nil && *meta.CopyBoundary >= 0 {
		authority.CopyBoundary = meta.CopyBoundary
		authority.OriginalOwnershipProven = meta.ForkedFromID != "" || len(meta.History) > 0
	} else if meta.CopyBoundary != nil {
		authority.DerivationIncomplete = true
		authority.DerivationDiagnostics = append(authority.DerivationDiagnostics, DiagnosticEntry{
			ErrorType:   "codex_copy_boundary_malformed",
			Location:    fmt.Sprintf("thread %s", stableID),
			Message:     "the native copied-creation boundary is negative; no boundary was invented and the prefix stays own content",
			Remediation: "Repair the native session_meta envelope and rerun; the capture used no copied prefix.",
		})
	}
	if meta.HistoryMalformed {
		authority.DerivationIncomplete = true
		authority.DerivationDiagnostics = append(authority.DerivationDiagnostics, DiagnosticEntry{
			ErrorType:   "codex_history_envelope_malformed",
			Location:    fmt.Sprintf("thread %s", stableID),
			Message:     "the native history envelope beside the current session_meta could not be decoded; no dependency was invented and the current records replay alone",
			Remediation: "Repair the native history envelope and rerun; the capture stays incomplete until the dependencies prove.",
		})
	}
	return authority
}

// codexReferenceResolution tracks the active resolver recursion separately from
// the pointers already retained, so a true recursion-stack cycle is
// distinguishable from a retained reference that is legitimately reachable by
// more than one ordered dependency.
type codexReferenceResolution struct {
	stack    map[string]bool
	retained map[string]bool
}

// newCodexReferenceResolution starts a resolution with the current source
// pointer on the active stack: a dependency that points back at the current
// source is a cycle, not a retained reference.
func newCodexReferenceResolution(currentPointer string) *codexReferenceResolution {
	return &codexReferenceResolution{
		stack:    map[string]bool{currentPointer: true},
		retained: map[string]bool{currentPointer: true},
	}
}

// resolveDerivedReferences resolves the ordered native history references
// oldest-to-newest, recursing into each referenced native envelope with a
// depth bound and an active recursion stack so a reference cycle can never loop
// the resolver. Every recursive cycle, ambiguity, unavailable dependency and
// depth/segment truncation is propagated into the authority as
// DerivationIncomplete plus a diagnostic, so the capture can never certify an
// unresolved dependency graph as complete. Unresolvable references are also
// kept as unavailable evidence; the replay proves coverage against them
// instead of guessing parent content.
func (s *CodexFileSource) resolveDerivedReferences(ctx context.Context, authority *CodexSourceAuthority, meta codexNativeSessionMeta, depth int, resolution *codexReferenceResolution) error {
	if len(meta.History) == 0 && meta.HistoryBaseThreadID == "" {
		return nil
	}
	if depth >= maxCodexHistoryDepth {
		authority.DerivationIncomplete = true
		authority.DerivationDiagnostics = append(authority.DerivationDiagnostics, DiagnosticEntry{
			ErrorType:   "codex_reference_depth_exceeded",
			Location:    fmt.Sprintf("thread %s", authority.StableThreadID),
			Message:     "native history references recurse deeper than the bounded resolver follows; deeper ancestors were not invented",
			Remediation: "Flatten the native history chain and rerun; the resolved prefix replays and the capture records the bound.",
		})
		return nil
	}
	return s.resolveHistoryEntries(ctx, authority, meta, depth, resolution)
}

// resolveSibling resolves one referenced native envelope into an independent
// sibling authority and merges every recursive flag and diagnostic back into
// the parent, so a nested cycle or truncation is never silently discarded.
func (s *CodexFileSource) resolveSibling(ctx context.Context, authority *CodexSourceAuthority, meta codexNativeSessionMeta, depth int, resolution *codexReferenceResolution) []CodexReference {
	sibling := *authority
	sibling.References = nil
	sibling.DerivationIncomplete = false
	sibling.DerivationDiagnostics = nil
	_ = s.resolveDerivedReferences(ctx, &sibling, meta, depth+1, resolution)
	authority.DerivationIncomplete = authority.DerivationIncomplete || sibling.DerivationIncomplete
	authority.DerivationDiagnostics = append(authority.DerivationDiagnostics, sibling.DerivationDiagnostics...)
	return sibling.References
}

// resolveHistoryEntries resolves the declared native history array
// oldest-to-newest after the history-base reference. A dependency whose thread
// identity maps to more than one native rollout is refused as ambiguous rather
// than selected by lexical order; a dependency that points back into the active
// recursion stack is recorded as a cycle; a dependency already retained by
// another ordered path is a retained reference and is not re-added.
func (s *CodexFileSource) resolveHistoryEntries(ctx context.Context, authority *CodexSourceAuthority, meta codexNativeSessionMeta, depth int, resolution *codexReferenceResolution) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	index := s.scanThreadIndex(ctx, authority.CurrentPointer)
	var ordered []CodexReference
	if meta.HistoryBaseThreadID != "" {
		bases := index[meta.HistoryBaseThreadID]
		switch {
		case len(bases) > 1:
			ordered = append(ordered, s.ambiguousDependencyReference(authority, "history-base", meta.HistoryBaseThreadID))
		case len(bases) == 1:
			basePath := bases[0]
			switch {
			case resolution.stack[basePath]:
				ordered = append(ordered, s.cycleDependencyReference(authority, indexformat.CoordinateKindUnknown, basePath, nil, nil, nil, nil, "", resolveCodexHistoryMode(authority.HistoryMode)))
			case resolution.retained[basePath]:
				// Retained by an earlier ordered dependency: not a cycle, and
				// no duplicate reference is added.
			default:
				resolution.stack[basePath] = true
				if baseMeta, hasBase, err := s.readNativeMeta(basePath); err == nil && hasBase {
					ordered = append(ordered, s.resolveSibling(ctx, authority, baseMeta, depth, resolution)...)
				}
				delete(resolution.stack, basePath)
				resolution.retained[basePath] = true
				ordered = append(ordered, CodexReference{
					Pointer:          basePath,
					PhysicalSourceID: codexPhysicalSourceID(basePath),
					Mode:             resolveCodexHistoryMode(authority.HistoryMode),
					Inclusion:        codexHistoryBaseInclusion(authority, meta),
					Coordinates:      indexformat.SegmentCoordinates{Kind: indexformat.CoordinateKindUnknown},
				})
			}
		default:
			ordered = append(ordered, CodexReference{
				Pointer:          "history-base:" + meta.HistoryBaseThreadID,
				PhysicalSourceID: codexPhysicalSourceID("history-base:" + meta.HistoryBaseThreadID),
				Mode:             resolveCodexHistoryMode(authority.HistoryMode),
				Inclusion:        codexHistoryBaseInclusion(authority, meta),
				Coordinates:      indexformat.SegmentCoordinates{Kind: indexformat.CoordinateKindUnknown},
			})
		}
	}
	for _, entry := range meta.History {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(ordered)+len(authority.References) >= maxCodexHistorySegments {
			authority.DerivationIncomplete = true
			authority.DerivationDiagnostics = append(authority.DerivationDiagnostics, DiagnosticEntry{
				ErrorType:   "codex_reference_segments_bounded",
				Location:    fmt.Sprintf("thread %s", authority.StableThreadID),
				Message:     "native history references exceed the bounded segment budget; later dependencies were not invented",
				Remediation: "Flatten the native history chain and rerun; the resolved prefix replays and the capture records the bound.",
			})
			break
		}
		mode := entry.Mode
		if !entry.ModeSpecified {
			mode = resolveCodexHistoryMode(authority.HistoryMode)
		}
		targets := index[entry.ThreadID]
		switch {
		case len(targets) > 1:
			ordered = append(ordered, s.ambiguousDependencyReference(authority, "history", entry.ThreadID))
			continue
		case len(targets) == 0:
			ordered = append(ordered, CodexReference{
				Pointer:          "history:" + entry.ThreadID,
				PhysicalSourceID: codexPhysicalSourceID("history:" + entry.ThreadID),
				Mode:             mode,
				HistoryKind:      entry.Kind,
				ThroughCompleted: entry.Completed,
				Inclusion:        indexformat.SegmentInclusionInherited,
				Coordinates: indexformat.SegmentCoordinates{
					Kind:                    indexformat.CoordinateKindCodexReferenceRange,
					Start:                   entry.Start,
					EndExclusive:            entry.EndExclusive,
					DecodedByteStart:        entry.DecodedByteStart,
					DecodedByteEndExclusive: entry.DecodedByteEndExclusive,
				},
			})
			continue
		}
		target := targets[0]
		if resolution.stack[target] {
			ordered = append(ordered, s.cycleDependencyReference(
				authority,
				indexformat.CoordinateKindCodexReferenceRange,
				target,
				entry.Start,
				entry.EndExclusive,
				entry.DecodedByteStart,
				entry.DecodedByteEndExclusive,
				entry.Kind,
				mode,
			))
			continue
		}
		if resolution.retained[target] {
			continue
		}
		resolution.stack[target] = true
		if refMeta, hasRef, err := s.readNativeMeta(target); err == nil && hasRef {
			if !entry.ModeSpecified {
				mode = resolveCodexHistoryMode(refMeta.HistoryMode)
			}
			ordered = append(ordered, s.resolveSibling(ctx, authority, refMeta, depth, resolution)...)
		}
		delete(resolution.stack, target)
		resolution.retained[target] = true
		ordered = append(ordered, CodexReference{
			Pointer:          target,
			PhysicalSourceID: codexPhysicalSourceID(target),
			Mode:             mode,
			HistoryKind:      entry.Kind,
			ThroughCompleted: entry.Completed,
			Inclusion:        indexformat.SegmentInclusionInherited,
			Coordinates: indexformat.SegmentCoordinates{
				Kind:                    indexformat.CoordinateKindCodexReferenceRange,
				Start:                   entry.Start,
				EndExclusive:            entry.EndExclusive,
				DecodedByteStart:        entry.DecodedByteStart,
				DecodedByteEndExclusive: entry.DecodedByteEndExclusive,
			},
		})
	}
	authority.References = append(authority.References, ordered...)
	return nil
}

// ambiguousDependencyReference records a native dependency whose thread
// identity maps to more than one rollout. It refuses to select a candidate by
// lexical order, marks the derivation incomplete, and returns the incomplete
// reference the replay can never read.
func (s *CodexFileSource) ambiguousDependencyReference(authority *CodexSourceAuthority, kind, threadID string) CodexReference {
	authority.DerivationIncomplete = true
	authority.DerivationDiagnostics = append(authority.DerivationDiagnostics, DiagnosticEntry{
		ErrorType:   "codex_reference_ambiguous",
		Location:    fmt.Sprintf("thread %s %s dependency", authority.StableThreadID, kind),
		Message:     fmt.Sprintf("the native %s dependency names thread %s, but several native rollouts in the sessions tree claim that identity; no candidate was selected by lexical order and no parent content was guessed", kind, threadID),
		Remediation: "Resolve the competing native rollouts to one authoritative incarnation and rerun; the capture stays incomplete and the last good generation is retained.",
	})
	return CodexReference{
		Pointer:          "history-ambiguous:" + threadID,
		PhysicalSourceID: codexPhysicalSourceID("history-ambiguous:" + threadID),
		Mode:             resolveCodexHistoryMode(authority.HistoryMode),
		Inclusion:        indexformat.SegmentInclusionInvalidIncomplete,
		Coordinates:      indexformat.SegmentCoordinates{Kind: indexformat.CoordinateKindUnknown},
	}
}

// cycleDependencyReference records a native dependency that points back into
// the active recursion stack. The cycle is retained as invalid/incomplete
// evidence instead of silently dropped, and the derivation is marked
// incomplete so the capture can never certify the unresolved graph complete.
func (s *CodexFileSource) cycleDependencyReference(authority *CodexSourceAuthority, coordinateKind indexformat.CoordinateKind, pointer string, start, endExclusive, decodedByteStart, decodedByteEndExclusive *int64, historyKind string, mode CodexHistoryMode) CodexReference {
	authority.DerivationIncomplete = true
	authority.DerivationDiagnostics = append(authority.DerivationDiagnostics, DiagnosticEntry{
		ErrorType:   "codex_reference_cycle",
		Location:    fmt.Sprintf("thread %s", authority.StableThreadID),
		Message:     "a native history dependency points back into the already-open recursion path; the cycle was recorded and no parent content was guessed or looped",
		Remediation: "Repair the native reference metadata and rerun; the last good generation is retained and no cycle was followed.",
	})
	return CodexReference{
		Pointer:          pointer,
		PhysicalSourceID: codexPhysicalSourceID(pointer),
		Mode:             mode,
		HistoryKind:      historyKind,
		Inclusion:        indexformat.SegmentInclusionInvalidIncomplete,
		Coordinates: indexformat.SegmentCoordinates{
			Kind:                    coordinateKind,
			Start:                   start,
			EndExclusive:            endExclusive,
			DecodedByteStart:        decodedByteStart,
			DecodedByteEndExclusive: decodedByteEndExclusive,
		},
	}
}

// codexHistoryBaseInclusion classifies a history-base reference: a base that
// directly names the logical parent or fork source is proven inherited
// history; any other base is uncertain earlier history, never invented
// parent content.
func codexHistoryBaseInclusion(authority *CodexSourceAuthority, meta codexNativeSessionMeta) indexformat.SegmentInclusion {
	if meta.HistoryBaseThreadID == "" {
		return indexformat.SegmentInclusionUncertainEarlierHistory
	}
	if meta.HistoryBaseThreadID == authority.ParentID || meta.HistoryBaseThreadID == authority.ForkSourceID {
		return indexformat.SegmentInclusionInherited
	}
	return indexformat.SegmentInclusionUncertainEarlierHistory
}

// readNativeMeta reads and parses the native authority envelope of one
// sibling rollout file.
func (s *CodexFileSource) readNativeMeta(path string) (codexNativeSessionMeta, bool, error) {
	header, err := codexReadHeader(s.fs, path, codexHeaderLimit)
	if err != nil {
		return codexNativeSessionMeta{}, false, err
	}
	meta, ok := parseCodexNativeAuthority(header)
	return meta, ok, nil
}

// scanThreadCandidates lists every sibling rollout path whose native
// session_meta.id names stableID, oldest path first. Unreadable siblings are
// skipped; readable conflicts are never hidden by file age.
func (s *CodexFileSource) scanThreadCandidates(ctx context.Context, pointer, stableID string) []string {
	candidates := append([]string(nil), s.scanThreadIndex(ctx, pointer)[stableID]...)
	sortStrings(candidates)
	return candidates
}

// scanThreadIndex maps native session_meta.id to every rollout path that
// claims it, oldest path first, over the native sessions tree that contains
// pointer. The scan is read-only and bounded to header reads.
func (s *CodexFileSource) scanThreadIndex(ctx context.Context, pointer string) map[string][]string {
	index := map[string][]string{}
	root := codexSessionsRoot(pointer)
	_ = s.fs.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return nil
		}
		if d.IsDir() || d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if !strings.HasSuffix(filepath.Base(path), ".jsonl") || !strings.HasPrefix(filepath.Base(path), codexRolloutFilePrefix) {
			return nil
		}
		header, readErr := codexReadHeader(s.fs, path, codexHeaderLimit)
		if readErr != nil {
			return nil
		}
		meta, ok := parseCodexNativeAuthority(header)
		if !ok || meta.ID == "" {
			return nil
		}
		index[meta.ID] = append(index[meta.ID], path)
		return nil
	})
	for id := range index {
		sortStrings(index[id])
	}
	return index
}

// codexSessionsRoot finds the native sessions tree that contains a rollout
// path. A Codex date-partitioned layout root/YYYY/MM/DD/file resolves to
// root; any other layout resolves to the containing directory.
func codexSessionsRoot(pointer string) string {
	dir := filepath.Dir(pointer)
	day := filepath.Base(dir)
	month := filepath.Base(filepath.Dir(dir))
	year := filepath.Base(filepath.Dir(filepath.Dir(dir)))
	if len(year) == 4 && len(month) == 2 && len(day) == 2 && isDecimalDigits(year) && isDecimalDigits(month) && isDecimalDigits(day) {
		return filepath.Dir(filepath.Dir(filepath.Dir(dir)))
	}
	return dir
}

func isDecimalDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

// ReadCodexSource reads the bounded decoded bytes at one pointer. The
// descriptor-size capture boundary is used when the FileSystem can resolve it.
// Failures are sanitized at this source boundary: no raw path and no wrapped
// OS error ever leaves it.
func (s *CodexFileSource) ReadCodexSource(_ context.Context, pointer string) ([]byte, error) {
	if strings.HasPrefix(pointer, "history:") || strings.HasPrefix(pointer, "history-base:") || strings.HasPrefix(pointer, "history-ambiguous:") {
		thread := strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(pointer, "history-base:"), "history:"), "history-ambiguous:")
		return nil, codexAuthorityReadFailure("ReadCodexSource", thread, codexPhysicalSourceID(pointer), "read bounded dependency", "the referenced native source is not present in the sessions tree")
	}
	if reader, ok := s.fs.(sourcePrefixReader); ok {
		data, err := reader.ReadSourcePrefix(pointer)
		if err != nil {
			return nil, codexAuthorityReadFailure("ReadCodexSource", "", codexPhysicalSourceID(pointer), "bounded read", codexSanitizedFSCause(err))
		}
		return data, nil
	}
	data, err := s.fs.ReadFile(pointer)
	if err != nil {
		return nil, codexAuthorityReadFailure("ReadCodexSource", "", codexPhysicalSourceID(pointer), "read", codexSanitizedFSCause(err))
	}
	return data, nil
}

// codexReadHeader reads at most limit header bytes, falling back to a full read
// for filesystems without the optional header capability.
func codexReadHeader(fs FileSystem, path string, limit int) ([]byte, error) {
	if reader, ok := fs.(fileHeaderReader); ok {
		return reader.ReadFileHeader(path, limit)
	}
	data, err := fs.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return data[:limit], nil
	}
	return data, nil
}

// codexNativeHistoryEntry is one declared native history dependency decoded
// from the session_meta history envelope. ThreadID names the referenced
// stable thread; Start/EndExclusive are decoded-ordinal bounds and the byte
// bounds are decoded-byte bounds. Mode is the declared history mode of the
// referenced segment. The envelope is optional: absent history means no
// declared dependencies, and a malformed envelope marks the authority
// derivation incomplete without inventing a dependency.
type codexNativeHistoryEntry struct {
	ThreadID                string
	Mode                    CodexHistoryMode
	ModeSpecified           bool
	Kind                    string
	Completed               bool
	Start                   *int64
	EndExclusive            *int64
	DecodedByteStart        *int64
	DecodedByteEndExclusive *int64
}

// codexNativeSessionMeta is the authority-relevant native session_meta
// envelope decoded from real rollout bytes. Every field is optional except
// the stable identity: absent bounds stay absent and malformed envelopes
// stay incomplete, never invented.
type codexNativeSessionMeta struct {
	ID                  string
	HistoryMode         json.RawMessage
	ForkedFromID        string
	ParentThreadID      string
	Source              json.RawMessage
	RootSessionID       string
	HistoryBaseThreadID string
	CopyBoundary        *int64
	History             []codexNativeHistoryEntry
	HistoryMalformed    bool
}

// nativeParentThreadID reads the nested thread-spawn parent when the
// top-level field is absent. Nesting alone is not a cycle proof.
func (m codexNativeSessionMeta) nativeParentThreadID() string {
	if m.ParentThreadID != "" {
		return m.ParentThreadID
	}
	if len(m.Source) == 0 {
		return ""
	}
	var source struct {
		Subagent struct {
			ThreadSpawn struct {
				ParentThreadID string `json:"parent_thread_id"`
			} `json:"thread_spawn"`
		} `json:"subagent"`
	}
	if err := json.Unmarshal(m.Source, &source); err != nil {
		return ""
	}
	return source.Subagent.ThreadSpawn.ParentThreadID
}

// parseCodexNativeAuthority finds the first session_meta record in a header
// peek and decodes its authority-relevant native envelope. It reports false
// when no session_meta could be decoded.
func parseCodexNativeAuthority(header []byte) (codexNativeSessionMeta, bool) {
	scanner := newJSONLRecordScanner(header, defaults.MaxJSONLRecordBytes)
	for scanner.Scan() {
		raw := strings.TrimSpace(string(scanner.Bytes()))
		if raw == "" {
			continue
		}
		var env codexRolloutLine
		if err := json.Unmarshal([]byte(raw), &env); err != nil {
			return codexNativeSessionMeta{}, false
		}
		if env.Type != codexTypeSessionMeta {
			return codexNativeSessionMeta{}, false
		}
		var wire struct {
			ID             string          `json:"id"`
			HistoryMode    json.RawMessage `json:"history_mode"`
			ForkedFromID   string          `json:"forked_from_id"`
			ParentThreadID string          `json:"parent_thread_id"`
			Source         json.RawMessage `json:"source"`
			RootSessionID  string          `json:"session_id"`
			HistoryBase    json.RawMessage `json:"history_base"`
			CopyBoundary   *int64          `json:"subagent_history_start_ordinal"`
			History        json.RawMessage `json:"history"`
		}
		if err := json.Unmarshal(env.Payload, &wire); err != nil {
			return codexNativeSessionMeta{}, false
		}
		meta := codexNativeSessionMeta{
			ID:             wire.ID,
			HistoryMode:    wire.HistoryMode,
			ForkedFromID:   wire.ForkedFromID,
			ParentThreadID: wire.ParentThreadID,
			Source:         wire.Source,
			RootSessionID:  wire.RootSessionID,
			CopyBoundary:   wire.CopyBoundary,
		}
		if len(bytes.TrimSpace(wire.HistoryBase)) > 0 && !bytes.Equal(bytes.TrimSpace(wire.HistoryBase), []byte("null")) {
			var base struct {
				ThreadID string `json:"thread_id"`
			}
			if err := json.Unmarshal(wire.HistoryBase, &base); err == nil {
				meta.HistoryBaseThreadID = base.ThreadID
			} else {
				meta.HistoryMalformed = true
			}
		}
		meta.History, meta.HistoryMalformed = decodeCodexNativeHistory(wire.History, meta.HistoryMalformed)
		return meta, true
	}
	return codexNativeSessionMeta{}, false
}

// decodeCodexNativeHistory decodes the optional native history envelope.
// Absent or null history is not malformed. A present envelope must be an
// array of objects with a thread_id, ordinal bounds and optional byte
// bounds; anything else marks the envelope malformed without inventing a
// dependency.
func decodeCodexNativeHistory(raw json.RawMessage, malformed bool) ([]codexNativeHistoryEntry, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, malformed
	}
	var entries []struct {
		ThreadID                string          `json:"thread_id"`
		Mode                    json.RawMessage `json:"mode"`
		Kind                    string          `json:"kind"`
		Completed               bool            `json:"completed"`
		Start                   *int64          `json:"start"`
		EndExclusive            *int64          `json:"end_exclusive"`
		DecodedByteStart        *int64          `json:"decoded_byte_start"`
		DecodedByteEndExclusive *int64          `json:"decoded_byte_end_exclusive"`
	}
	if err := json.Unmarshal(trimmed, &entries); err != nil {
		return nil, true
	}
	var history []codexNativeHistoryEntry
	for _, entry := range entries {
		if entry.ThreadID == "" || entry.Start == nil || entry.EndExclusive == nil {
			return nil, true
		}
		if *entry.Start < 0 || *entry.EndExclusive <= *entry.Start {
			return nil, true
		}
		if (entry.DecodedByteStart == nil) != (entry.DecodedByteEndExclusive == nil) {
			return nil, true
		}
		if entry.DecodedByteStart != nil && (*entry.DecodedByteStart < 0 || *entry.DecodedByteEndExclusive <= *entry.DecodedByteStart) {
			return nil, true
		}
		mode := CodexHistoryModePaginated
		modeSpecified := false
		if len(bytes.TrimSpace(entry.Mode)) > 0 && !bytes.Equal(bytes.TrimSpace(entry.Mode), []byte("null")) {
			var name string
			if err := json.Unmarshal(entry.Mode, &name); err != nil {
				return nil, true
			}
			mode = CodexHistoryMode(name)
			if !mode.IsValid() {
				return nil, true
			}
			modeSpecified = true
		}
		kind := entry.Kind
		if kind == "" {
			kind = "before"
		}
		if kind != "before" && kind != "through" {
			return nil, true
		}
		history = append(history, codexNativeHistoryEntry{
			ThreadID:                entry.ThreadID,
			Mode:                    mode,
			ModeSpecified:           modeSpecified,
			Kind:                    kind,
			Completed:               entry.Completed,
			Start:                   entry.Start,
			EndExclusive:            entry.EndExclusive,
			DecodedByteStart:        entry.DecodedByteStart,
			DecodedByteEndExclusive: entry.DecodedByteEndExclusive,
		})
	}
	return history, malformed
}

// metaHistoryModeRaw returns the raw history_mode of a parsed native
// session_meta, or nil when the header could not be parsed.
func metaHistoryModeRaw(meta codexNativeSessionMeta, hasMeta bool) json.RawMessage {
	if !hasMeta {
		return nil
	}
	return meta.HistoryMode
}
