package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dayvidpham/bestiary"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// executeModelsCmd runs the models cobra command under a test root with
// --data-dir=dir (parallel-safe; no t.Setenv), capturing combined output.
func executeModelsCmd(t *testing.T, dir string, args []string) (string, error) {
	t.Helper()
	return executeWithDataDir(t, BuildModelsCommand(), dir, args)
}

// runModelsSync builds the models command with an injected ModelFetcher and runs
// the `sync` subcommand with the given extra args. It registers the persistent
// --data-dir/--config-dir flags (mirroring main()) and points them at dir so the
// command resolves its DB under dir/<AppName>/peasant.db without mutating
// process-global env — this is what lets the models tests run with t.Parallel().
// It drives past the [y/N] confirmation prompt (cmd.SetIn) and returns stdout and
// stderr SEPARATELY — the static-fallback warning is written to stderr and must be
// asserted apart from the success line on stdout.
func runModelsSync(t *testing.T, ctx context.Context, dir string, fetcher ModelFetcher, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := buildModelsCommand(fetcher)
	cmd.PersistentFlags().String("data-dir", "", "")
	cmd.PersistentFlags().String("config-dir", "", "")
	cmd.PersistentFlags().String("config", "", "")
	var outBuf, errBuf bytes.Buffer
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	cmd.SetIn(strings.NewReader("y\n"))
	cmd.SetArgs(append([]string{"--data-dir", dir, "--config-dir", dir, "sync"}, args...))
	err = cmd.ExecuteContext(ctx)
	return outBuf.String(), errBuf.String(), err
}

// failFetcher is a ModelFetcher whose methods fail the test if ever called.
// It proves the invalid-provider guard fires BEFORE any fetch (hermetic, no network).
type failFetcher struct{ t *testing.T }

var _ ModelFetcher = failFetcher{}

func (f failFetcher) FetchModels(ctx context.Context) ([]bestiary.ModelInfo, error) {
	f.t.Fatal("FetchModels called: the provider-filter guard must fire BEFORE any fetch")
	return nil, nil
}

func (f failFetcher) FetchModelsByProvider(ctx context.Context, p bestiary.Provider) ([]bestiary.ModelInfo, error) {
	f.t.Fatalf("FetchModelsByProvider(%s) called: the provider-filter guard must fire BEFORE any fetch", p)
	return nil, nil
}

// TestModelsSync_InvalidProvider_HermeticGuard verifies that `--provider=bogus`
// hard-errors before any network access (the failFetcher would fail the test if
// reached), echoing the invalid value and listing valid providers (C2). The guard
// fires regardless of connectivity, so this holds even offline.
func TestModelsSync_InvalidProvider_HermeticGuard(t *testing.T) {
	t.Parallel()
	// Defensive isolation: the guard returns before openDB, but never let a test
	// touch the real data dir.
	_, _, err := runModelsSync(t, context.Background(), t.TempDir(), failFetcher{t}, "--provider=bogus")
	if err == nil {
		t.Fatal("expected a non-zero error for --provider=bogus, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "bogus") {
		t.Errorf("error should echo the invalid value \"bogus\"; got: %v", err)
	}
	// The error must list valid providers; assert a known one is present
	// (runtime-derived from bestiary, not a literal list).
	known := bestiary.ProviderAnthropic.String()
	if !strings.Contains(msg, known) {
		t.Errorf("error should list valid providers (expected to contain %q); got: %v", known, err)
	}
}

// ---- L3 integration test helpers (shared httptest fixture + DB queries) ----

// loadModelsFixture reads the single shared models.dev-shaped fixture and derives
// the model counts from it. Counts are computed from the fixture itself (one
// source of truth) rather than hardcoded magic numbers.
func loadModelsFixture(t *testing.T) (raw []byte, total, anthropicCount int) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "models_dev.json"))
	if err != nil {
		t.Fatalf("read models fixture: %v", err)
	}
	var wire map[string]struct {
		Models map[string]json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("parse models fixture: %v", err)
	}
	for slug, prov := range wire {
		total += len(prov.Models)
		if slug == bestiary.ProviderAnthropic.String() {
			anthropicCount = len(prov.Models)
		}
	}
	return raw, total, anthropicCount
}

