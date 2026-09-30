package main

import (
	"context"
	_ "embed"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/testkit/testwait"
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

// webProbeBlocked starts an httptest server that holds each health request open
// until the request's own context ends or the test releases it, and signals the
// returned channel once per request that reached the handler. The signal is
// buffered, so a handler that must return never blocks on it; the cleanup
// releases the handlers before Close waits for them.
func webProbeBlocked(t *testing.T) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	requested := make(chan struct{}, 1)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case requested <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() {
		close(release)
		server.Close()
	})
	return server, requested
}

// TestWaitForWebServerStopsOnContextCancel checks that cancelling the wait's
// context ends it at once: only the cancel can end a one-hour wait, because the
// double never answers.
func TestWaitForWebServerStopsOnContextCancel(t *testing.T) {
	t.Parallel()
	server, requested := webProbeBlocked(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client := &http.Client{Timeout: time.Hour}

	done := make(chan bool, 1)
	go func() { done <- waitForWebServer(ctx, client, server.URL, time.Hour, 10*time.Millisecond) }()
	testwait.Receive(t, requested, "the readiness probe's first health request")
	cancel()
	if ready := testwait.Receive(t, done, "the readiness probe result after the cancel"); ready {
		t.Fatal("a cancelled readiness wait reported the server ready")
	}
}

// TestWaitForWebServerReportsReady checks the ready path: a server that answers
// the health route with 200 is reported ready.
func TestWaitForWebServerReportsReady(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	client := &http.Client{Timeout: time.Hour}
	if !waitForWebServer(t.Context(), client, server.URL, webServerReadyWait, 10*time.Millisecond) {
		t.Fatal("a server answering the health route was not reported ready")
	}
}

// TestWaitForWebServerStopsAtItsWait checks that a listener that accepts the
// connection and never answers cannot stretch the readiness wait past its
// bound, even when the client itself would wait far longer.
func TestWaitForWebServerStopsAtItsWait(t *testing.T) {
	t.Parallel()
	server, _ := webProbeBlocked(t)
	client := &http.Client{Timeout: time.Hour}

	done := make(chan bool, 1)
	go func() {
		done <- waitForWebServer(t.Context(), client, server.URL, 50*time.Millisecond, 10*time.Millisecond)
	}()
	if ready := testwait.Receive(t, done, "the readiness wait result at its own bound"); ready {
		t.Fatal("a server that never answered was reported ready")
	}
}

// TestRunWebBackgroundCancelSkipsBrowserAndReturnsCanceled is the integrated
// cancel path. By design it prints the existing warning and URL, stop, and PID
// lines to stdout and stderr; that output is not a failure. The start must
// stop the probe, skip the browser, and return the wrapped cancellation. The
// half-budget bound fails a probe that waits out its own budget after the
// cancel.
func TestRunWebBackgroundCancelSkipsBrowserAndReturnsCanceled(t *testing.T) {
	t.Parallel()
	server, requested := webProbeBlocked(t)
	addr, ok := server.Listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("httptest listener address = %T, want *net.TCPAddr", server.Listener.Addr())
	}
	pidFile := filepath.Join(t.TempDir(), "web.pid")

	var browserMu sync.Mutex
	var browserCalls []string
	deps := webStartDependencies{
		startServer: func(webServerSpawn) (int, string, error) { return 4242, pidFile, nil },
		openBrowser: func(url string) error {
			browserMu.Lock()
			defer browserMu.Unlock()
			browserCalls = append(browserCalls, url)
			return nil
		},
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runWebBackground(ctx, deps, webServerSpawn{port: addr.Port}, false) }()

	testwait.Receive(t, requested, "the background start's first health request")
	cancelAt := time.Now()
	cancel()
	err := testwait.Receive(t, done, "the background start result after the cancel")

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("background start error = %v, want an error wrapping context.Canceled", err)
	}
	browserMu.Lock()
	calls := slices.Clone(browserCalls)
	browserMu.Unlock()
	if len(calls) != 0 {
		t.Fatalf("the browser was opened on a cancelled start: %v", calls)
	}
	if elapsed := time.Since(cancelAt); elapsed >= webServerReadyWait/2 {
		t.Fatalf("the cancelled start returned after %s, want less than half the %s readiness budget", elapsed, webServerReadyWait)
	}
}
