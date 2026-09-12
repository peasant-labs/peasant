package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"github.com/peasant-labs/schema"
)

// managedArtifactStateDir is the private directory an earlier build kept its
// file transactions and lock files in. The write path no longer creates it;
// the one-time upgrade pass resolves what an earlier build left there and
// the inventory walk skips it.
const managedArtifactStateDir = ".peasant-state"

// ManagedArtifact is a captured metadata/transcript pair. Its hash identifies
// retained semantic input, not an indexer's input or proof of computation.
//
// The database is the source of truth for everything derived from the pair;
// the pair itself is the retained input the parsers read. A pair read back
// from disk is validated by NewManagedArtifact, which hashes it once. A pair
// the worker just produced is built by newIngestArtifact from the bytes it is
// about to write, without a second hash and without a copy.
type ManagedArtifact struct {
	Metadata     UnifiedMetadata
	MetadataJSON []byte
	Transcript   []byte
	ArtifactHash string
}

// NewManagedArtifact validates a pair read from disk without changing either
// file. Missing historical checksums remain missing; computing current
// identity does not invent a historical parser run or a verified checksum.
func NewManagedArtifact(metadataJSON, transcript []byte) (*ManagedArtifact, error) {
	meta, err := decodeManagedMetadata(metadataJSON, "captured artifact")
	if err != nil {
		return nil, fmt.Errorf("capture managed artifact before use: %w", err)
	}
	if err := checkManagedIdentity(meta); err != nil {
		return nil, err
	}
	contentHash := schema.ComputeTranscriptHash(transcript)
	if meta.ContentHash != "" && meta.ContentHash != contentHash {
		return nil, fmt.Errorf("capture managed artifact for session %s: transcript checksum does not match committed metadata; the pair cannot be used safely; run harvest to recover or restore matching retained files", meta.SessionID)
	}
	if meta.MetadataHash != "" && meta.MetadataHash != schema.ComputeMetadataHash(meta) {
		return nil, fmt.Errorf("capture managed artifact for session %s: metadata checksum is inconsistent; no producer evidence was changed; restore valid committed metadata before retrying", meta.SessionID)
	}
	semantic, err := artifactSemanticJSON(metadataJSON, contentHash)
	if err != nil {
		return nil, err
	}
	return &ManagedArtifact{Metadata: *meta, MetadataJSON: bytes.Clone(metadataJSON), Transcript: bytes.Clone(transcript), ArtifactHash: schema.ComputeTranscriptHash(semantic)}, nil
}

// newIngestArtifact builds the artifact for a pair the worker is about to
// write. The worker already hashed the transcript into meta.ContentHash and
// encoded meta as metadataJSON, so nothing is hashed or copied again: the
// artifact identity is computed from the metadata alone, and the transcript
// slice is the one the worker writes and then hands to the staging arena.
func newIngestArtifact(meta *UnifiedMetadata, metadataJSON, transcript []byte) (*ManagedArtifact, error) {
	if err := checkManagedIdentity(meta); err != nil {
		return nil, err
	}
	if meta.ContentHash == "" {
		return nil, fmt.Errorf("prepare managed artifact for session %s: the worker did not record the transcript checksum; nothing was written; compute the content hash before building the artifact", meta.SessionID)
	}
	// The stored metadata is the encoded bytes, and the row is written from the
	// decoded form of those exact bytes: decoding here keeps the two consistent
	// through a JSON round-trip (an empty slice, an absent optional), so the
	// pre-persistence check sees the same metadata the store will read back.
	decoded, err := decodeManagedMetadata(metadataJSON, "captured artifact")
	if err != nil {
		return nil, fmt.Errorf("prepare managed artifact for session %s: %w", meta.SessionID, err)
	}
	semantic, err := artifactSemanticJSON(metadataJSON, meta.ContentHash)
	if err != nil {
		return nil, err
	}
	return &ManagedArtifact{Metadata: *decoded, MetadataJSON: metadataJSON, Transcript: transcript, ArtifactHash: schema.ComputeTranscriptHash(semantic)}, nil
}

