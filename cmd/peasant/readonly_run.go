package main

import (
	"context"
	"errors"
	"fmt"
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

func openRunStore(cmd *cobra.Command, dryRun bool) (*store.Store, error) {
	path := string(defaults.ResolveDBFilePathWith(dataDirOverride(cmd)))
	if dryRun {
		return store.OpenReadOnly(path)
	}
	directory := string(defaults.ResolveDataDirPathWith(dataDirOverride(cmd)))
	if err := os.MkdirAll(directory, defaults.PrivateDirPerm); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	return store.Open(path)
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
