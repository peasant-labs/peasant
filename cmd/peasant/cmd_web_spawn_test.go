package main

import (
	_ "embed"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/spf13/cobra"
)

//go:embed testdata/web-spawn-argv.yaml
var webSpawnArgvYAML []byte

type webSpawnArgvFixture struct {
	RequiredNames []string `yaml:"requiredNames"`
	Cases         []struct {
		Name string   `yaml:"name"`
		Args []string `yaml:"args"`
		Want []string `yaml:"want"`
	} `yaml:"cases"`
}

// TestWebServerSpawnForwardsTheCommandDirectories checks the argv of the
// detached server against the root flags of the command that forked it.
func TestWebServerSpawnForwardsTheCommandDirectories(t *testing.T) {
	t.Parallel()
	var fixture webSpawnArgvFixture
	if err := testutil.DecodeNamedFixtureYAML(webSpawnArgvYAML, &fixture); err != nil {
		t.Fatalf("decode web spawn argv fixture: %v", err)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			var spawn webServerSpawn
			probe := &cobra.Command{Use: "probe", RunE: func(cmd *cobra.Command, _ []string) error {
				spawn = webServerSpawnFor(cmd, 9123)
				return nil
			}}
			root := newTestRoot()
			root.AddCommand(probe)
			root.SetArgs(append(slices.Clone(tc.Args), "probe"))
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}
			if got := spawn.args(); !slices.Equal(got, tc.Want) {
				t.Fatalf("server argv = %q, want %q", got, tc.Want)
			}
		})
	}
}

// TestWaitForWebServerStopsAtItsWait checks that a listener that accepts the
// connection and never answers cannot stretch the readiness wait past its
// bound, even when the client itself would wait far longer.
func TestWaitForWebServerStopsAtItsWait(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() {
		close(release)
		server.Close()
	})
	client := &http.Client{Timeout: time.Hour}

	done := make(chan bool, 1)
	go func() {
		done <- waitForWebServer(t.Context(), client, server.URL, 50*time.Millisecond, 10*time.Millisecond)
	}()
	select {
	case ready := <-done:
		if ready {
			t.Fatal("a server that never answered was reported ready")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the readiness wait outlived its 50ms bound")
	}
}
