package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// One file, opened by the real server/provider and independently by the CLI.
// A service-only comparison cannot observe route encoding or middleware drift.
func TestSessionsSearchHTTPParity(t *testing.T) {
	t.Parallel()
	family := loadSessionsSearchFixtures(t)
	dir := t.TempDir()
	db := openSeedStore(t, dir, "search parity")
	t.Cleanup(func() { db.Close() })
	seedSearchFixtures(t, db, family.Seeds)
	conn, err := db.Pool().Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var dbPath string
	err = sqlitex.ExecuteTransient(conn, "PRAGMA database_list", &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
		if stmt.ColumnText(1) == "main" {
			dbPath = stmt.ColumnText(2)
		}
		return nil
	}})
	db.Pool().Put(conn)
	if err != nil {
		t.Fatal(err)
	}
	if dbPath != string(defaults.ResolveDBFilePathWith(dir)) {
		t.Fatal("server and CLI must use the same database file")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	srv := api.NewServer(api.ServerConfig{Port: 0, Store: db, Config: config.BaseConfig(), Provider: api.NewStoreDataProvider(db, sessionvisibility.Browsing()), ConfigHome: t.TempDir()})
	if err := srv.Listen(ctx); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("server shutdown: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("server did not stop after cancellation")
		}
	})
	query := url.Values{"q": {family.Parity.Query}, "limit": {family.Parity.Limit}}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get("http://" + srv.Addr().String() + "/api/v1/search?" + query.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("search route returned %s", resp.Status)
	}
	var want schema.SearchPayload
	if err := json.NewDecoder(resp.Body).Decode(&want); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := executeWithDataDirStreams(t, BuildSessionsCommand(), dir, []string{"search", family.Parity.Query, "--limit", family.Parity.Limit, "--json"})
	if err != nil || stderr != "" {
		t.Fatalf("CLI: %v; stderr=%s", err, stderr)
	}
	var got sessionsSearchOutput
	if err := json.NewDecoder(strings.NewReader(stdout)).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(want.Results) < 2 {
		t.Fatal("parity must compare multiple ordered results, not an empty response")
	}
	if !reflect.DeepEqual(got.SearchPayload, want) {
		t.Fatalf("CLI payload: %+v\nHTTP payload: %+v", got.SearchPayload, want)
	}
}
