package ingest

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/peasant-labs/schema"
)

// A fallback never creates metadata. It can reuse a persisted capture only
// after proving the input, and the entry writer still checks its revision in
// the write transaction. Keep the verified bytes, not a path to reread later.
func (p *Pipeline) prepareReindexFallback(ctx context.Context, target reindexTarget) indexedMeta {
	im := indexedMeta{session: target.session, startMs: target.startMs, outputTranscriptPath: target.transcriptPath}
	data, err := p.fs.ReadFile(target.transcriptPath)
	if err != nil {
		slog.Warn("reindex: cannot read managed capture", "session_id", target.session.SessionID, "error", err,
			"impact", "no publication proof will be assigned", "fix", "restore the retained source and run peasant ingest")
		// Do not retry a failed read later against a potentially different file.
		im.transcriptData = []byte{}
		return im
	}
	im.transcriptData = data
	reader, ok := p.store.(PublicationInputReader)
	if !ok {
		return im
	}
	bundle, err := reader.LoadPublicationInput(ctx, target.session.SessionID)
	if err != nil || bundle.Readiness != PublicationReady || bundle.Metadata.ModelHarness != target.session.Harness || bundle.Metadata.Source.Format != target.session.SourceFormat || bundle.Metadata.ContentHash != schema.ComputeTranscriptHash(data) {
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
	im.captureRevision = bundle.CaptureRevision
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

// Normalize historical locators before constructing any output paths or hashes.
// The store verifies the corresponding opaque-host relation transactionally.
func normalizePublicationAttribution(meta *UnifiedMetadata, loc SessionLocation) error {
	// Old external store doubles may only implement the original locator fields.
	if loc.ProjectHash != "" {
		meta.Project.Hash = loc.ProjectHash
		meta.Git.Remote = loc.GitRemote
	}
	if loc.HostSlug != "" {
		slug, err := NewHostSlug(loc.HostSlug)
		if err != nil {
			return fmt.Errorf("normalize publication capture for %s: stored host locator is invalid: %w; no capture was written; repair the stored locator before running peasant ingest", meta.SessionID, err)
		}
		meta.HostSlug = slug
	}
	meta.ParentUUID = nil
	if loc.ParentID != "" {
		parent, err := NewSessionID(loc.ParentID)
		if err != nil {
			return fmt.Errorf("normalize publication capture for %s: stored parent identity is invalid: %w; no capture was written; repair the stored parent before running peasant ingest", meta.SessionID, err)
		}
		meta.ParentUUID = &parent
	}
	return nil
}
