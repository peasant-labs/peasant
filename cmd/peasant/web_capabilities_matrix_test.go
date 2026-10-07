package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	schema "github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

const expectedWebCapabilityMatrixRows = 4

// matrixReadyTimeout bounds how long a case waits for its server to answer the
// capabilities endpoint before failing with actionable text.
const matrixReadyTimeout = 30 * time.Second

// matrixPollInterval is the delay between readiness polls.
const matrixPollInterval = 50 * time.Millisecond

//go:embed testdata/web_capabilities_matrix.yaml
var webCapabilitiesMatrixYAML []byte

// webCapabilityMatrixFixtures is the real-binary matrix corpus.
type webCapabilityMatrixFixtures struct {
	DeclaredRows  int                       `yaml:"declared_rows"`
	RequiredCases []string                  `yaml:"required_cases"`
	Cases         []webCapabilityMatrixCase `yaml:"cases"`
}

// webCapabilityMatrixCase is one invocation of the built binary and its expected
// advertised token set.
type webCapabilityMatrixCase struct {
	Name           string   `yaml:"name"`
	Experimental   bool     `yaml:"experimental"`
	Foreground     bool     `yaml:"foreground"`
	ExpectedTokens []string `yaml:"expected_tokens"`
}

// decodeWebCapabilityMatrixFixtures strictly decodes the matrix corpus: unknown
// fields and trailing documents are rejected.
func decodeWebCapabilityMatrixFixtures(data []byte) (webCapabilityMatrixFixtures, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)

	var fixtures webCapabilityMatrixFixtures
	if err := decoder.Decode(&fixtures); err != nil {
		return webCapabilityMatrixFixtures{}, fmt.Errorf("decode web-capabilities matrix fixtures with known fields: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return webCapabilityMatrixFixtures{}, fmt.Errorf("decode web-capabilities matrix fixtures: expected exactly one YAML document, got trailing content: %w", err)
	}
	return fixtures, nil
}

// loadWebCapabilityMatrixFixtures decodes and validates the matrix corpus:
// declared row count must match, and case names must be unique.
func loadWebCapabilityMatrixFixtures(t *testing.T) webCapabilityMatrixFixtures {
	t.Helper()

	fixtures, err := decodeWebCapabilityMatrixFixtures(webCapabilitiesMatrixYAML)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if fixtures.DeclaredRows != expectedWebCapabilityMatrixRows || len(fixtures.Cases) != expectedWebCapabilityMatrixRows {
		t.Fatalf(
			"validate web-capabilities matrix row guard: declared=%d, actual=%d, required=%d",
			fixtures.DeclaredRows, len(fixtures.Cases), expectedWebCapabilityMatrixRows,
		)
	}
	if len(fixtures.RequiredCases) == 0 {
		t.Fatal("validate web-capabilities matrix fixtures: required_cases is empty, so no case is protected from deletion")
	}
	names := make(map[string]struct{}, len(fixtures.Cases))
	for _, c := range fixtures.Cases {
		if strings.TrimSpace(c.Name) == "" {
			t.Fatal("validate web-capabilities matrix fixtures: case name is empty")
		}
		if _, dup := names[c.Name]; dup {
			t.Fatalf("validate web-capabilities matrix fixtures: duplicate case %q", c.Name)
		}
		names[c.Name] = struct{}{}
	}
	// Named deletion protection: a count guard cannot say WHICH case vanished, and
	// the case this file calls the forwarding proof is the one worth naming.
	for _, required := range fixtures.RequiredCases {
		if _, ok := names[required]; !ok {
			t.Fatalf("validate web-capabilities matrix fixtures: required case %q is missing", required)
		}
	}
	return fixtures
}

// TestWebCapabilitiesMatrix_StrictDecoder proves the matrix loader rejects
// unknown fields and trailing documents, so the strict guards are not vacuous.
func TestWebCapabilitiesMatrix_StrictDecoder(t *testing.T) {
	t.Parallel()
	unknownField := append([]byte("unexpected_fixture_field: true\n"), webCapabilitiesMatrixYAML...)
	if _, err := decodeWebCapabilityMatrixFixtures(unknownField); err == nil || !strings.Contains(err.Error(), "field unexpected_fixture_field not found") {
		t.Fatalf("unknown field error = %v, want strict field rejection", err)
	}
	trailingDocument := append(append([]byte{}, webCapabilitiesMatrixYAML...), []byte("\n---\nunexpected: document\n")...)
	if _, err := decodeWebCapabilityMatrixFixtures(trailingDocument); err == nil || !strings.Contains(err.Error(), "exactly one YAML document") {
		t.Fatalf("trailing document error = %v, want single-document rejection", err)
	}
}

