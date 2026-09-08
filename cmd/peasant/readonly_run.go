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
