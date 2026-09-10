package ingest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"github.com/peasant-labs/schema"
)

type artifactIntentFile struct {
	Path          string  `json:"path"`
	PreviousHash  *string `json:"previousHash"`
	CandidateHash *string `json:"candidateHash"`
}

type artifactIntent struct {
	CWDProvenance         CWDProvenanceKind     `json:"cwdProvenance,omitempty"`
	SourceFingerprint     []byte                `json:"sourceFingerprint,omitempty"`
	CommitCaptureComplete bool                  `json:"commitCaptureComplete,omitempty"`
	Version               int                   `json:"version"`
	Phase                 artifactIntentPhase   `json:"phase"`
	SessionID             SessionID             `json:"sessionId"`
	Harness               Harness               `json:"harness"`
	Directory             string                `json:"directory"`
	PreviousDirectory     string                `json:"previousDirectory,omitempty"`
	ArtifactHash          string                `json:"artifactHash"`
	MetadataIdentity      string                `json:"metadataIdentity"`
	NeedsDatabase         bool                  `json:"needsDatabase"`
	EventSeq              *int64                `json:"eventSeq,omitempty"`
	Origin                *sessionorigin.Origin `json:"origin,omitempty"`
	Files                 []artifactIntentFile  `json:"files"`
}

type artifactIntentPhase string

const (
	artifactPrepared      artifactIntentPhase = "prepared"
	artifactFilesChanging artifactIntentPhase = "files-changing"
	artifactRollingBack   artifactIntentPhase = "rolling-back"
)

func intentMetadataPath(intent *artifactIntent) string {
	return filepath.Join(intent.Directory, string(intent.SessionID)+defaults.MetadataSuffix)
}
func intentFilePath(key, group string, index int) string {
	return filepath.Join(artifactTransactionPath(key), group, fmt.Sprintf("%04d", index))
}

func validArtifactHash(hash string) bool {
	decoded, err := hex.DecodeString(hash)
	return err == nil && len(decoded) == 32 && strings.ToLower(hash) == hash
}

func artifactOwnedName(path, directory string, sid SessionID) bool {
	relative, err := filepath.Rel(directory, path)
	if err != nil || !filepath.IsLocal(relative) {
		return false
	}
	if relative == string(sid)+defaults.MetadataSuffix || relative == string(sid)+"--transcript.json" || relative == string(sid)+"--transcript.jsonl" || relative == fileCaptureEvidenceName(sid) {
		return true
	}
	return filepath.Dir(relative) == defaults.DirDebug.String() && validArtifactDebugName(filepath.Base(relative))
}