// TestWebCapabilitiesMatrix boots the real peasant binary for each matrix case
// and asserts the advertised capability set (plus JSON content type and no-store
// cache header) and that the direct /map and /projects SPA routes stay mounted
// regardless of the advertisement. The background cases prove --experimental is
// forwarded to the forked foreground child.
func TestWebCapabilitiesMatrix(t *testing.T) {
	t.Parallel()
	fixtures := loadWebCapabilityMatrixFixtures(t)
	bin := buildPeasantMatrixBinary(t)

	for _, tc := range fixtures.Cases {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			port := freeTCPPort(t)
			baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
			env := isolatedXDGEnv(t)

			if tc.Foreground {
				runForegroundCase(t, bin, env, port, tc.Experimental)
			} else {
				runBackgroundCase(t, bin, env, port, tc.Experimental)
			}

			resp := pollCapabilities(t, baseURL)
			assertAdvertisedTokens(t, tc.ExpectedTokens, resp.tokens, resp.body)
			assertJSONNoStore(t, resp)
			assertSPARouteMounted(t, baseURL+"/map/abc123")
			assertSPARouteMounted(t, baseURL+"/projects/abc123/11111111-1111-4111-8111-111111111111")
		})
	}

	// The same real binary proves that background `web start` refuses a port
	// another server already answers, before it forks. Without that check the
	// readiness probe would take the other server's answer as its own.
	t.Run("background start refuses a port another server answers", func(t *testing.T) {
		holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer holder.Close()
		port := holder.Listener.Addr().(*net.TCPAddr).Port
		env := isolatedXDGEnv(t)
		stateHome := ""
		for _, kv := range env {
			if value, ok := strings.CutPrefix(kv, defaults.EnvXDGStateHome.String()+"="); ok {
				stateHome = value
			}
		}
		t.Cleanup(func() {
			stop := exec.Command(bin, "web", "stop", "--port", strconv.Itoa(port))
			stop.Env = env
			_ = stop.Run()
		})

		cmd := exec.Command(bin, "web", "start", "--no-browser", "--port", strconv.Itoa(port))
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("background `peasant web start` succeeded on port %d that another server answers; output: %s", port, out)
		}
		if !strings.Contains(string(out), "peasant web stop") || strings.Contains(string(out), "Usage:") {
			t.Fatalf("background `peasant web start` output = %q, want the actionable refusal without usage text", out)
		}
		// Track pidFilePath's base name so a future rename cannot leave this
		// assertion checking a path nothing writes. Join the child's stateHome
		// with that base name rather than calling pidFilePath, which reads this
		// test process's state dir.
		pidFile := filepath.Join(stateHome, "peasant", filepath.Base(pidFilePath(port)))
		if _, statErr := os.Stat(pidFile); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("background `peasant web start` wrote %s for a server it did not start (stat error: %v)", pidFile, statErr)
		}
	})
}

// peasantCLIOnce guards the single per-test-binary CLI build. Both the web
// capabilities matrix and any future in-process test that needs the real
// production entry point share this one artifact, so the test binary runs
// exactly one `go build` no matter how many tests ask for it.
var (
	peasantCLIOnce sync.Once
	peasantCLIPath string
	peasantCLIErr  error
)

// buildPeasantMatrixBinary returns the peasant CLI for the matrix. The binary is
// built once per test binary (through the shared peasantCLIOnce) so the matrix
// drives the real production entry point rather than an in-process server.
//
// The existing PEASANT_BIN seam is honored first: when a caller has already
// produced a CLI, that path is used instead of building, so a suite that
// pre-builds once pays for the build once, not once per test binary.
func buildPeasantMatrixBinary(t *testing.T) string {
	t.Helper()
	if injected := strings.TrimSpace(os.Getenv(defaults.EnvPeasantBin.String())); injected != "" {
		return injected
	}
	peasantCLIOnce.Do(func() {
		dir, err := os.MkdirTemp("", "peasant-matrix-bin-*")
		if err != nil {
			peasantCLIErr = fmt.Errorf("create temp dir for the peasant CLI build: %w", err)
			return
		}
		out := filepath.Join(dir, "peasant")
		// -race=false is explicit: the matrix asserts the advertised
		// capabilities of the production binary, not race coverage of it. The
		// build flag is resolved by the no-race partition registry against this
		// exec site. -buildvcs=false keeps the build from stamping VCS, which
		// mis-resolves in a linked worktree whose .git is a file; the matrix
		// asserts advertised capabilities, not build provenance.
		cmd := exec.Command("go", "build", "-race=false", "-buildvcs=false", "-o", out, "github.com/peasant-labs/peasant/cmd/peasant")
		if output, err := cmd.CombinedOutput(); err != nil {
			peasantCLIErr = fmt.Errorf(
				"build peasant binary for the web capabilities matrix failed.\n"+
					"  what: `go build -race=false -o %s github.com/peasant-labs/peasant/cmd/peasant` returned an error\n"+
					"  why:  %v\n"+
					"  where: cmd/peasant/web_capabilities_matrix_test.go buildPeasantMatrixBinary\n"+
					"  when: before booting any matrix case\n"+
					"  means: the matrix cannot exercise the real binary\n"+
					"  fix:  run the build manually to see the compiler error; ensure the module builds\n"+
					"  output:\n%s",
				out, err, output,
			)
			return
		}
		peasantCLIPath = out
	})
	if peasantCLIErr != nil {
		t.Fatalf("%v", peasantCLIErr)
	}
	return peasantCLIPath
}

