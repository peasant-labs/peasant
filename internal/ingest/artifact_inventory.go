package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/peasant-labs/peasant/internal/defaults"
)

const artifactDirectoryPageSize = 64

func walkArtifactDirectory(ctx context.Context, root ArtifactRoot, path string, visit func(fs.DirEntry) error) error {
	directory, err := root.OpenDirectory(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer directory.Close()
	var failures error
	for {
		if err := ctx.Err(); err != nil {
			return errors.Join(failures, err)
		}
		page, readErr := directory.ReadDir(artifactDirectoryPageSize)
		for _, entry := range page {
			if err := ctx.Err(); err != nil {
				return errors.Join(failures, err)
			}
			failures = errors.Join(failures, visit(entry))
		}
		if errors.Is(readErr, io.EOF) {
			return failures
		}
		if readErr != nil {
			return errors.Join(failures, readErr)
		}
	}
}

// WalkMetadata yields only exact managed metadata locators, in bounded directory
// pages with each parent before its nested children. It opens no transcript or
// database and creates no coordination state. Capture/validation belongs under
// ownership in the visitor; directory enumeration alone is not a snapshot.
func (p *ArtifactPublisher) WalkMetadata(ctx context.Context, visit func(SessionID, string) error) error {
	if visit == nil {
		return fmt.Errorf("walk managed metadata: no locator consumer was supplied")
	}
	root, err := p.fs.OpenArtifactRoot(p.output)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	visitSession := func(directory string, sid SessionID) error {
		path := filepath.Join(directory, string(sid)+defaults.MetadataSuffix)
		info, err := root.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("inspect managed metadata %q: not an owned regular file; restore the expected file type before reconciliation", path)
		}
		return visit(sid, filepath.Join(p.output, path))
	}
	return walkArtifactDirectory(ctx, root, ".", func(host fs.DirEntry) error {
		if !host.IsDir() || host.Name() == managedArtifactStateDir || strings.HasPrefix(host.Name(), defaults.TempDirPrefix) {
			return nil
		}
		if _, err := NewHostSlug(host.Name()); err != nil {
			return nil
		}
		return walkArtifactDirectory(ctx, root, host.Name(), func(parent fs.DirEntry) error {
			if !parent.IsDir() {
				return nil
			}
			sid, err := NewSessionID(parent.Name())
			if err != nil {
				return nil
			}
			directory := filepath.Join(host.Name(), parent.Name())
			parentErr := visitSession(directory, sid)
			childErr := walkArtifactDirectory(ctx, root, filepath.Join(directory, defaults.DirSubagents.String()), func(child fs.DirEntry) error {
				if !child.IsDir() {
					return nil
				}
				childID, err := NewSessionID(child.Name())
				if err != nil {
					return nil
				}
				return visitSession(filepath.Join(directory, defaults.DirSubagents.String(), child.Name()), childID)
			})
			return errors.Join(parentErr, childErr)
		})
	})
}

// CountMetadata reports how many retained sessions WalkMetadata would visit.
//
// It is the inventory count the reconciliation progress bar needs before it
// visits the first session, and it is deliberately the same walk: directory
// enumeration and one Lstat per session, no transcript, no database and no
// coordination state. A partially readable tree still returns the count of the
// locators it did reach, together with the failure, so a caller that only wants
// a total can use the count and leave the reporting to the walk that follows.
func (p *ArtifactPublisher) CountMetadata(ctx context.Context) (int, error) {
	total := 0
	err := p.WalkMetadata(ctx, func(SessionID, string) error {
		total++
		return nil
	})
	return total, err
}
