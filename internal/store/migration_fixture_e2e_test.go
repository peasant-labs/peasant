//go:build e2e

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite/sqlitemigration"
	"zombiezen.com/go/sqlite/sqlitex"
)

const v39FixtureRequestEnv = "PEASANT_E2E_V39_FIXTURE"

type v39FixtureRequest struct {
	Destination    string `json:"destination"`
	IngestedSource string `json:"ingestedSource"`
	SessionID      string `json:"sessionID"`
	CommitHash     string `json:"commitHash"`
	Subject        string `json:"subject"`
	AuthorTime     int64  `json:"authorTime"`
	PushedAt       int64  `json:"pushedAt"`
}

// TestBuildV39E2EFixture is invoked by the E2E package through a separate test
// process so the fixture builder can keep the V1-V39 migration schema private to
// the store package.
func TestBuildV39E2EFixture(t *testing.T) {
	raw := os.Getenv(v39FixtureRequestEnv)
	if raw == "" {
		t.Skip("V39 fixture builder is invoked only by the association E2E")
	}

	var request v39FixtureRequest
	if err := json.Unmarshal([]byte(raw), &request); err != nil {
		t.Fatalf("decode V39 fixture request: %v", err)
	}
	if err := request.validate(); err != nil {
		t.Fatalf("validate V39 fixture request: %v", err)
	}
	if err := buildV39E2EFixture(request); err != nil {
		t.Fatalf("build V39 E2E fixture: %v", err)
	}
}

func (r v39FixtureRequest) validate() error {
	if strings.TrimSpace(r.Destination) == "" || strings.TrimSpace(r.IngestedSource) == "" {
		return fmt.Errorf("destination and ingested source database paths are required")
	}
	if _, err := ingest.NewSessionID(r.SessionID); err != nil {
		return fmt.Errorf("session ID %q is invalid: %w", r.SessionID, err)
	}
	if strings.TrimSpace(r.CommitHash) == "" || strings.TrimSpace(r.Subject) == "" {
		return fmt.Errorf("observed commit hash and subject are required")
	}
	if r.AuthorTime <= 0 || r.PushedAt <= r.AuthorTime {
		return fmt.Errorf("author time and later pushed time are required, got author_time=%d pushed_at=%d", r.AuthorTime, r.PushedAt)
	}
	if _, err := os.Stat(r.IngestedSource); err != nil {
		return fmt.Errorf("inspect ingested source database %q: %w", r.IngestedSource, err)
	}
	return nil
}

func buildV39E2EFixture(request v39FixtureRequest) error {
	pool, err := sqlitex.NewPool(request.Destination, sqlitex.PoolOptions{PoolSize: 1, PrepareConn: preparePragmas})
	if err != nil {
		return fmt.Errorf("open destination pool: %w", err)
	}
	defer pool.Close()

	ctx := context.Background()
	conn, err := pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("take destination connection: %w", err)
	}
	defer pool.Put(conn)

	v39 := dbSchema
	v39.Migrations = dbSchema.Migrations[:39]
	v39.MigrationOptions = dbSchema.MigrationOptions[:39]
	if err := sqlitemigration.Migrate(ctx, conn, v39); err != nil {
		return fmt.Errorf("apply production V1-V39 migrations: %w", err)
	}
	if err := sqlitex.ExecuteTransient(conn, "ATTACH DATABASE ? AS ingested", &sqlitex.ExecOptions{Args: []any{request.IngestedSource}}); err != nil {
		return fmt.Errorf("attach ingested fixture database: %w", err)
	}
	defer func() { _ = sqlitex.ExecuteTransient(conn, "DETACH DATABASE ingested", nil) }()

	for _, table := range []string{"projects", "host_slugs", "sessions", "session_entries", "session_metrics"} {
		statement := fmt.Sprintf("INSERT INTO main.%s SELECT * FROM ingested.%s", table, table)
		if err := sqlitex.ExecuteTransient(conn, statement, nil); err != nil {
			return fmt.Errorf("copy %s rows from committed ingest fixture: %w", table, err)
		}
	}
	if err := sqlitex.ExecuteTransient(conn, `
INSERT INTO session_commits (session_id, commit_hash, message, author_time)
VALUES (?, ?, ?, ?)`, &sqlitex.ExecOptions{Args: []any{request.SessionID, request.CommitHash, request.Subject, request.AuthorTime}}); err != nil {
		return fmt.Errorf("seed observed commit before the V40 association ledger exists: %w", err)
	}
	if conn.Changes() != 1 {
		return fmt.Errorf("seed observed commit changed %d rows, want exactly 1", conn.Changes())
	}
	if err := sqlitex.ExecuteTransient(conn, `
UPDATE sessions
SET pushed_at = ?, license_id = ?
WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{request.PushedAt, string(schema.LicenseCCBY), request.SessionID}}); err != nil {
		return fmt.Errorf("seed already-pushed V39 session: %w", err)
	}
	if conn.Changes() != 1 {
		return fmt.Errorf("seed already-pushed V39 session changed %d rows, want exactly 1", conn.Changes())
	}
	return nil
}