// isolatedXDGEnv returns the parent process environment with HOME and the XDG
// base dirs redirected under a fresh temp dir, so each case's data, config, and
// state (including the background pid file) stay hermetic and never touch the
// developer's real peasant directories. Background children inherit this env
// through the parent process, so --experimental forwarding is the only thing
// carrying the capability decision into the child.
func isolatedXDGEnv(t *testing.T) []string {
	t.Helper()
	root := t.TempDir()
	overrides := map[string]string{
		"HOME":                             root,
		defaults.EnvXDGConfigHome.String(): filepath.Join(root, "config"),
		defaults.EnvXDGDataHome.String():   filepath.Join(root, "data"),
		defaults.EnvXDGStateHome.String():  filepath.Join(root, "state"),
	}
	base := os.Environ()
	env := make([]string, 0, len(base)+len(overrides))
	for _, kv := range base {
		key := kv[:strings.IndexByte(kv, '=')]
		if _, overridden := overrides[key]; overridden {
			continue
		}
		env = append(env, kv)
	}
	for key, value := range overrides {
		env = append(env, key+"="+value)
	}
	return env
}

// runForegroundCase starts `peasant web start --foreground` and registers its
// termination. The process blocks while serving, so it runs under a cancelable
// context and is killed on cleanup.
func runForegroundCase(t *testing.T, bin string, env []string, port int, experimental bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())

	args := []string{"web", "start", "--foreground", "--no-browser", "--port", strconv.Itoa(port)}
	if experimental {
		args = append(args, "--experimental")
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = env
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start foreground `peasant web start`: %v (output: %s)", err, out.String())
	}
	t.Cleanup(func() {
		cancel()
		_ = cmd.Wait()
	})
}

// runBackgroundCase starts `peasant web start` in background mode, which forks a
// detached foreground child and returns once the child is ready. The child
// survives the parent, so it is shut down through the CLI's own stop path on
// cleanup.
func runBackgroundCase(t *testing.T, bin string, env []string, port int, experimental bool) {
	t.Helper()

	args := []string{"web", "start", "--no-browser", "--port", strconv.Itoa(port)}
	if experimental {
		args = append(args, "--experimental")
	}
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("run background `peasant web start`: %v (output: %s)", err, out.String())
	}

	t.Cleanup(func() {
		stop := exec.Command(bin, "web", "stop", "--port", strconv.Itoa(port))
		stop.Env = env
		out, err := stop.CombinedOutput()
		if err != nil {
			t.Errorf("`web stop --port %d` failed: %v (output: %s)", port, err, out)
			return
		}
		if stateHome := envValue(env, defaults.EnvXDGStateHome.String()); stateHome != "" {
			pidFile := filepath.Join(stateHome, "peasant", filepath.Base(pidFilePath(port)))
			if _, statErr := os.Stat(pidFile); !errors.Is(statErr, os.ErrNotExist) {
				t.Errorf("`web stop --port %d` left the PID file at %s (stat error: %v)", port, pidFile, statErr)
			}
		}
		if !waitPortClosed(port) {
			t.Errorf("`web stop --port %d` left the server answering on 127.0.0.1:%d", port, port)
		}
	})
}

// envValue returns the value of key in a KEY=VALUE environment slice.
func envValue(env []string, key string) string {
	prefix := key + "="
	for _, kv := range env {
		if value, ok := strings.CutPrefix(kv, prefix); ok {
			return value
		}
	}
	return ""
}

