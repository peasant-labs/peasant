package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testkit/testwait"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

type browsingStartupCase struct {
	Name           string `yaml:"name"`
	Document       string `yaml:"document"`
	SelectionError string `yaml:"selection_error"`
	StartupError   string `yaml:"startup_error"`
}

// Execute the real root, pre-run, foreground startup, disk loader and registered
// HTTP routes. Only the temporary configuration and stored history are synthetic.
func TestBrowsingStartupFromSavedConfig(t *testing.T) {
	fixtures := loadWebCapabilityMatrixFixtures(t)
	seen := map[string]bool{}
	for _, tc := range fixtures.StartupCases {
		invalidIdentity := seen[tc.Name] || tc.Name == "" || tc.Document == ""
		ambiguousBoundary := (tc.SelectionError == "") == (tc.StartupError == "")
		if invalidIdentity || ambiguousBoundary {
			t.Fatalf("startup fixture %q must be unique, have a document, and name exactly one failure boundary", tc.Name)
		}
		seen[tc.Name] = true
	}
	if err := testutil.RequireFixtureNames("browsing startup", "case", strings.Fields("unsupported-selection-mode deprecated-selection-policy malformed-selection-exclusion unrelated-redaction-stays-strict unrelated-display-stays-strict unrelated-write-budget-stays-strict malformed-yaml-stays-strict"), seen); err != nil {
		t.Fatal(err)
	}
	commands := map[string]bool{}
	for _, command := range fixtures.StrictCommands {
		commands[strings.Join(command, " ")] = true
	}
	if err := testutil.RequireFixtureNames("browsing startup", "strict command", []string{"harvest", "harvest --dry-run", "village push --json", "village push --dry-run --json", "prune --unselected"}, commands); err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixtures.StartupCases {
		t.Run(tc.Name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(path, []byte(tc.Document), 0600); err != nil {
				t.Fatal(err)
			}
			args := []string{"--config", path, "--data-dir", dir, "--config-dir", dir, "--state-dir", dir}
			if tc.StartupError != "" {
				ctx, cancel := context.WithTimeout(t.Context(), matrixReadyTimeout)
				defer cancel()
				root := buildRootCommand()
				root.SetOut(io.Discard)
				root.SetErr(io.Discard)
				root.SetArgs(append(args, "web", "start", "--foreground", "--port", strconv.Itoa(freeTCPPort(t))))
				if err := root.ExecuteContext(ctx); err == nil || !strings.Contains(err.Error(), tc.StartupError) {
					t.Fatalf("web startup error = %v, want %q", err, tc.StartupError)
				}
				root = buildRootCommand()
				root.SetOut(io.Discard)
				root.SetErr(io.Discard)
				root.SetArgs(append(args, "tui", "--mock-data-store", "invalid-section"))
				if err := root.ExecuteContext(t.Context()); err == nil || !strings.Contains(err.Error(), tc.StartupError) {
					t.Fatalf("tui startup error = %v, want %q", err, tc.StartupError)
				}
				return
			}
			// Strict commands must refuse before ingesting, publishing or pruning.
			writeTestCredentials(t, dir)
			for _, command := range fixtures.StrictCommands {
				root := buildRootCommand()
				root.SetOut(io.Discard)
				root.SetErr(io.Discard)
				root.SetArgs(append(append([]string{}, args...), command...))
				if err := root.ExecuteContext(t.Context()); err == nil || !strings.Contains(err.Error(), tc.SelectionError) {
					t.Fatalf("%v error = %v, want %q", command, err, tc.SelectionError)
				}
			}
			root := buildRootCommand()
			dbPath := string(defaults.ResolveDBFilePathWith(dir))
			if err := os.MkdirAll(filepath.Dir(dbPath), 0700); err != nil {
				t.Fatal(err)
			}
			storetest.CopyGoldenTo(t, dbPath)
			db, err := openPreparedStore(t, dbPath)
			if err != nil {
				t.Fatal(err)
			}
			entry := makeCmdStoreEntry(t, testSessionUUID, "example.test--garden", "https://example.test/garden.git", "main", 1705276800000)
			if err := db.InsertSessions(t.Context(), []ingest.StoreEntry{entry}); err != nil {
				db.Close()
				t.Fatal(err)
			}
			err = db.UpdateIndexState(t.Context(), ingest.SessionID(testSessionUUID), ingest.HarvesterVersionRegistry[ingest.HarnessClaudeCode].IndexerVersion, 1705276800000)
			db.Close()
			if err != nil {
				t.Fatal(err)
			}
			root.SetOut(io.Discard)
			root.SetErr(io.Discard)
			port := freeTCPPort(t)
			root.SetArgs(append(args, "web", "start", "--foreground", "--no-browser", "--port", strconv.Itoa(port)))
			ctx, cancel := context.WithTimeout(t.Context(), matrixReadyTimeout)
			done := make(chan error, 1)
			go func() { done <- root.ExecuteContext(ctx) }()
			t.Cleanup(func() {
				cancel()
				select {
				case <-done:
				case <-time.After(matrixReadyTimeout):
					t.Error("foreground server did not stop after context cancellation")
				}
			})
			base := fmt.Sprintf("http://127.0.0.1:%d", port)
			client := &http.Client{Timeout: time.Second}
			// The real HTTP listener has no readiness channel; poll its route.
			testwait.Until(t, "foreground loopback capabilities route", func() bool {
				select {
				case err := <-done:
					done <- err
					t.Fatalf("foreground startup exited before serving stored history: %v", err)
				default:
				}
				if err := ctx.Err(); err != nil {
					t.Fatalf("foreground readiness deadline exceeded: %v", err)
				}
				response, err := client.Get(base + defaults.RouteConfigCapabilities.String())
				if err == nil {
					response.Body.Close()
					if response.StatusCode == http.StatusOK {
						return true
					}
				}
				return false
			})
			read := func(route string, status int) []byte {
				t.Helper()
				response, err := client.Get(base + route)
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				body, err := io.ReadAll(response.Body)
				if err != nil || response.StatusCode != status {
					t.Fatalf("GET %s: status %d, body %s, error %v; want %d", route, response.StatusCode, body, err, status)
				}
				return body
			}
			if body := read(defaults.RouteSessions.String(), http.StatusOK); !bytes.Contains(body, []byte(testSessionUUID)) {
				t.Fatalf("stored session missing: %s", body)
			}
			if body := read(defaults.RouteProjectsSummary.String(), http.StatusOK); !bytes.Contains(body, []byte(testutil.TestProjectHash.String())) {
				t.Fatalf("stored project missing: %s", body)
			}
			if body := read("/api/v1/sessions/"+testSessionUUID, http.StatusOK); !bytes.Contains(body, []byte(testSessionUUID)) {
				t.Fatalf("direct session missing: %s", body)
			}
			body := read(defaults.RoutePublications.String()+"?sessionIds="+testSessionUUID, http.StatusInternalServerError)
			var failure map[string]json.RawMessage
			if err := json.Unmarshal(body, &failure); err != nil {
				t.Fatal(err)
			}
			partialMetadata := failure["items"] != nil || failure["publications"] != nil || failure["next"] != nil
			missingRemedy := !bytes.Contains(body, []byte(tc.SelectionError)) || !bytes.Contains(body, []byte("kickstart"))
			if partialMetadata || missingRemedy {
				t.Fatalf("metadata failure guessed flags or lost remedy: %s", body)
			}
			read(defaults.RoutePublications.String()+"?sessionIds=11111111-1111-4111-8111-111111111111", http.StatusOK)
			body = read("/api/v1/web/discovery", http.StatusInternalServerError)
			if !bytes.Contains(body, []byte(tc.SelectionError)) || !bytes.Contains(body, []byte("kickstart")) {
				t.Fatalf("ingest selection metadata lost its cause or remedy: %s", body)
			}
			// Ingest may no longer rely on browsing startup having validated its
			// selector. Its existing asynchronous failure must precede source I/O.
			response, err := client.Post(base+defaults.RouteSyncIngest.String(), "application/json", nil)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("start ingest status = %d, want existing asynchronous 200", response.StatusCode)
			}
			testwait.Until(t, "web ingest refuses invalid selector before source I/O", func() bool {
				body := read(defaults.RouteSyncIngestStatus.String(), http.StatusOK)
				var status struct {
					Status string `json:"status"`
					Error  string `json:"error"`
				}
				if err := json.Unmarshal(body, &status); err != nil {
					t.Fatal(err)
				}
				if status.Status == "running" {
					return false
				}
				if status.Status != "error" || !strings.Contains(status.Error, tc.SelectionError) {
					t.Fatalf("web ingest did not refuse invalid selection: %s", body)
				}
				return true
			})
			// The real TUI command must pass its disk-load boundary. The invalid
			// mock override stops it before terminal acquisition, without a PTY.
			tuiRoot := buildRootCommand()
			tuiRoot.SetOut(io.Discard)
			tuiRoot.SetErr(io.Discard)
			tuiRoot.SetArgs(append(args, "tui", "--mock-data-store", "invalid-section"))
			if err := tuiRoot.ExecuteContext(t.Context()); err == nil || !strings.Contains(err.Error(), "invalid --mock-data-store") {
				t.Fatalf("TUI did not reach override validation after loading: %v", err)
			}
			loaded, err := config.LoadForBrowsing(path, &ingest.OSFileSystem{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			raw := config.BaseConfig()
			if err := yaml.Unmarshal([]byte(tc.Document), raw); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(loaded.Selection, raw.Selection) {
				t.Fatalf("raw selector changed: got %+v, want %+v", loaded.Selection, raw.Selection)
			}
			if err := config.SaveAtomic(path, loaded); err == nil {
				t.Fatal("configuration writer accepted invalid saved selector")
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != tc.Document {
				t.Fatalf("browsing or refused write changed saved config: %q, %v", after, err)
			}
		})
	}
}
