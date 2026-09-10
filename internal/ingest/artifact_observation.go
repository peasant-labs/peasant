package ingest

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/schema"
)

func findArtifactDirectory(root ArtifactRoot, sid SessionID) (string, error) {
	hosts, err := root.ReadDir(".")
	if err != nil {
		return "", err
	}
	found := ""
	check := func(directory string) error {
		// Metadata may be missing after an interrupted legacy write. Observe
		// any exact owned transcript too, so replacement can prove its old bytes.
		for _, suffix := range []string{defaults.MetadataSuffix, "--transcript.json", "--transcript.jsonl"} {
			_, err := root.Lstat(filepath.Join(directory, string(sid)+suffix))
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			if found != "" && found != directory {
				return fmt.Errorf("observe session %s: multiple managed artifact locations exist; no files were changed; resolve duplicate session artifacts before retrying", sid)
			}
			found = directory
			break
		}
		return nil
	}
	for _, host := range hosts {
		if !host.IsDir() || host.Name() == managedArtifactStateDir || strings.HasPrefix(host.Name(), defaults.TempDirPrefix) {
			continue
		}
		if err := check(filepath.Join(host.Name(), string(sid))); err != nil {
			return "", err
		}
		parents, err := root.ReadDir(host.Name())
		if err != nil {
			return "", err
		}
		for _, parent := range parents {
			if !parent.IsDir() {
				continue
			}
			if err := check(filepath.Join(host.Name(), parent.Name(), defaults.DirSubagents.String(), string(sid))); err != nil {
				return "", err
			}
		}
	}
	return found, nil
}

func observeArtifactFiles(root ArtifactRoot, directory string, sid SessionID, debugNames []string) ([]artifactFileVersion, error) {
	if directory == "" {
		return nil, nil
	}
	if !validArtifactDirectory(directory, sid) {
		return nil, fmt.Errorf("observe session %s: invalid managed directory %q; no files were changed", sid, directory)
	}
	names := []string{string(sid) + defaults.MetadataSuffix, string(sid) + "--transcript.json", string(sid) + "--transcript.jsonl"}
	for _, name := range debugNames {
		if !validArtifactDebugName(name) {
			return nil, fmt.Errorf("observe session %s: invalid debug filename %q", sid, name)
		}
		names = append(names, filepath.Join(defaults.DirDebug.String(), name))
	}
	// The names above are what THIS run intends to write. A session directory
	// can also still hold members of the same owned families that an earlier
	// run wrote and this one no longer produces, and those are the session's
	// own files just as much: observing them is what lets the publication
	// retire them instead of leaving them beside the current artifact forever.
	// Anything whose name is not one of peasant's own is not observed and so
	// can never be touched.
	stale, err := observeStaleOwnedNames(root, directory, sid, names)
	if err != nil {
		return nil, err
	}
	names = append(names, stale...)
	versions := make([]artifactFileVersion, 0, len(names))
	for _, name := range names {
		path := filepath.Join(directory, name)
		info, err := root.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			versions = append(versions, artifactFileVersion{Path: path})
			continue
		}
		if err != nil {
			return nil, managedInputIOError(path, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("observe session %s: managed path %q is not a regular owned file; no replacement was started; restore the expected file type", sid, path)
		}
		data, err := root.ReadFile(path)
		if err != nil {
			return nil, managedInputIOError(path, err)
		}
		hash := schema.ComputeTranscriptHash(data)
		versions = append(versions, artifactFileVersion{Path: path, Hash: &hash})
	}
	return versions, nil
}

// observeStaleOwnedNames lists the owned files already in the session directory
// whose names known does not already carry. It reads the directory and its
// debug subdirectory only; a directory that is absent contributes nothing, and
// a member the naming rule does not claim is skipped rather than refused,
// because a publication must leave a user's own files alone.
func observeStaleOwnedNames(root ArtifactRoot, directory string, sid SessionID, known []string) ([]string, error) {
	var found []string
	for _, sub := range []string{"", defaults.DirDebug.String()} {
		listing := directory
		if sub != "" {
			listing = filepath.Join(directory, sub)
		}
		entries, err := root.ReadDir(listing)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, managedInputIOError(listing, err)
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			name := filepath.Join(sub, entry.Name())
			if slices.Contains(known, name) || slices.Contains(found, name) {
				continue
			}
			if _, err := newArtifactOwnedFile(filepath.Join(directory, name), directory, sid); err != nil {
				continue
			}
			found = append(found, name)
		}
	}
	slices.Sort(found)
	return found, nil
}