// waitPortClosed reports whether nothing answers on the loopback port within a
// bounded retry.
func waitPortClosed(port int) bool {
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", address, 500*time.Millisecond)
		if err != nil {
			return true
		}
		_ = conn.Close()
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

// capabilitiesResponse captures the observable result of one capabilities fetch.
type capabilitiesResponse struct {
	contentType  string
	cacheControl string
	tokens       []string
	body         string
}

// pollCapabilities repeatedly GETs the capabilities endpoint until it answers
// 200 with a decodable body or the deadline elapses, then returns the observed
// response. It fails with actionable text on timeout.
func pollCapabilities(t *testing.T, baseURL string) capabilitiesResponse {
	t.Helper()
	url := baseURL + defaults.RouteConfigCapabilities.String()
	deadline := time.Now().Add(matrixReadyTimeout)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err != nil {
			lastErr = err
			time.Sleep(matrixPollInterval)
			continue
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			time.Sleep(matrixPollInterval)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("status %d (body: %s)", resp.StatusCode, string(body))
			time.Sleep(matrixPollInterval)
			continue
		}
		var decoded schema.UICapabilitiesResponse
		if err := json.Unmarshal(body, &decoded); err != nil {
			lastErr = fmt.Errorf("unmarshal %q: %w", string(body), err)
			time.Sleep(matrixPollInterval)
			continue
		}
		return capabilitiesResponse{
			contentType:  resp.Header.Get(defaults.HeaderContentType),
			cacheControl: resp.Header.Get(defaults.HeaderCacheControl),
			tokens:       decoded.UICapabilities,
			body:         string(body),
		}
	}
	t.Fatalf(
		"capabilities endpoint never became ready.\n"+
			"  what: GET %s did not return a decodable 200 within %s\n"+
			"  why:  last attempt failed with: %v\n"+
			"  where: cmd/peasant/web_capabilities_matrix_test.go pollCapabilities\n"+
			"  when: while polling the booted server for its capability advertisement\n"+
			"  means: the server never started, bound a different port, or crashed on startup\n"+
			"  fix:  run the same `peasant web start` invocation manually with these env dirs and inspect its output",
		url, matrixReadyTimeout, lastErr,
	)
	return capabilitiesResponse{}
}

// assertAdvertisedTokens compares the expected token list against the advertised
// list, treating an empty expectation and a nil/omitted advertisement as equal.
func assertAdvertisedTokens(t *testing.T, want, got []string, body string) {
	t.Helper()
	if len(want) != len(got) {
		t.Errorf("advertised tokens: want %v (%d), got %v (%d) (body: %s)", want, len(want), got, len(got), body)
		return
	}
	for i := range want {
		if want[i] != got[i] {
			t.Errorf("advertised tokens index %d: want %q, got %q (want=%v got=%v)", i, want[i], got[i], want, got)
		}
	}
}

// assertJSONNoStore asserts the capabilities response carried the JSON content
// type and the no-store cache header.
func assertJSONNoStore(t *testing.T, resp capabilitiesResponse) {
	t.Helper()
	if resp.contentType != defaults.ContentJSON.String() {
		t.Errorf("Content-Type = %q, want %q", resp.contentType, defaults.ContentJSON.String())
	}
	if resp.cacheControl != defaults.CacheControlNoStore {
		t.Errorf("Cache-Control = %q, want %q", resp.cacheControl, defaults.CacheControlNoStore)
	}
}

// assertSPARouteMounted asserts a direct SPA route answers 200 with an HTML
// content type, proving the route stays mounted regardless of the advertised
// capabilities (capabilities gate discoverability only, never routes). A hidden
// route would answer 404 here.
func assertSPARouteMounted(t *testing.T, url string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("GET %s: status = %d, want %d (body: %s)", url, resp.StatusCode, http.StatusOK, string(body))
	}
	if ct := resp.Header.Get(defaults.HeaderContentType); !strings.HasPrefix(ct, defaults.ContentHTML.String()) {
		t.Errorf("GET %s: Content-Type = %q, want prefix %q", url, ct, defaults.ContentHTML.String())
	}
}

// freeTCPPort reserves an ephemeral port that is free on both loopback
// addresses, because the server binds both, and returns it. There is a small
// window between closing the listeners and the server binding; it is
// acceptable for a local test and mirrors the existing free-port discovery
// idiom.
func freeTCPPort(t *testing.T) int {
	t.Helper()
	for attempt := 1; attempt <= 8; attempt++ {
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("reserve free TCP port: %v", err)
		}
		port := ln.Addr().(*net.TCPAddr).Port
		v6, v6Err := net.Listen("tcp6", net.JoinHostPort("::1", strconv.Itoa(port)))
		if v6Err == nil {
			_ = v6.Close()
		}
		if err := ln.Close(); err != nil {
			t.Fatalf("release reserved TCP port %d: %v", port, err)
		}
		// Any IPv6 failure other than a busy port means the host has no IPv6
		// loopback, and the server then binds IPv4 only.
		if v6Err == nil || !errors.Is(v6Err, syscall.EADDRINUSE) {
			return port
		}
	}
	t.Fatal("every reserved TCP port was already taken on the IPv6 loopback")
	return 0
}