// firstAnthropicModelID returns a model ID known to exist under the anthropic
// provider in the shared fixture (lexicographically smallest, for determinism).
// Deriving it from the fixture avoids hardcoding a literal that would silently
// query a non-existent row — and pass a misleading assertion — if the fixture changes.
func firstAnthropicModelID(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "models_dev.json"))
	if err != nil {
		t.Fatalf("read models fixture: %v", err)
	}
	var wire map[string]struct {
		Models map[string]json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("parse models fixture: %v", err)
	}
	prov, ok := wire[bestiary.ProviderAnthropic.String()]
	if !ok || len(prov.Models) == 0 {
		t.Fatal("models fixture has no anthropic models")
	}
	best := ""
	for id := range prov.Models {
		if best == "" || id < best {
			best = id
		}
	}
	return best
}

// newModelsServer starts an httptest server serving body with status. A non-200
// status writes only the header, simulating an API outage.
func newModelsServer(t *testing.T, status int, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// modelsDBPath returns the analytics DB path under the given XDG data home.
func modelsDBPath(dataHome string) string {
	return filepath.Join(dataHome, string(defaults.AppName), "peasant.db")
}

// queryModelInt runs a single-int aggregate query against the analytics DB.
func queryModelInt(t *testing.T, dbPath, query string) int {
	t.Helper()
	pool, err := sqlitex.NewPool(dbPath, sqlitex.PoolOptions{})
	if err != nil {
		t.Fatalf("open analytics DB pool %s: %v", dbPath, err)
	}
	defer func() { _ = pool.Close() }()
	conn, err := pool.Take(context.Background())
	if err != nil {
		t.Fatalf("take conn: %v", err)
	}
	defer pool.Put(conn)
	var n int
	if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error { n = stmt.ColumnInt(0); return nil },
	}); err != nil {
		t.Fatalf("query %q against %s: %v", query, dbPath, err)
	}
	return n
}

// countModels returns the number of rows in the models table.
func countModels(t *testing.T, dbPath string) int {
	t.Helper()
	return queryModelInt(t, dbPath, "SELECT COUNT(*) FROM models")
}

// countEmptyLastSynced returns how many model rows have an empty/NULL last_synced.
func countEmptyLastSynced(t *testing.T, dbPath string) int {
	t.Helper()
	return queryModelInt(t, dbPath, "SELECT COUNT(*) FROM models WHERE last_synced IS NULL OR last_synced = ''")
}

// modelLastSynced returns the last_synced value for one (model_id, provider_key) row.
func modelLastSynced(t *testing.T, dbPath, modelID, providerKey string) string {
	t.Helper()
	pool, err := sqlitex.NewPool(dbPath, sqlitex.PoolOptions{})
	if err != nil {
		t.Fatalf("open analytics DB pool %s: %v", dbPath, err)
	}
	defer func() { _ = pool.Close() }()
	conn, err := pool.Take(context.Background())
	if err != nil {
		t.Fatalf("take conn: %v", err)
	}
	defer pool.Put(conn)
	var ts string
	if err := sqlitex.ExecuteTransient(conn,
		"SELECT last_synced FROM models WHERE model_id = ? AND provider_key = ?",
		&sqlitex.ExecOptions{
			Args:       []any{modelID, providerKey},
			ResultFunc: func(stmt *sqlite.Stmt) error { ts = stmt.ColumnText(0); return nil },
		}); err != nil {
		t.Fatalf("query last_synced for %s/%s: %v", modelID, providerKey, err)
	}
	return ts
}

// distinctKeyCount runs a bestiary slice through the production adapter and
// returns the number of distinct (model_id, provider_key) pairs. SyncModels is
// INSERT OR REPLACE keyed on (model_id, provider_key), so the persisted row count
// equals this distinct-key count — not the raw slice length.
func distinctKeyCount(bs []bestiary.ModelInfo) int {
	seen := map[[2]string]struct{}{}
	for _, m := range ingest.ModelsFromBestiary(bs, time.Now().UTC()) {
		seen[[2]string{m.ModelID, m.ProviderKey}] = struct{}{}
	}
	return len(seen)
}

// ---- L3 integration legs ----

