package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/spf13/cobra"
)

func loadRunConfig(path string, dryRun bool) (*config.Config, error) {
	if !dryRun {
		return loadConfig(path)
	}
	if path != "" {
		data, err := os.ReadFile(path)
		if err == nil {
			return config.Parse(data)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		legacy := strings.TrimSuffix(path, filepath.Ext(path)) + ".toml"
		if _, err := os.Stat(legacy); err == nil {
			return nil, fmt.Errorf("dry-run cannot migrate legacy configuration %s to %s; no files were changed; migrate the configuration with a normal command before retrying", legacy, path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	return config.LoadDefaults(context.Background(), &ingest.ExecGitResolver{}), nil
}

// contentIndexFormats is the index-format registration seam for every CLI
// store open. The store's WithIndexFormats keeps its shape; this one function
// names the content representations the command tree supports, so the
// harmonized handler plugs in at a single site instead of at every open call.
// It currently registers the managed-generation (V2) handler only.
func contentIndexFormats() []store.OpenOption {
	return []store.OpenOption{store.WithIndexFormats(store.V2IndexFormat())}
}

// generationStoreOptions wires the managed-generation writer, snapshot reader
// and per-session lock namespace rooted at the owned output directory. The
// root is the same owned-artifact tree the saved sessions live under; opening
// it here is what lets a capable store activate and read a managed generation
// instead of refusing format 2.
func generationStoreOptions(ownedRoot string) ([]store.OpenOption, error) {
	options := contentIndexFormats()
	if ownedRoot == "" {
		return options, nil
	}
	artifacts, err := store.NewOSGenerationArtifactStore(ownedRoot)
	if err != nil {
		return nil, fmt.Errorf("open managed generation store: %w", err)
	}
	locker, err := store.NewFileSessionLocker(ownedRoot)
	if err != nil {
		return nil, fmt.Errorf("open managed generation lock: %w", err)
	}
	return append(options, store.WithGenerationArtifacts(artifacts, locker)), nil
}

// generationStoreOptionsReadOnly wires the managed-generation reader for a
// dry-run store. It never creates the owned root: a dry run changes no file.
// The reader is what lets a dry run resolve the same native harness targets a
// real run uses instead of reporting the retained baseline and false
// newer-producer refusals. A missing owned root yields the retained baseline,
// which is the target set a fresh store resolves anyway.
func generationStoreOptionsReadOnly(ownedRoot string) ([]store.OpenOption, error) {
	options := contentIndexFormats()
	if ownedRoot == "" {
		return options, nil
	}
	artifacts, err := store.NewOSGenerationArtifactStoreExisting(ownedRoot)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return options, nil
		}
		return nil, fmt.Errorf("open managed generation reader: %w", err)
	}
	locker, err := store.NewFileSessionLocker(ownedRoot)
	if err != nil {
		return nil, fmt.Errorf("open managed generation lock: %w", err)
	}
	return append(options, store.WithGenerationArtifacts(artifacts, locker)), nil
}

// openRunStore opens the analytics store for an ingestion or publication run.
// ownedRoot is the run's resolved output directory; when it is set the store
// can stage, activate and read managed generations. write carries the
// configured write.* budgets into the staging and activation lanes, so a run
// honors the user's batch caps instead of the shipped defaults. A dry-run
// store is opened read-only but with the same target-resolving reader, so its
// forecast matches a real run; it still changes no file.
func openRunStore(cmd *cobra.Command, dryRun bool, ownedRoot string, write ingest.WriteConfig) (*store.Store, error) {
	path := string(defaults.ResolveDBFilePathWith(dataDirOverride(cmd)))
	if dryRun {
		options, err := generationStoreOptionsReadOnly(ownedRoot)
		if err != nil {
			return nil, err
		}
		return store.OpenReadOnlyWithOptions(path, options...)
	}
	directory := string(defaults.ResolveDataDirPathWith(dataDirOverride(cmd)))
	if err := os.MkdirAll(directory, defaults.PrivateDirPerm); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	options, err := generationStoreOptions(ownedRoot)
	if err != nil {
		return nil, err
	}
	options = append(options, store.WithWriteConfig(write))
	return store.Open(path, options...)
}

// runStoreIsAbsent reports whether the analytics database this run would use does
// not exist yet, so a forecast can decide between inspecting existing state and
// forecasting a first harvest. It never creates the file or its directory.
//
// A stat error other than "not found" is returned rather than treated as absent:
// an unreadable path is a state the caller must report, not a fresh install.
func runStoreIsAbsent(cmd *cobra.Command) (string, bool, error) {
	path := string(defaults.ResolveDBFilePathWith(dataDirOverride(cmd)))
	_, err := os.Stat(path)
	switch {
	case err == nil:
		return path, false, nil
	case errors.Is(err, os.ErrNotExist):
		return path, true, nil
	default:
		return path, false, fmt.Errorf("check for an analytics database at %s before forecasting: %w; no files were changed; make the path readable, or pass --data-dir to point at a directory this user can read", path, err)
	}
}