func validateArtifactIntent(intent *artifactIntent, key string) error {
	if intent.Version != artifactTransactionVersion {
		return fmt.Errorf("recover artifact intent: unsupported intent version %d; no files were changed; use the Peasant build that wrote this pending publication", intent.Version)
	}
	if intent.Phase != artifactPrepared && intent.Phase != artifactFilesChanging && intent.Phase != artifactRollingBack {
		return fmt.Errorf("recover artifact intent: unsupported phase %q; no files were changed", intent.Phase)
	}
	if _, err := NewSessionID(string(intent.SessionID)); err != nil {
		return err
	}
	if artifactKey(intent.SessionID) != key || !validArtifactHash(key) {
		return fmt.Errorf("recover artifact intent: session does not match its ownership key; no files were changed; restore the original intent")
	}
	if !validArtifactDirectory(intent.Directory, intent.SessionID) || intent.PreviousDirectory != "" && !validArtifactDirectory(intent.PreviousDirectory, intent.SessionID) {
		return fmt.Errorf("recover artifact intent for %s: invalid owned directory; no paths were changed", intent.SessionID)
	}
	if !validArtifactHash(intent.ArtifactHash) || !validArtifactHash(intent.MetadataIdentity) || len(intent.Files) == 0 || len(intent.Files) > 1024 {
		return fmt.Errorf("recover artifact intent for %s: invalid hashes or owned-file inventory; no paths were changed; restore a complete publication intent", intent.SessionID)
	}
	if _, ok := HarvesterVersionRegistry[intent.Harness]; !ok {
		return fmt.Errorf("recover artifact intent for %s: unsupported harness %q; use a compatible Peasant build", intent.SessionID, intent.Harness)
	}
	if intent.EventSeq != nil && (*intent.EventSeq < 0 || intent.Harness != HarnessOpenCode) {
		return fmt.Errorf("recover artifact intent for %s: invalid acquired event sequence", intent.SessionID)
	}
	if intent.Origin != nil {
		if err := intent.Origin.Validate(); err != nil {
			return err
		}
	}
	seen := make(map[string]bool)
	metadataFound := false
	for _, file := range intent.Files {
		owned := artifactOwnedName(file.Path, intent.Directory, intent.SessionID)
		if intent.PreviousDirectory != "" {
			owned = owned || artifactOwnedName(file.Path, intent.PreviousDirectory, intent.SessionID)
		}
		if !owned || seen[file.Path] || file.PreviousHash == nil && file.CandidateHash == nil {
			return fmt.Errorf("recover artifact intent for %s: invalid or duplicate owned path %q; no files were changed", intent.SessionID, file.Path)
		}
		if file.PreviousHash != nil && !validArtifactHash(*file.PreviousHash) || file.CandidateHash != nil && !validArtifactHash(*file.CandidateHash) {
			return fmt.Errorf("recover artifact intent for %s: invalid file checksum", intent.SessionID)
		}
		seen[file.Path] = true
		if file.Path == intentMetadataPath(intent) && file.CandidateHash != nil {
			metadataFound = true
		}
	}
	if !metadataFound {
		return fmt.Errorf("recover artifact intent for %s: candidate metadata commit point is missing", intent.SessionID)
	}
	return nil
}

func readArtifactIntent(root ArtifactRoot, key string) (*artifactIntent, error) {
	path := filepath.Join(artifactTransactionPath(key), "intent.json")
	return readArtifactIntentAt(root, path, key)
}

func readArtifactIntentAt(root ArtifactRoot, path, key string) (*artifactIntent, error) {
	info, err := root.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxArtifactIntentBytes {
		return nil, fmt.Errorf("read artifact intent %q: not a bounded regular file; no recovery was attempted", path)
	}
	data, err := root.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var intent artifactIntent
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&intent); err != nil {
		return nil, fmt.Errorf("read artifact intent %q: %w; no recovery was attempted; restore a complete valid intent", path, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("read artifact intent %q: trailing JSON data; no recovery was attempted", path)
	}
	if err := validateArtifactIntent(&intent, key); err != nil {
		return nil, err
	}
	return &intent, nil
}

