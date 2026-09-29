package main

import (
	"slices"
	"testing"

	"github.com/spf13/cobra"
)

// TestWebServerSpawnForwardsTheCommandDirectories checks the argv of the
// detached server: it serves the configuration and data of the command that
// forked it, and adds no directory flag the command did not set.
func TestWebServerSpawnForwardsTheCommandDirectories(t *testing.T) {
	t.Parallel()
	spawnFor := func(args ...string) []string {
		t.Helper()
		var spawn webServerSpawn
		probe := &cobra.Command{Use: "probe", RunE: func(cmd *cobra.Command, _ []string) error {
			spawn = webServerSpawnFor(cmd, 9123)
			return nil
		}}
		root := newTestRoot()
		root.AddCommand(probe)
		root.SetArgs(append(args, "probe"))
		if err := root.Execute(); err != nil {
			t.Fatal(err)
		}
		return spawn.args()
	}

	got := spawnFor("--config", "/cfg/peasant/config.yaml", "--data-dir", "/data", "--config-dir", "/cfg", "--state-dir", "/state")
	want := []string{"web", "start", "--foreground", "--port", "9123", "--config", "/cfg/peasant/config.yaml", "--data-dir", "/data", "--config-dir", "/cfg", "--state-dir", "/state"}
	if !slices.Equal(got, want) {
		t.Fatalf("server argv with overrides = %q, want %q", got, want)
	}

	got = spawnFor("--config", "/cfg/peasant/config.yaml")
	want = []string{"web", "start", "--foreground", "--port", "9123", "--config", "/cfg/peasant/config.yaml"}
	if !slices.Equal(got, want) {
		t.Fatalf("server argv without overrides = %q, want %q", got, want)
	}
}