// Leg 1: 200 round-trip — sync upserts every fixture model, each stamped with a
// fresh sync-time last_synced.
func TestModelsSync_200_RoundTrip(t *testing.T) {
	t.Parallel()
	dataHome := t.TempDir()

	raw, total, _ := loadModelsFixture(t)
	srv := newModelsServer(t, http.StatusOK, raw)
	fetcher := bestiary.NewClient(bestiary.WithBaseURL(srv.URL))

	before := time.Now().UTC().Add(-time.Minute)
	stdout, stderr, err := runModelsSync(t, context.Background(), dataHome, fetcher)
	if err != nil {
		t.Fatalf("sync: unexpected error: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	if stderr != "" {
		t.Errorf("200 round-trip must not warn; stderr: %s", stderr)
	}
	if !strings.Contains(stdout, fmt.Sprintf("%d model(s) synced", total)) {
		t.Errorf("stdout missing success line for %d models; got: %s", total, stdout)
	}

	dbPath := modelsDBPath(dataHome)
	if got := countModels(t, dbPath); got != total {
		t.Errorf("DB model count: got %d, want fixture total %d", got, total)
	}
	if n := countEmptyLastSynced(t, dbPath); n != 0 {
		t.Errorf("%d rows have empty last_synced; the live path must stamp every row", n)
	}
	// A live row carries a sync-time stamp (~now), not a static codegen vintage.
	ts := modelLastSynced(t, dbPath, firstAnthropicModelID(t), bestiary.ProviderAnthropic.String())
	parsed, perr := time.Parse(time.RFC3339, ts)
	if perr != nil {
		t.Fatalf("last_synced %q is not RFC3339: %v", ts, perr)
	}
	if parsed.Before(before) {
		t.Errorf("last_synced %q predates test start %q; expected a fresh sync-time stamp", ts, before)
	}
}

// Leg 2: 200 with --provider filter — DB holds only the fixture's anthropic subset.
func TestModelsSync_200_ProviderFilter(t *testing.T) {
	t.Parallel()
	dataHome := t.TempDir()

	raw, _, anthropicCount := loadModelsFixture(t)
	srv := newModelsServer(t, http.StatusOK, raw)
	fetcher := bestiary.NewClient(bestiary.WithBaseURL(srv.URL))

	_, stderr, err := runModelsSync(t, context.Background(), dataHome, fetcher,
		"--provider="+bestiary.ProviderAnthropic.String())
	if err != nil {
		t.Fatalf("sync --provider: unexpected error: %v", err)
	}
	if stderr != "" {
		t.Errorf("200 provider filter must not warn; stderr: %s", stderr)
	}
	if got := countModels(t, modelsDBPath(dataHome)); got != anthropicCount {
		t.Errorf("DB model count: got %d, want anthropic subset %d", got, anthropicCount)
	}
}

// Leg 3: --dry-run prints the count and writes nothing (the DB is never created).
func TestModelsSync_DryRun_WritesNothing(t *testing.T) {
	t.Parallel()
	dataHome := t.TempDir()

	raw, total, _ := loadModelsFixture(t)
	srv := newModelsServer(t, http.StatusOK, raw)
	fetcher := bestiary.NewClient(bestiary.WithBaseURL(srv.URL))

	stdout, _, err := runModelsSync(t, context.Background(), dataHome, fetcher, "--dry-run")
	if err != nil {
		t.Fatalf("dry-run sync: unexpected error: %v", err)
	}
	if !strings.Contains(stdout, fmt.Sprintf("%d model(s) would be synced (dry-run)", total)) {
		t.Errorf("dry-run stdout missing count for %d models; got: %s", total, stdout)
	}
	// Dry-run returns before openDB — the analytics DB must not exist.
	if _, statErr := os.Stat(modelsDBPath(dataHome)); !os.IsNotExist(statErr) {
		t.Errorf("dry-run must not write the DB; os.Stat err = %v (want IsNotExist)", statErr)
	}
}

// Leg 4: 500 → static fallback. Exit 0; stderr names the snapshot vintage; DB holds
// the full static snapshot with bestiary's codegen last_synced (NOT a faked "now").
func TestModelsSync_500_StaticFallback(t *testing.T) {
	t.Parallel()
	dataHome := t.TempDir()

	srv := newModelsServer(t, http.StatusInternalServerError, nil)
	// WithRetries(0): one attempt → immediate *ErrAPIUnavailable, no backoff waits.
	// Safe here because a 500 IS an API-unavailable error (not ctx cancellation).
	fetcher := bestiary.NewClient(bestiary.WithBaseURL(srv.URL), bestiary.WithRetries(0))

	stdout, stderr, err := runModelsSync(t, context.Background(), dataHome, fetcher)
	if err != nil {
		t.Fatalf("500 fallback must exit 0; got error: %v\nstderr: %s", err, stderr)
	}

	// Snapshot vintage read at runtime — never a hardcoded literal.
	vintage := bestiary.StaticModels()[0].LastSynced
	if vintage == "" {
		t.Fatal("precondition failed: bestiary.StaticModels()[0].LastSynced is empty")
	}
	if !strings.Contains(stderr, vintage) {
		t.Errorf("stderr warning must name snapshot vintage %q; got: %s", vintage, stderr)
	}

	want := distinctKeyCount(bestiary.StaticModels())
	if !strings.Contains(stdout, fmt.Sprintf("%d model(s) synced", want)) {
		t.Errorf("stdout missing success line for %d models; got: %s", want, stdout)
	}

	dbPath := modelsDBPath(dataHome)
	if got := countModels(t, dbPath); got != want {
		t.Errorf("DB count: got %d, want distinct static count %d", got, want)
	}
	// Rows preserve bestiary's codegen last_synced (preserve-if-present), not "now".
	src := bestiary.StaticModels()[0]
	gotTS := modelLastSynced(t, dbPath, string(src.ID), src.Provider.String())
	if gotTS != src.LastSynced {
		t.Errorf("row last_synced: got %q, want preserved codegen vintage %q", gotTS, src.LastSynced)
	}
}

// Leg 5: 500 + --provider — fallback filtered to the static anthropic subset.
func TestModelsSync_500_StaticFallback_ProviderFilter(t *testing.T) {
	t.Parallel()
	dataHome := t.TempDir()

	srv := newModelsServer(t, http.StatusInternalServerError, nil)
	fetcher := bestiary.NewClient(bestiary.WithBaseURL(srv.URL), bestiary.WithRetries(0))

	_, stderr, err := runModelsSync(t, context.Background(), dataHome, fetcher,
		"--provider="+bestiary.ProviderAnthropic.String())
	if err != nil {
		t.Fatalf("500 fallback (filtered) must exit 0; got error: %v", err)
	}
	if !strings.Contains(stderr, "static snapshot") {
		t.Errorf("expected static-fallback warning on stderr; got: %s", stderr)
	}
	want := distinctKeyCount(bestiary.ModelsByProvider(bestiary.ProviderAnthropic))
	if got := countModels(t, modelsDBPath(dataHome)); got != want {
		t.Errorf("DB count: got %d, want filtered static count %d", got, want)
	}
}

// Leg 6: canceled context (NOT *ErrAPIUnavailable) propagates non-zero and does
// NOT fall back to static data.
func TestModelsSync_CanceledContext_Propagates(t *testing.T) {
	t.Parallel()
	dataHome := t.TempDir()

	srv := newModelsServer(t, http.StatusOK, nil) // never successfully reached
	// DEFAULT retries (>= 1) is load-bearing: with a pre-canceled context the retry
	// loop returns the raw ctx.Err() (NOT wrapped in *ErrAPIUnavailable), so the
	// command propagates instead of falling back. Do NOT set WithRetries(0) here —
	// that would wrap ctx.Err in *ErrAPIUnavailable and wrongly trigger fallback.
	fetcher := bestiary.NewClient(bestiary.WithBaseURL(srv.URL))

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before execution

	_, stderr, err := runModelsSync(t, ctx, dataHome, fetcher)
	if err == nil {
		t.Fatal("canceled context must yield a non-zero error, got nil")
	}
	if !strings.Contains(err.Error(), "fetch models:") {
		t.Errorf("error should be wrapped as 'fetch models: ...'; got: %v", err)
	}
	// Must NOT fall back: no static-snapshot warning and nothing written.
	if strings.Contains(stderr, "static snapshot") {
		t.Errorf("canceled context must NOT fall back to static data; stderr: %s", stderr)
	}
	if _, statErr := os.Stat(modelsDBPath(dataHome)); !os.IsNotExist(statErr) {
		t.Errorf("canceled sync must not write the DB; os.Stat err = %v (want IsNotExist)", statErr)
	}
}

// TestModelsCmd_Help verifies that peasant models sync --help shows usage information.
func TestModelsCmd_Help(t *testing.T) {
	t.Parallel()
	output, err := executeModelsCmd(t, t.TempDir(), []string{"sync", "--help"})
	if err != nil {
		t.Fatalf("models sync --help: unexpected error: %v\noutput: %s", err, output)
	}

	// Verify key sections are present.
	if !strings.Contains(output, "sync") {
		t.Error("help output should mention 'sync'")
	}
	if !strings.Contains(output, "--provider") {
		t.Error("help output should mention '--provider' flag")
	}
	if !strings.Contains(output, "--dry-run") {
		t.Error("help output should mention '--dry-run' flag")
	}
}

// TestModelsCmd_SyncSubcommandExists verifies the sync subcommand is registered.
func TestModelsCmd_SyncSubcommandExists(t *testing.T) {
	t.Parallel()
	cmd := BuildModelsCommand()
	subCmd, _, err := cmd.Find([]string{"sync"})
	if err != nil {
		t.Fatalf("models sync subcommand not found: %v", err)
	}
	if subCmd.Name() != "sync" {
		t.Errorf("expected subcommand name 'sync', got %q", subCmd.Name())
	}
}