// Publish commits complete metadata last, after every candidate and backup has
// been synced. Database reconciliation is separate so parent ordering can be
// preserved without holding a parent lock while processing children.
func (p *ArtifactPublisher) Publish(ctx context.Context, request ArtifactPublication) (*ManagedArtifact, error) {
	if request.Observation == nil {
		return nil, fmt.Errorf("publish managed artifact: missing pre-extraction observation; no files were changed; observe current owned files before preparing a candidate")
	}
	candidate, err := normalizedArtifactCandidate(request.Artifact)
	if err != nil {
		return nil, err
	}
	observation := request.Observation
	if candidate.Metadata.SessionID != observation.sessionID || candidate.Metadata.ModelHarness != observation.harness {
		return nil, fmt.Errorf("publish managed artifact: candidate identity differs from observed session; no files were changed")
	}
	if version := candidate.Metadata.AdapterVersion; version != nil && *version > p.versions[observation.harness].AdapterVersion {
		return nil, &AdapterVersionError{Path: string(observation.sessionID), Version: *version, Target: p.versions[observation.harness].AdapterVersion}
	}
	key := artifactKey(observation.sessionID)
	root, lock, err := p.lockedRoot(ctx, key, ArtifactLockWrite, true)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	defer lock.Close()
	if err := refusePendingArtifact(root, key); err != nil {
		return nil, err
	}
	current, err := observeArtifactFiles(root, observation.directory, observation.sessionID, observation.debugNames)
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(current, observation.files) {
		return nil, fmt.Errorf("publish session %s: owned files changed after extraction began; newer state was preserved; retry harvest from a fresh observation", observation.sessionID)
	}
	if observation.directory == "" {
		found, err := findArtifactDirectory(root, observation.sessionID)
		if err != nil {
			return nil, err
		}
		if found != "" {
			return nil, fmt.Errorf("publish session %s: another publisher committed an artifact after the absent observation; existing files were preserved; retry harvest", observation.sessionID)
		}
	}
	intent, payloads, err := p.buildArtifactIntent(root, request, candidate)
	if err != nil {
		return nil, err
	}
	if err := stageArtifactIntent(root, intent, payloads); err != nil {
		return nil, fmt.Errorf("stage managed session %s before file commit: %w; active files were not replaced; retry recovery before harvesting again", observation.sessionID, err)
	}
	// Detach old target metadata while exclusive ownership is held. This makes
	// final metadata presence a real commit point even for byte-identical
	// metadata/transcript with changed debug files.
	if err := root.Remove(intentMetadataPath(intent)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, errors.Join(err, rollbackArtifactIntent(root, key, intent))
	}
	if err := syncArtifactParents(root, intent.Directory); err != nil {
		return nil, errors.Join(err, rollbackArtifactIntent(root, key, intent))
	}
	if err := updateArtifactIntentPhase(root, key, intent, artifactFilesChanging); err != nil {
		return nil, errors.Join(err, rollbackArtifactIntent(root, key, intent))
	}
	metadataAttempted := false
	for index, file := range intent.Files {
		if file.Path == intentMetadataPath(intent) {
			continue
		}
		if err := applyArtifactIntentFile(root, key, index, file); err != nil {
			return nil, errors.Join(p.artifactInstallError(observation.sessionID, file.Path, err), rollbackArtifactIntent(root, key, intent))
		}
	}
	for index, file := range intent.Files {
		if file.Path != intentMetadataPath(intent) {
			continue
		}
		metadataAttempted = true
		if err := applyArtifactIntentFile(root, key, index, file); err != nil {
			return nil, fmt.Errorf("commit managed metadata for session %s: %w; commit status is uncertain, so recovery state was retained without rolling files backward; retry harvest", observation.sessionID, err)
		}
	}
	if !metadataAttempted {
		return nil, fmt.Errorf("publish session %s: metadata commit point was not reached; recovery state was retained", observation.sessionID)
	}
	committed, err := readArtifactPair(root, intentMetadataPath(intent), intent.SessionID)
	if err != nil {
		return nil, fmt.Errorf("verify committed managed session %s: %w; recovery state was retained", observation.sessionID, err)
	}
	if committed.ArtifactHash != intent.ArtifactHash {
		return nil, fmt.Errorf("verify committed managed session %s: candidate identity changed; recovery state was retained", observation.sessionID)
	}
	return committed, nil
}