func checkManagedIdentity(meta *UnifiedMetadata) error {
	if meta.SchemaVersion < 1 {
		return fmt.Errorf("capture managed artifact for session %s: missing positive schema version; no files or database rows were changed; restore valid managed metadata before retrying", meta.SessionID)
	}
	if _, err := NewSessionID(string(meta.SessionID)); err != nil {
		return err
	}
	if meta.ParentUUID != nil {
		if _, err := NewSessionID(string(*meta.ParentUUID)); err != nil {
			return err
		}
		if *meta.ParentUUID == meta.SessionID {
			return fmt.Errorf("capture managed artifact for session %s: session cannot be its own parent; restore the recorded parent before retrying", meta.SessionID)
		}
	}
	if _, err := NewHostSlug(string(meta.HostSlug)); err != nil {
		return err
	}
	if _, ok := HarvesterVersionRegistry[meta.ModelHarness]; !ok {
		return fmt.Errorf("capture managed artifact for session %s: harness %q has no supported reader; preserve the files and use a compatible Peasant build", meta.SessionID, meta.ModelHarness)
	}
	if meta.Source.Format != SourceFormatJSON && meta.Source.Format != SourceFormatJSONL {
		return fmt.Errorf("capture managed artifact for session %s: unsupported transcript format %q; no files were changed; restore its JSON or JSONL metadata or use a compatible build", meta.SessionID, meta.Source.Format)
	}
	return nil
}

// Validate refuses a mirror request whose metadata bytes and decoded metadata
// disagree, or whose stated identity is not a digest. It reads the metadata
// only: the transcript is never hashed again on the way into the database,
// because the worker hashed it once and the pair on disk is validated by the
// reader that opens it.
func (a *ManagedArtifact) Validate() error {
	if a == nil {
		return fmt.Errorf("validate managed artifact before persistence: no captured pair was supplied; capture matching committed metadata and transcript before retrying")
	}
	if !validArtifactHash(a.ArtifactHash) {
		return fmt.Errorf("validate managed artifact for session %s before persistence: artifact identity %q is not a digest; no database rows were changed; capture the pair again", a.Metadata.SessionID, a.ArtifactHash)
	}
	decoded, err := decodeManagedMetadata(a.MetadataJSON, "captured artifact")
	if err != nil {
		return fmt.Errorf("validate managed artifact for session %s before persistence: %w; no database rows were changed", a.Metadata.SessionID, err)
	}
	if !reflect.DeepEqual(*decoded, a.Metadata) {
		return fmt.Errorf("validate managed artifact for session %s before persistence: captured metadata was changed after it was encoded; no database rows were changed; capture the pair again", a.Metadata.SessionID)
	}
	return nil
}

// MetricSeed distinguishes absent historical statistics from an explicit
// retained zero. Callers validate the captured pair before using its seed.
func (a *ManagedArtifact) MetricSeed() *StatsInfo {
	var fields map[string]json.RawMessage
	if a == nil || json.Unmarshal(a.MetadataJSON, &fields) != nil {
		return nil
	}
	raw, present := fields["stats"]
	if !present || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	stats := a.Metadata.Stats
	return &stats
}

