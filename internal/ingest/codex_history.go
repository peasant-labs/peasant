package ingest

// Codex current-history capture and replay.
//
// This file owns the read-only Codex source authority, the bounded ordered
// segment capture, and the deterministic replay that turns captured native
// records into an adapter-private node graph for the native provenance
// classifier. It does not classify blocks, does not allocate durable
// generation rows, and never writes the native source.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

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
// the bounded local identity the registry keys on.
type CodexCapturedNode struct {
	NativeKey        string
	Ref              schema.SourceEntryRef
	SegmentOrdinal   int
	Ordinal          int64
	ByteStart        int64
	ByteEndExclusive int64
	EnvelopeType     string
	NativeType       string
	NativeRole       string
	ItemID           string
	CallID           string
	TurnID           string
	Ownership        CodexOwnership
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

// CodexReference is one ordered bounded dependency of the current source. The
// reference graph is resolved oldest-to-newest before the current pointer.
type CodexReference struct {
	LogicalSessionID        *SessionID
	Pointer                 string
	PhysicalSourceID        string
	Mode                    CodexHistoryMode
	Coordinates             indexformat.SegmentCoordinates
	Inclusion               indexformat.SegmentInclusion
	CopyBoundary            *int64
	OriginalOwnershipProven bool
}

// CodexSourceAuthority is the selected read-only authority for one Codex
// thread. HistoryMode is the raw native value: the capture resolves it so an
// omitted value and a null value stay distinguishable.
type CodexSourceAuthority struct {
	StableThreadID   string
	Kind             CodexSourceAuthorityKind
	CurrentPointer   string
	PhysicalSourceID string
	HistoryMode      json.RawMessage
	RootID           string
	ParentID         string
	ForkSourceID     string
	// CopyBoundary is the native copied-creation boundary S of the current
	// incarnation. Records before S are inherited only when native ownership is
	// proven; otherwise they are migrated/uncertain earlier history.
	CopyBoundary            *int64
	OriginalOwnershipProven bool
	// References are the ordered bounded dependencies, oldest first.
	References []CodexReference
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
// ordered segment evidence, the proven correlations and completeness. RawBytes
// is the bounded decoded prefix of the authoritative current source.
type CodexCapturedHistory struct {
	StableThreadID   string
	AuthorityKind    CodexSourceAuthorityKind
	Pointer          string
	PhysicalSourceID string
	Mode             CodexHistoryMode
	Completeness     indexformat.GenerationCompleteness
	Nodes            []CodexCapturedNode
	Segments         []indexformat.ContextSegment
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
	writeLengthPrefixed(&b, authority.StableThreadID)
	writeLengthPrefixed(&b, string(authority.Kind))
	writeLengthPrefixed(&b, authority.CurrentPointer)
	writeLengthPrefixed(&b, authority.PhysicalSourceID)
	writeLengthPrefixed(&b, strings.TrimSpace(string(authority.HistoryMode)))
	writeLengthPrefixed(&b, authority.RootID)
	writeLengthPrefixed(&b, authority.ParentID)
	writeLengthPrefixed(&b, authority.ForkSourceID)
	writeLengthPrefixed(&b, codexOptionalInt64(authority.CopyBoundary))
	writeLengthPrefixed(&b, fmt.Sprintf("%t", authority.OriginalOwnershipProven))
	for _, ref := range authority.References {
		writeLengthPrefixed(&b, ref.Pointer)
		writeLengthPrefixed(&b, ref.PhysicalSourceID)
		writeLengthPrefixed(&b, string(ref.Mode))
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
// digest, and the ordered per-segment identities and bounded byte digests.
// Parent whole-file mtime, size and digest after the child cutoff are NOT
// inputs.
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
		bounded := segment.boundedData()
		writeHashedLengthPrefixed(hash, fmt.Sprintf("%d", len(bounded)))
		digest := sha256.Sum256(bounded)
		writeHashedLengthPrefixed(hash, hex.EncodeToString(digest[:]))
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

// codexFileSource is the production read-only source. It selects the detached
// rollout file as the authority when no native pointer index exists, and reads
// the bounded decoded prefix through the FileSystem.
type codexFileSource struct {
	fs FileSystem
}

var _ CodexReadOnlySource = (*codexFileSource)(nil)

func newCodexFileSource(fs FileSystem) *codexFileSource {
	return &codexFileSource{fs: fs}
}

// ResolveCodexAuthority selects the stable session_meta.id and the one current
// detached rollout pointer. A missing file is an error; it is never replaced by
// an older rollout.
func (s *codexFileSource) ResolveCodexAuthority(ctx context.Context, session DiscoveredSession) (CodexSourceAuthority, error) {
	pointer := session.SourcePath.String()
	header, err := codexReadHeader(s.fs, pointer, codexHeaderLimit)
	if err != nil {
		return CodexSourceAuthority{}, fmt.Errorf("ingest.codexFileSource.ResolveCodexAuthority: reading the Codex current pointer %s for session %s failed: %w; the authoritative source is unavailable, no older rollout was substituted, and the last good generation is retained until the source returns", codexPhysicalSourceID(pointer), session.SessionID, err)
	}
	meta, hasMeta := parseCodexSessionMetaHeader(header)
	stableID := session.SessionID.String()
	if hasMeta && meta.ID != "" {
		stableID = meta.ID
	}
	authority := CodexSourceAuthority{
		StableThreadID:   stableID,
		Kind:             CodexAuthorityDetachedFile,
		CurrentPointer:   pointer,
		PhysicalSourceID: codexPhysicalSourceID(pointer),
		HistoryMode:      metaHistoryMode(meta, hasMeta),
	}
	if hasMeta {
		authority.ForkSourceID = meta.ForkedFromID
		authority.ParentID = meta.nestedParentThreadID()
	}
	return authority, nil
}

// ReadCodexSource reads the bounded decoded bytes at one pointer. The
// descriptor-size capture boundary is used when the FileSystem can resolve it.
func (s *codexFileSource) ReadCodexSource(_ context.Context, pointer string) ([]byte, error) {
	if reader, ok := s.fs.(sourcePrefixReader); ok {
		data, err := reader.ReadSourcePrefix(pointer)
		if err != nil {
			return nil, fmt.Errorf("ingest.codexFileSource.ReadCodexSource: bounded read of %s failed: %w; the capture is incomplete and no prior snapshot was overwritten", codexPhysicalSourceID(pointer), err)
		}
		return data, nil
	}
	data, err := s.fs.ReadFile(pointer)
	if err != nil {
		return nil, fmt.Errorf("ingest.codexFileSource.ReadCodexSource: read of %s failed: %w; the capture is incomplete and no prior snapshot was overwritten", codexPhysicalSourceID(pointer), err)
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

// codexSessionMetaRead is the header projection of a session_meta payload.
type codexSessionMetaRead struct {
	ID             string          `json:"id"`
	HistoryMode    json.RawMessage `json:"history_mode"`
	ForkedFromID   string          `json:"forked_from_id"`
	ParentThreadID string          `json:"parent_thread_id"`
	Source         json.RawMessage `json:"source"`
}

// nestedParentThreadID reads the nested thread-spawn parent when the top-level
// field is absent. Nesting alone is not a cycle proof.
func (m codexSessionMetaRead) nestedParentThreadID() string {
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

// parseCodexSessionMetaHeader finds the first session_meta record in a header
// peek. It reports false when no session_meta could be decoded.
func parseCodexSessionMetaHeader(header []byte) (codexSessionMetaRead, bool) {
	scanner := newJSONLRecordScanner(header, defaults.MaxJSONLRecordBytes)
	for scanner.Scan() {
		raw := strings.TrimSpace(string(scanner.Bytes()))
		if raw == "" {
			continue
		}
		var env codexRolloutLine
		if err := json.Unmarshal([]byte(raw), &env); err != nil {
			return codexSessionMetaRead{}, false
		}
		if env.Type != codexTypeSessionMeta {
			return codexSessionMetaRead{}, false
		}
		var meta codexSessionMetaRead
		if err := json.Unmarshal(env.Payload, &meta); err != nil {
			return codexSessionMetaRead{}, false
		}
		return meta, true
	}
	return codexSessionMetaRead{}, false
}

// metaHistoryMode returns the raw history_mode of a parsed session_meta, or nil
// when the header could not be parsed.
func metaHistoryMode(meta codexSessionMetaRead, hasMeta bool) json.RawMessage {
	if !hasMeta {
		return nil
	}
	return meta.HistoryMode
}