func (p *ArtifactPublisher) buildArtifactIntent(root ArtifactRoot, request ArtifactPublication, candidate *ManagedArtifact) (*artifactIntent, map[string][]byte, error) {
	meta := candidate.Metadata
	parent := ""
	if meta.ParentUUID != nil {
		parent = string(*meta.ParentUUID)
	}
	directory := SessionDir("", string(meta.HostSlug), string(meta.SessionID), parent)
	metadataPath := filepath.Join(directory, string(meta.SessionID)+defaults.MetadataSuffix)
	payloads := map[string][]byte{metadataPath: candidate.MetadataJSON, filepath.Join(directory, string(meta.SessionID)+"--transcript."+string(meta.Source.Format)): candidate.Transcript}
	for name, data := range request.DebugFiles {
		if !validArtifactDebugName(name) || !slices.Contains(request.Observation.debugNames, name) {
			return nil, nil, fmt.Errorf("publish session %s: debug output %q was not observed before extraction", meta.SessionID, name)
		}
		payloads[filepath.Join(directory, defaults.DirDebug.String(), name)] = data
	}
	previous := make(map[string]*string)
	files := make(map[string]artifactIntentFile)
	for _, old := range request.Observation.files {
		previous[old.Path] = old.Hash
		if old.Hash != nil && filepath.Dir(old.Path) == request.Observation.directory {
			files[old.Path] = artifactIntentFile{Path: old.Path, PreviousHash: old.Hash}
		}
	}
	for path, data := range payloads {
		old, observed := previous[path]
		if !observed {
			if _, err := root.Lstat(path); err == nil {
				return nil, nil, fmt.Errorf("publish session %s: destination %q appeared outside the observed owned set; no files were changed", meta.SessionID, path)
			} else if !errors.Is(err, fs.ErrNotExist) {
				return nil, nil, err
			}
		}
		hash := schema.ComputeTranscriptHash(data)
		files[path] = artifactIntentFile{Path: path, PreviousHash: old, CandidateHash: &hash}
	}
	// The file-only freshness marker is owned private evidence, staged in the
	// same intent as the metadata it describes and bound to those exact bytes:
	// it commits before metadata-last and rolls back with it. Without new
	// evidence any existing marker is removed, so a stale marker can never
	// vouch for a newer artifact; an absent marker reads as unknown.
	for _, markerPath := range []string{fileCaptureEvidencePath(metadataPath), fileCaptureEvidencePath(filepath.Join(request.Observation.directory, string(meta.SessionID)+defaults.MetadataSuffix))} {
		if request.Observation.directory == "" && markerPath != fileCaptureEvidencePath(metadataPath) {
			continue
		}
		if _, seen := files[markerPath]; seen {
			continue
		}
		previousMarker, err := currentArtifactFileHash(root, markerPath)
		if err != nil {
			return nil, nil, err
		}
		if previousMarker != nil {
			previous[markerPath] = previousMarker
			files[markerPath] = artifactIntentFile{Path: markerPath, PreviousHash: previousMarker}
		}
	}
	if len(request.SourceEvidence) > 0 {
		markerPath := fileCaptureEvidencePath(metadataPath)
		digest := sha256.Sum256(candidate.MetadataJSON)
		marker := append(append([]byte(nil), request.SourceEvidence...), digest[:]...)
		hash := schema.ComputeTranscriptHash(marker)
		payloads[markerPath] = marker
		files[markerPath] = artifactIntentFile{Path: markerPath, PreviousHash: previous[markerPath], CandidateHash: &hash}
	}
	identity, err := artifactMetadataIdentity(candidate.MetadataJSON)
	if err != nil {
		return nil, nil, err
	}
	intent := &artifactIntent{Version: artifactTransactionVersion, Phase: artifactPrepared, SessionID: meta.SessionID, Harness: meta.ModelHarness, Directory: directory, PreviousDirectory: request.Observation.directory, ArtifactHash: candidate.ArtifactHash, MetadataIdentity: identity, NeedsDatabase: p.mirror != nil}
	if p.mirror != nil {
		intent.EventSeq, intent.Origin = request.EventSeq, request.Origin
		intent.CWDProvenance = request.CWDProvenance
		intent.SourceFingerprint = request.SourceFingerprint
		intent.CommitCaptureComplete = request.CommitCaptureComplete
	}
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	for _, path := range paths {
		intent.Files = append(intent.Files, files[path])
	}
	if err := validateArtifactIntent(intent, artifactKey(meta.SessionID)); err != nil {
		return nil, nil, err
	}
	return intent, payloads, nil
}

func durableArtifactWrite(root ArtifactRoot, path string, data []byte) error {
	if err := root.CreateFile(path, data, defaults.PrivateFilePerm); err != nil {
		return err
	}
	return root.SyncFile(path)
}

func syncArtifactParents(root ArtifactRoot, directory string) error {
	if err := root.MkdirAll(directory, defaults.PrivateDirPerm); err != nil {
		return err
	}
	parent := "."
	for _, part := range strings.Split(directory, string(filepath.Separator)) {
		if err := root.SyncDir(parent); err != nil {
			return err
		}
		parent = filepath.Join(parent, part)
	}
	return root.SyncDir(directory)
}