func validArtifactHash(hash string) bool {
	if len(hash) != 64 {
		return false
	}
	for _, character := range hash {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func artifactSemanticJSON(data []byte, contentHash string) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	delete(fields, "derivedAt")
	delete(fields, "metadataHash")
	fields["contentHash"], _ = json.Marshal(contentHash)
	var timestamps map[string]json.RawMessage
	if err := json.Unmarshal(fields["timestamp"], &timestamps); err != nil {
		return nil, fmt.Errorf("capture managed artifact semantic timestamp: %w; restore valid metadata before retrying", err)
	}
	delete(timestamps, "ingested")
	fields["timestamp"], _ = json.Marshal(timestamps)
	if raw := fields["redaction"]; len(raw) != 0 {
		var redaction map[string]json.RawMessage
		if err := json.Unmarshal(raw, &redaction); err != nil {
			return nil, err
		}
		delete(redaction, "redacted_at_ms")
		fields["redaction"], _ = json.Marshal(redaction)
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	// RawMessage preserves nested source key order. Normalize every object,
	// while keeping integer precision and array order, before computing identity.
	var normalized any
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if err := decoder.Decode(&normalized); err != nil {
		return nil, err
	}
	return json.Marshal(normalized)
}

// ArtifactMirrorRequest carries only native evidence actually acquired for this
// artifact. Nil cursor/origin preserves the stored value; zero is valid evidence.
type ArtifactMirrorRequest struct {
	CWDProvenance         CWDProvenanceKind
	SourceFingerprint     []byte
	CommitCaptureComplete bool
	Artifact              *ManagedArtifact
	EventSeq              *int64
	Origin                *sessionorigin.Origin
}

// ArtifactMirrorResult reports committed success for one session, including
// explicit refusal when its parent or compatibility evidence is unavailable.
type ArtifactMirrorResult struct {
	SessionID SessionID
	Mirrored  bool
	Err       error
}

// ArtifactMirrorStore records a written pair, its seeds, commit bindings and
// acquired native evidence in one transaction per page. That transaction is
// the durability point of the write path.
type ArtifactMirrorStore interface {
	MirrorArtifacts(context.Context, []ArtifactMirrorRequest) []ArtifactMirrorResult
}

// MirrorPageSize is the most sessions one MirrorArtifacts call records. The
// drain hands its results to the store in pages of this size, so one commit
// covers many sessions and no page grows past what the store accepts.
const MirrorPageSize = 256

// readArtifactPair reads and validates the retained pair whose metadata is at
// metadataPath under output. It is the only way a pair reaches a parser
// after ingest: the reader hashes the pair once and refuses a torn, mixed or
// stale one. Every path is absolute.
func readArtifactPair(filesystem FileSystem, output, metadataPath string, sid SessionID) (*ManagedArtifact, error) {
	data, err := filesystem.ReadFile(metadataPath)
	if err != nil {
		return nil, err
	}
	meta, err := decodeManagedMetadata(data, metadataPath)
	if err != nil {
		return nil, err
	}
	if meta.SessionID != sid {
		return nil, fmt.Errorf("capture session %s: metadata names different session %s; preserve the files and restore the correct locator", sid, meta.SessionID)
	}
	parent := ""
	if meta.ParentUUID != nil {
		parent = string(*meta.ParentUUID)
	}
	if filepath.Clean(filepath.Dir(metadataPath)) != filepath.Clean(SessionDir(output, string(meta.HostSlug), string(sid), parent)) {
		return nil, fmt.Errorf("capture session %s: metadata host/parent disagrees with its owned locator; no files were changed", sid)
	}
	if meta.Source.Format != SourceFormatJSON && meta.Source.Format != SourceFormatJSONL {
		return nil, fmt.Errorf("capture session %s: unsupported transcript format %q; no transcript was read", sid, meta.Source.Format)
	}
	transcript, err := filesystem.ReadFile(retainedTranscriptPath(metadataPath, sid, meta.Source.Format))
	if err != nil {
		return nil, err
	}
	return NewManagedArtifact(data, transcript)
}

// ReadManagedPair reads and validates the retained pair whose metadata is at
// metadataPath under output. It is the exported reader tests and rebuild
// callers use to open a saved session's pair, refusing a torn, mixed or stale
// one by its hash.
func ReadManagedPair(filesystem FileSystem, output, metadataPath string, sid SessionID) (*ManagedArtifact, error) {
	return readArtifactPair(filesystem, output, metadataPath, sid)
}

// retainedTranscriptPath names the transcript beside a session's metadata.
func retainedTranscriptPath(metadataPath string, sid SessionID, format SourceFormat) string {
	return filepath.Join(filepath.Dir(metadataPath), string(sid)+"--transcript."+string(format))
}

// damagedPairText is the one text every reader of a damaged pair prints. The
// database still serves the session; the saved copy is what needs repair.
func damagedPairText(sid SessionID) string {
	return fmt.Sprintf("The saved copy of session %s is damaged. Its stored transcript is still served from the database. Run `peasant harvest --force --session %s` to save it again from the source.", sid, sid)
}

// missingPairText names a session whose saved pair is missing or damaged and
// whose native source could not be reached to re-ingest it. The repair itself
// is automatic when the source exists; this is the remaining actionable report.
func missingPairText(sid SessionID, source string) string {
	if source == "" {
		return fmt.Sprintf("session %s has no usable saved copy in peasant-sync/ and no recorded original source, so it could not be re-ingested", sid)
	}
	return fmt.Sprintf("session %s has no usable saved copy in peasant-sync/ and its original source %q is unavailable, so it could not be re-ingested", sid, source)
}

// artifactOwnedKind names the families of file peasant's own artifact naming
// claims inside one session directory. An install owns every member of these
// families for its session and nothing else, so this list is the whole of what
// an install may write, replace or prune.
type artifactOwnedKind string

const (
	// artifactOwnedMetadata is the session's metadata document, the file that
	// is installed last.
	artifactOwnedMetadata artifactOwnedKind = "metadata"
	// artifactOwnedTranscript is the session's managed transcript.
	artifactOwnedTranscript artifactOwnedKind = "transcript"
	// artifactOwnedSourceCapture is the private marker recording which source
	// bytes a file-only capture read.
	artifactOwnedSourceCapture artifactOwnedKind = "source-capture"
	// artifactOwnedDebug is one of the session's debug outputs, which live in
	// their own directory beside the pair.
	artifactOwnedDebug artifactOwnedKind = "debug"
)

// artifactOwnedFile is a path inside a session directory that peasant's own
// naming claims for that session. Values are produced only by
// newArtifactOwnedFile, so a path that reached this type has been checked.
type artifactOwnedFile struct {
	// Relative is the path under the session directory, in platform form.
	Relative string
	Kind     artifactOwnedKind
}

// newArtifactOwnedFile admits a path this session's install owns and refuses
// every other member of the directory. Ownership is decided by NAME alone,
// never by what a particular run happens to write, so a file this build no
// longer produces is still recognised as the session's own and can be pruned,
// while a file a user or another tool put beside the artifact is never touched.
func newArtifactOwnedFile(path, directory string, sid SessionID) (artifactOwnedFile, error) {
	relative, err := filepath.Rel(directory, path)
	if err != nil || !filepath.IsLocal(relative) {
		return artifactOwnedFile{}, fmt.Errorf("classify managed path %q for session %s: it is not inside that session's directory %q; nothing was changed; install only paths under the session's own directory", path, sid, directory)
	}
	switch relative {
	case string(sid) + defaults.MetadataSuffix:
		return artifactOwnedFile{Relative: relative, Kind: artifactOwnedMetadata}, nil
	case string(sid) + "--transcript.json", string(sid) + "--transcript.jsonl":
		return artifactOwnedFile{Relative: relative, Kind: artifactOwnedTranscript}, nil
	case fileCaptureEvidenceName(sid):
		return artifactOwnedFile{Relative: relative, Kind: artifactOwnedSourceCapture}, nil
	}
	if filepath.Dir(relative) == defaults.DirDebug.String() && validArtifactDebugName(filepath.Base(relative)) {
		return artifactOwnedFile{Relative: relative, Kind: artifactOwnedDebug}, nil
	}
	return artifactOwnedFile{}, fmt.Errorf("classify managed path %q for session %s: its name is not one this session's artifacts use; the file was left untouched; only peasant's own artifact names are owned, everything else beside them is the user's", path, sid)
}

func artifactOwnedName(path, directory string, sid SessionID) bool {
	_, err := newArtifactOwnedFile(path, directory, sid)
	return err == nil
}

// validArtifactDebugName reports whether a name inside a session's debug
// directory is one of Peasant's OWN debug outputs.
//
// It is deliberately narrower than "any local file name". An install may
// retire a file it owns, and the debug directory is an ordinary directory a
// user or another tool can write into, so claiming every name there by
// location alone would let an install delete a file that was never Peasant's.
// The extension has to be one of the closed set Peasant's own outputs use; a
// name outside it is the user's and is left untouched.
func validArtifactDebugName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if filepath.Base(name) != name || !filepath.IsLocal(name) {
		return false
	}
	for _, suffix := range defaults.DebugArtifactSuffixes() {
		if stem, found := strings.CutSuffix(name, suffix); found && stem != "" {
			return true
		}
	}
	return false
}

// validArtifactDirectory reports whether a root-relative directory is one a
// session's pair may live in: `<host>/<sid>` or
// `<host>/<parent>/subagents/<sid>`.
func validArtifactDirectory(directory string, sid SessionID) bool {
	if !filepath.IsLocal(directory) {
		return false
	}
	parts := strings.Split(filepath.ToSlash(directory), "/")
	if len(parts) != 2 && len(parts) != 4 || parts[0] == managedArtifactStateDir {
		return false
	}
	if _, err := NewHostSlug(parts[0]); err != nil {
		return false
	}
	if parts[len(parts)-1] != string(sid) {
		return false
	}
	if len(parts) == 4 {
		if parts[2] != defaults.DirSubagents.String() {
			return false
		}
		if _, err := NewSessionID(parts[1]); err != nil {
			return false
		}
	}
	return true
}

// checkReplacementHeader refuses to replace a pair that a newer adapter than
// this build produced. It reads the header of the existing metadata only; a
// historic body this build cannot decode keeps its existing policy.
func checkReplacementHeader(data []byte, session DiscoveredSession, versions map[Harness]HarvesterVersions) error {
	header, err := decodeManagedMetadataHeader(data, string(session.SessionID))
	if header != nil && header.AdapterVersion != nil && *header.AdapterVersion > versions[session.Harness].AdapterVersion {
		return &AdapterVersionError{Path: string(session.SessionID), Version: *header.AdapterVersion, Target: versions[session.Harness].AdapterVersion}
	}
	if isMetadataCompatibilityError(err) {
		return err
	}
	return nil
}

// artifactMirrorError distinguishes a failed database mirror from a failed
// file install, so a written pair keeps its own outcome: the files are on
// disk, the row is not, and the next harvest reads the session again.
type artifactMirrorError struct{ error }

func (e *artifactMirrorError) Unwrap() error { return e.error }

var errArtifactNotMirrored = errors.New("the saved files are on disk without a database row; the next harvest reads the session again")
