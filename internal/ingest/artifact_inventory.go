package ingest

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/peasant-labs/peasant/internal/defaults"
)

// walkManagedMetadata yields the metadata locator of every retained session
// under output, each parent before its nested children. It opens no
// transcript and no database. It is the one reader of the whole tree, and
// only `harvest index` runs it: the ordinary harvest selects its work from
// the database and reads a pair only for a session the database selected.
func walkManagedMetadata(ctx context.Context, filesystem FileSystem, output string, visit func(SessionID, string) error) error {
	if visit == nil {
		return fmt.Errorf("walk managed metadata: no locator consumer was supplied")
	}
	hosts, err := filesystem.ReadDir(output)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var failures error
	visitSession := func(directory string, sid SessionID) error {
		path := filepath.Join(directory, string(sid)+defaults.MetadataSuffix)
		info, err := filesystem.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("inspect managed metadata %q: not an owned regular file; restore the expected file type before indexing", path)
		}
		return visit(sid, path)
	}
	for _, host := range hosts {
		if err := ctx.Err(); err != nil {
			return errors.Join(failures, err)
		}
		if !host.IsDir() || host.Name() == managedArtifactStateDir || strings.HasPrefix(host.Name(), defaults.TempDirPrefix) {
			continue
		}
		if _, err := NewHostSlug(host.Name()); err != nil {
			continue
		}
		hostDir := filepath.Join(output, host.Name())
		parents, err := filesystem.ReadDir(hostDir)
		if err != nil {
			failures = errors.Join(failures, err)
			continue
		}
		for _, parent := range parents {
			if err := ctx.Err(); err != nil {
				return errors.Join(failures, err)
			}
			if !parent.IsDir() {
				continue
			}
			sid, err := NewSessionID(parent.Name())
			if err != nil {
				continue
			}
			directory := filepath.Join(hostDir, parent.Name())
			failures = errors.Join(failures, visitSession(directory, sid))
			children, err := filesystem.ReadDir(filepath.Join(directory, defaults.DirSubagents.String()))
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				failures = errors.Join(failures, err)
				continue
			}
			for _, child := range children {
				if !child.IsDir() {
					continue
				}
				childID, err := NewSessionID(child.Name())
				if err != nil {
					continue
				}
				failures = errors.Join(failures, visitSession(filepath.Join(directory, defaults.DirSubagents.String(), child.Name()), childID))
			}
		}
	}
	return failures
}

// RetainedTreeHoldsSessions reports whether output already holds at least
// one session directory, without counting the tree: it reads the host
// directories and stops at the first one that holds a session directory.
func RetainedTreeHoldsSessions(filesystem FileSystem, output string) bool {
	hosts, err := filesystem.ReadDir(output)
	if err != nil {
		return false
	}
	for _, host := range hosts {
		if !host.IsDir() || host.Name() == managedArtifactStateDir || strings.HasPrefix(host.Name(), defaults.TempDirPrefix) {
			continue
		}
		if _, err := NewHostSlug(host.Name()); err != nil {
			continue
		}
		sessions, err := filesystem.ReadDir(filepath.Join(output, host.Name()))
		if err != nil {
			continue
		}
		for _, session := range sessions {
			if !session.IsDir() {
				continue
			}
			if _, err := NewSessionID(session.Name()); err == nil {
				return true
			}
		}
	}
	return false
}
