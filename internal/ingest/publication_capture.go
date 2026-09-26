package ingest

import (
	"context"
	"errors"
	"log/slog"
	"os"

	"github.com/peasant-labs/schema"
)

// A fallback never creates metadata. It can reuse a persisted capture only
// after proving the input, and the entry writer still checks its revision in
// the write transaction. Keep the verified bytes, not a path to reread later.
func (p *Pipeline) prepareReindexFallback(ctx context.Context, target reindexTarget) indexedMeta {
	im := indexedMeta{session: target.session, startMs: target.startMs, outputTranscriptPath: target.transcriptPath}
	// One validated read of both halves. The index must never pair bytes
	// from two different reads, and the decoded publication projection has
	// already dropped the fields the metadata document carries outside the
	// struct, so re-encoding it as the pair's metadata would be lossy.
	artifact, err := readArtifactPair(p.fs, string(p.config.OutputDir), adapterTargetMetadataPath(target), target.session.SessionID)
	if err != nil {
		slog.Warn("reindex: cannot read managed capture", "session_id", target.session.SessionID, "error", err,
			"impact", "no publication proof will be assigned", "fix", "restore the retained source and run peasant ingest")
		// Do not retry a failed read later against a potentially different file.
		im.transcriptData = []byte{}
		return im
	}
	im.transcriptData = artifact.Transcript
	im.metadataData = artifact.MetadataJSON
	reader, ok := p.store.(PublicationMetadataReader)
	if !ok {
		return im
	}
	// Proof needs metadata and its paired revision, not the old transcript
	// entries, quality metrics, or association ledger. Keep this projection
	// lightweight even when publication loads full entry bodies.
	snapshots, err := reader.LoadPublicationMetadata(ctx, []SessionID{target.session.SessionID})
	snapshot := snapshots[target.session.SessionID]
	if err != nil || snapshot.Error != nil || snapshot.Readiness != PublicationReady || snapshot.Metadata.ModelHarness != target.session.Harness || snapshot.Metadata.Source.Format != target.session.SourceFormat || snapshot.Metadata.ContentHash != schema.ComputeTranscriptHash(artifact.Transcript) {
		return im
	}
	indexer, ok := p.indexers[target.session.Harness]
	if !ok {
		return im
	}
	sourceKind := indexer.SourceKind()
	if resolver, ok := indexer.(SessionTranscriptSourceResolver); ok {
		sourceKind = resolver.TranscriptSourceKindFor(indexTargetSession(im))
	}
	if sourceKind != TranscriptSourceFile {
		// Legacy JSON captures hash the header, not the separate message/part
		// files. Equal parsed entries cannot prove metadata facts (such as a
		// model) that the index might omit. Without the original source header
		// this format has no complete byte proof; do not certify it. SQLite
		// projections are self-contained and use the verified-bytes path below.
		return im
	}
	im.captureRevision = snapshot.CaptureRevision
	return im
}

// captureFileSystem retains exactly the source reads used by extraction. The
// OpenCode JSON indexer consumes the same message/part tree, rather than a newer
// tree that happens to exist when its queued index work runs.
type captureFileSystem struct {
	FileSystem
	files map[string]captureFile
	dirs  map[string]captureDirectory
}

type captureFile struct {
	data []byte
	err  error
}

type captureDirectory struct {
	entries []os.DirEntry
	err     error
}

var _ FileSystem = (*captureFileSystem)(nil)

func newCaptureFileSystem(fs FileSystem) *captureFileSystem {
	return &captureFileSystem{FileSystem: fs, files: make(map[string]captureFile), dirs: make(map[string]captureDirectory)}
}

func (fs *captureFileSystem) release() {
	fs.files = nil
	fs.dirs = nil
}

func (fs *captureFileSystem) ReadFile(path string) ([]byte, error) {
	if file, ok := fs.files[path]; ok {
		return file.data, file.err
	}
	data, err := fs.FileSystem.ReadFile(path)
	fs.files[path] = captureFile{data: data, err: err}
	return data, err
}

func (fs *captureFileSystem) ReadDir(path string) ([]os.DirEntry, error) {
	if dir, ok := fs.dirs[path]; ok {
		return dir.entries, dir.err
	}
	entries, err := fs.FileSystem.ReadDir(path)
	fs.dirs[path] = captureDirectory{entries: entries, err: err}
	return entries, err
}

func publicationCWDProvenance(meta *UnifiedMetadata, session DiscoveredSession) CWDProvenanceKind {
	switch meta.ModelHarness {
	case HarnessCursor:
		if session.CWD != "" {
			return CWDSourceWorkspace
		}
	case HarnessStrike:
		if session.CWD != "" {
			return CWDSourceWorktree
		}
	default:
		if meta.CWD != "" {
			return CWDSourceExact
		}
		return CWDSourceAbsent
	}
	return CWDSourceAbsent
}

// ValidatePublicationCaptureSnapshot reports why a recorded metadata snapshot
// and a working-directory provenance kind do not form a publication-capture
// agreement. It is the ONE rule the store enforces when it records a capture and
// the pipeline applies before it certifies one, so a caller never supplies a
// snapshot the store must refuse. A nil return means the snapshot is a capture;
// any error names the reason it is not, and the caller must record nothing and
// leave the stored provenance exactly as it was.
func ValidatePublicationCaptureSnapshot(m *schema.UnifiedMetadata, kind CWDProvenanceKind) error {
	if _, err := NewCWDProvenanceKind(string(kind)); err != nil {
		return err
	}
	if kind == CWDNotRecovered {
		return errors.New("source has not been inspected")
	}
	if (kind == CWDSourceExact) != (m.CWD != "") {
		return errors.New("CWD and its source provenance disagree")
	}
	if m.SchemaVersion != CurrentSchemaVersion {
		return errors.New("unsupported metadata schema")
	}
	if _, err := schema.NewSessionID(string(m.SessionID)); err != nil {
		return errors.New("invalid metadata session identity")
	}
	if _, err := schema.NewProjectHash(string(m.Project.Hash)); err != nil {
		return errors.New("invalid metadata project identity")
	}
	if _, err := schema.NewTranscriptContentHash(m.ContentHash); err != nil {
		return errors.New("invalid captured content digest")
	}
	if m.MetadataHash != schema.ComputeMetadataHash(m) {
		return errors.New("metadata integrity digest does not match snapshot")
	}
	return nil
}
