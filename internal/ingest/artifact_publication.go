package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"github.com/peasant-labs/schema"
)

const managedArtifactStateDir = ".peasant-state"
const artifactTransactionVersion = 1
const maxArtifactIntentBytes = 1 << 20

// ArtifactPublisherOptions supplies the parser targets and bookkeeping clock.
// Defaults use this build's registry and time.Now; construction performs no I/O.
type ArtifactPublisherOptions struct {
	Versions map[Harness]HarvesterVersions
	Now      func() time.Time
	Mirror   ArtifactMirrorStore
}

// ArtifactPublisher replaces only files owned by one session. Its private
// intent is recovery state, not a permanent receipt or a public wire contract.
type ArtifactPublisher struct {
	fs       DurableFileSystem
	output   string
	versions map[Harness]HarvesterVersions
	now      func() time.Time
	mirror   ArtifactMirrorStore
}

func NewArtifactPublisher(filesystem FileSystem, output string, options ArtifactPublisherOptions) (*ArtifactPublisher, error) {
	durable, ok := filesystem.(DurableFileSystem)
	if !ok {
		return nil, fmt.Errorf("prepare managed artifact publication: filesystem has no root-confined durability/locking capability; no artifact was changed; use a supported durable filesystem")
	}
	output = filepath.Clean(output)
	if !filepath.IsAbs(output) || output == string(filepath.Separator) {
		return nil, fmt.Errorf("prepare managed artifact publication: output %q is not a dedicated absolute directory; no path was created; configure a managed output directory", output)
	}
	versions := options.Versions
	if versions == nil {
		versions = HarvesterVersionRegistry
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &ArtifactPublisher{fs: durable, output: output, versions: maps.Clone(versions), now: now, mirror: options.Mirror}, nil
}

type artifactFileVersion struct {
	Path string  `json:"path"`
	Hash *string `json:"hash"`
}

// ArtifactObservation is the exact owned-byte/absence state seen before source
// extraction. Legacy body corruption is observable without treating it as a
// valid readable artifact; known incompatible headers still refuse replacement.
type ArtifactObservation struct {
	sessionID  SessionID
	harness    Harness
	directory  string
	files      []artifactFileVersion
	debugNames []string
}

// ArtifactPublication combines validated candidate output and the native
// evidence actually acquired while producing it. Nil evidence stays unknown.
type ArtifactPublication struct {
	CWDProvenance         CWDProvenanceKind
	SourceFingerprint     []byte
	CommitCaptureComplete bool
	Artifact              *ManagedArtifact
	Observation           *ArtifactObservation
	DebugFiles            map[string][]byte
	EventSeq              *int64
	Origin                *sessionorigin.Origin
	// SourceEvidence is the file-only harvest freshness marker. It records what
	// the harvest actually read from the retained files, so a later run can tell
	// whether the source changed without opening a native provider database. The
	// publisher writes it beside the metadata artifact in the same intent; a
	// later change adds that write. An empty value records no marker.
	SourceEvidence []byte
}

func artifactKey(sid SessionID) string { return schema.ComputeTranscriptHash([]byte(sid)) }
func artifactLockPath(key string) string {
	return filepath.Join(managedArtifactStateDir, "locks", key+".lock")
}
func artifactTransactionPath(key string) string {
	return filepath.Join(managedArtifactStateDir, "transactions", key)
}

func (p *ArtifactPublisher) lockedRoot(ctx context.Context, key string, mode ArtifactLockMode, create bool) (ArtifactRoot, io.Closer, error) {
	var root ArtifactRoot
	var err error
	if create {
		root, err = p.fs.CreateArtifactRoot(p.output)
	} else {
		root, err = p.fs.OpenArtifactRoot(p.output)
	}
	if err != nil {
		return nil, nil, err
	}
	if create {
		if err := root.MkdirAll(filepath.Join(managedArtifactStateDir, "locks"), defaults.PrivateDirPerm); err != nil {
			_ = root.Close()
			return nil, nil, err
		}
		if err := root.SyncDir(managedArtifactStateDir); err != nil {
			_ = root.Close()
			return nil, nil, err
		}
		if err := root.SyncDir("."); err != nil {
			_ = root.Close()
			return nil, nil, err
		}
	}
	lock, err := root.Lock(ctx, artifactLockPath(key), mode, create)
	if err != nil {
		_ = root.Close()
		return nil, nil, fmt.Errorf("acquire managed artifact ownership before %s: %w; no artifact was changed; complete pending harvest/recovery or restore the managed lock directory", modeLabel(mode), err)
	}
	if create {
		if err := root.SyncDir(filepath.Join(managedArtifactStateDir, "locks")); err != nil {
			_ = lock.Close()
			_ = root.Close()
			return nil, nil, err
		}
	}
	return root, lock, nil
}

func modeLabel(mode ArtifactLockMode) string {
	if mode == ArtifactLockRead {
		return "read-only capture"
	}
	return "publication or recovery"
}

func (p *ArtifactPublisher) relativeMetadataPath(path string, sid SessionID) (string, error) {
	if filepath.IsAbs(path) {
		var err error
		path, err = filepath.Rel(p.output, path)
		if err != nil {
			return "", err
		}
	}
	path = filepath.Clean(path)
	if !filepath.IsLocal(path) || filepath.Base(path) != string(sid)+defaults.MetadataSuffix || !validArtifactDirectory(filepath.Dir(path), sid) {
		return "", fmt.Errorf("inspect managed session %s: metadata locator %q is outside its owned layout; no files were changed; restore a valid host/session locator", sid, path)
	}
	return path, nil
}

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

func (p *ArtifactPublisher) Observe(ctx context.Context, session DiscoveredSession, metadataPath string) (*ArtifactObservation, error) {
	if _, err := NewSessionID(string(session.SessionID)); err != nil {
		return nil, err
	}
	root, lock, err := p.lockedRoot(ctx, artifactKey(session.SessionID), ArtifactLockWrite, true)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	defer lock.Close()
	if err := refusePendingArtifact(root, artifactKey(session.SessionID)); err != nil {
		return nil, err
	}
	directory := ""
	if metadataPath != "" {
		path, err := p.relativeMetadataPath(metadataPath, session.SessionID)
		if err != nil {
			return nil, err
		}
		directory = filepath.Dir(path)
	} else {
		directory, err = findArtifactDirectory(root, session.SessionID)
		if err != nil {
			return nil, err
		}
	}
	var names []string
	for _, path := range session.DebugPaths {
		name := filepath.Base(string(path))
		if !validArtifactDebugName(name) || slices.Contains(names, name) {
			return nil, fmt.Errorf("observe session %s before extraction: debug filenames collide or are invalid; no output was changed; use uniquely named regular debug files", session.SessionID)
		}
		names = append(names, name)
	}
	slices.Sort(names)
	observation := &ArtifactObservation{sessionID: session.SessionID, harness: session.Harness, directory: directory, debugNames: names}
	observation.files, err = observeArtifactFiles(root, directory, session.SessionID, names)
	if err != nil {
		return nil, err
	}
	if directory != "" {
		data, err := root.ReadFile(filepath.Join(directory, string(session.SessionID)+defaults.MetadataSuffix))
		if err == nil {
			if err := p.checkReplacementHeader(data, session); err != nil {
				return nil, err
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	return observation, nil
}

func (p *ArtifactPublisher) checkReplacementHeader(data []byte, session DiscoveredSession) error {
	header, err := decodeManagedMetadataHeader(data, string(session.SessionID))
	if header != nil && header.AdapterVersion != nil && *header.AdapterVersion > p.versions[session.Harness].AdapterVersion {
		return &AdapterVersionError{Path: string(session.SessionID), Version: *header.AdapterVersion, Target: p.versions[session.Harness].AdapterVersion}
	}
	if isMetadataCompatibilityError(err) {
		return err
	}
	return nil // Historic unreadable bodies retain their existing recovery policy.
}

func refusePendingArtifact(root ArtifactRoot, key string) error {
	_, err := root.Lstat(artifactTransactionPath(key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("managed artifact has pending publication state; read-only capture and new replacement were refused; run harvest to recover/reconcile it before retrying")
}

// Capture opens existing coordination state only. It never creates a lock,
// repairs an intent, changes metadata, or opens the analytics database.
func (p *ArtifactPublisher) Capture(ctx context.Context, sid SessionID, metadataPath string) (*ManagedArtifact, error) {
	var captured *ManagedArtifact
	err := p.WithCapture(ctx, sid, metadataPath, func(artifact *ManagedArtifact) error { captured = artifact; return nil })
	return captured, err
}

// WithCapture retains read ownership while a caller captures its corresponding
// database snapshot. The callback must release database resources before return;
// redaction/network I/O belongs outside it. No recovery or lock creation occurs.
func (p *ArtifactPublisher) WithCapture(ctx context.Context, sid SessionID, metadataPath string, use func(*ManagedArtifact) error) error {
	if use == nil {
		return fmt.Errorf("capture managed artifact: missing snapshot consumer; no file was changed")
	}
	path, err := p.relativeMetadataPath(metadataPath, sid)
	if err != nil {
		return err
	}
	root, lock, err := p.lockedRoot(ctx, artifactKey(sid), ArtifactLockRead, false)
	if err != nil {
		return err
	}
	defer root.Close()
	defer lock.Close()
	if err := refusePendingArtifact(root, artifactKey(sid)); err != nil {
		return err
	}
	artifact, err := readArtifactPair(root, path, sid)
	if err != nil {
		return err
	}
	return use(artifact)
}

func readArtifactPair(root ArtifactRoot, metadataPath string, sid SessionID) (*ManagedArtifact, error) {
	data, err := root.ReadFile(metadataPath)
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
	if filepath.Dir(metadataPath) != SessionDir("", string(meta.HostSlug), string(sid), parent) {
		return nil, fmt.Errorf("capture session %s: metadata host/parent disagrees with its owned locator; no files were changed", sid)
	}
	if meta.Source.Format != SourceFormatJSON && meta.Source.Format != SourceFormatJSONL {
		return nil, fmt.Errorf("capture session %s: unsupported transcript format %q; no transcript was read", sid, meta.Source.Format)
	}
	transcriptPath := filepath.Join(filepath.Dir(metadataPath), string(sid)+"--transcript."+string(meta.Source.Format))
	transcript, err := root.ReadFile(transcriptPath)
	if err != nil {
		return nil, err
	}
	return NewManagedArtifact(data, transcript)
}

func validArtifactDebugName(name string) bool {
	return name != "" && name != "." && name != ".." && filepath.Base(name) == name && filepath.IsLocal(name)
}

func normalizedArtifactCandidate(candidate *ManagedArtifact) (*ManagedArtifact, error) {
	if err := candidate.Validate(); err != nil {
		return nil, err
	}
	meta := candidate.Metadata
	meta.DerivedAt = nil
	meta.ContentHash = schema.ComputeTranscriptHash(candidate.Transcript)
	meta.MetadataHash = schema.ComputeMetadataHash(&meta)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(candidate.MetadataJSON, &fields); err != nil {
		return nil, err
	}
	delete(fields, "derivedAt")
	fields["contentHash"], _ = json.Marshal(meta.ContentHash)
	fields["metadataHash"], _ = json.Marshal(meta.MetadataHash)
	data, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	return NewManagedArtifact(data, candidate.Transcript)
}

func artifactMetadataIdentity(data []byte) (string, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return "", err
	}
	delete(fields, "derivedAt")
	encoded, err := json.Marshal(fields)
	if err != nil {
		return "", err
	}
	var normalized any
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if err := decoder.Decode(&normalized); err != nil {
		return "", err
	}
	encoded, err = json.Marshal(normalized)
	return schema.ComputeTranscriptHash(encoded), err
}
