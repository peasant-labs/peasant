package ingest

import (
	"fmt"
	"os"
)

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