func stageArtifactIntent(root ArtifactRoot, intent *artifactIntent, payloads map[string][]byte) error {
	key := artifactKey(intent.SessionID)
	parent := filepath.Dir(artifactTransactionPath(key))
	if err := syncArtifactParents(root, parent); err != nil {
		return err
	}
	nonce, err := randomHex(defaults.TempSuffixLen)
	if err != nil {
		return err
	}
	journal := filepath.Join(parent, ".prepare-"+key+"-"+nonce)
	if err := root.Mkdir(journal, defaults.PrivateDirPerm); err != nil {
		return err
	}
	if err := syncArtifactParents(root, filepath.Join(journal, "old")); err != nil {
		return err
	}
	if err := syncArtifactParents(root, filepath.Join(journal, "new")); err != nil {
		return err
	}
	data, err := json.Marshal(intent)
	if err != nil {
		return err
	}
	if len(data) > maxArtifactIntentBytes {
		return fmt.Errorf("artifact intent exceeds bounded recovery size")
	}
	// Persist the complete inventory before staging. A crash with incomplete
	// candidates/backups is safe: no active replacement starts until all sync.
	if err := durableArtifactWrite(root, filepath.Join(journal, "intent.json"), data); err != nil {
		return err
	}
	if err := root.SyncDir(journal); err != nil {
		return err
	}
	for index, file := range intent.Files {
		if file.PreviousHash != nil {
			old, err := root.ReadFile(file.Path)
			if err != nil {
				return err
			}
			if schema.ComputeTranscriptHash(old) != *file.PreviousHash {
				return fmt.Errorf("owned file %q changed before backup", file.Path)
			}
			if err := durableArtifactWrite(root, filepath.Join(journal, "old", fmt.Sprintf("%04d", index)), old); err != nil {
				return err
			}
		}
		if file.CandidateHash != nil {
			data := payloads[file.Path]
			if schema.ComputeTranscriptHash(data) != *file.CandidateHash {
				return fmt.Errorf("candidate %q changed before staging", file.Path)
			}
			if err := durableArtifactWrite(root, filepath.Join(journal, "new", fmt.Sprintf("%04d", index)), data); err != nil {
				return err
			}
		}
	}
	if err := root.SyncDir(filepath.Join(journal, "old")); err != nil {
		return err
	}
	if err := root.SyncDir(filepath.Join(journal, "new")); err != nil {
		return err
	}
	if err := root.Rename(journal, artifactTransactionPath(key)); err != nil {
		return err
	}
	return root.SyncDir(parent)
}

func updateArtifactIntentPhase(root ArtifactRoot, key string, intent *artifactIntent, phase artifactIntentPhase) error {
	updated := *intent
	updated.Phase = phase
	data, err := json.Marshal(&updated)
	if err != nil {
		return err
	}
	path := filepath.Join(artifactTransactionPath(key), "intent.next")
	if err := removeArtifactTemporary(root, path); err != nil {
		return err
	}
	if err := durableArtifactWrite(root, path, data); err != nil {
		return err
	}
	if err := root.Rename(path, filepath.Join(artifactTransactionPath(key), "intent.json")); err != nil {
		return err
	}
	if err := root.SyncDir(artifactTransactionPath(key)); err != nil {
		return err
	}
	*intent = updated
	return nil
}

// artifactInstallError describes a failed install of one owned file the way the
// user needs it: which file, in which session, why, what state the managed
// output is in now, and what to do next. The underlying error is a bare
// filesystem failure such as "no space left on device", which on its own names
// neither the artifact it was installing nor a way forward, so a user reading
// the harvest summary cannot tell which session to look at.
func (p *ArtifactPublisher) artifactInstallError(sid SessionID, path string, cause error) error {
	absolute := filepath.Join(p.output, path)
	return fmt.Errorf("install owned file %s of session %s: %w; the publication was rolled back, so the previous artifact and its index were preserved and no partial file was left behind; restore write access and free space under %s, then rerun ingest for this session",
		absolute, sid, cause, filepath.Dir(absolute))
}

func applyArtifactIntentFile(root ArtifactRoot, key string, index int, file artifactIntentFile) error {
	if err := syncArtifactParents(root, filepath.Dir(file.Path)); err != nil {
		return err
	}
	if file.CandidateHash == nil {
		if err := root.Remove(file.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	} else if err := root.Rename(intentFilePath(key, "new", index), file.Path); err != nil {
		return err
	}
	return root.SyncDir(filepath.Dir(file.Path))
}
