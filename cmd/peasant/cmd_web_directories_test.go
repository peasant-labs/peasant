package main

import (
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"

	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/githooks"
)

// TestWebServerRunsWithTheCommandsDirectories pins what `peasant web` gives
// its server: the --config-dir, --data-dir, and --state-dir it was started
// with, and the hook binding carrying them, so the settings routes read that
// config directory's hooks.yaml and a hook the server installs uploads from
// the same configuration and store.
func TestWebServerRunsWithTheCommandsDirectories(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	config, data, state := filepath.Join(dir, "config"), filepath.Join(dir, "data"), filepath.Join(dir, "state")
	var got api.ServerConfig
	root := newTestRoot()
	root.AddCommand(&cobra.Command{Use: "serve", RunE: func(cmd *cobra.Command, args []string) error {
		got = withCommandDirectories(cmd, api.ServerConfig{Port: 1})
		return nil
	}})
	root.SetArgs([]string{"--config-dir", config, "--data-dir", data, "--state-dir", state, "serve"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	want := githooks.Binding{ConfigDir: config, DataDir: data, StateDir: state}
	if got.ConfigHome != config || got.DataHome != data || got.StateHome != state || got.HookBinding != want || got.Port != 1 {
		t.Fatalf("server config = homes %q %q %q, binding %+v, port %d; want %q %q %q and %+v, keeping the rest", got.ConfigHome, got.DataHome, got.StateHome, got.HookBinding, got.Port, config, data, state, want)
	}
}
