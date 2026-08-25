//go:build e2e

package store

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"

	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/migrations/v39_legacy_fixture_source.yaml
var v39LegacyFixtureSourceBytes []byte

type v39LegacyFixtureSource struct {
	RequiredSeedNames []string `yaml:"required_seed_names"`
	Request           struct {
		SessionID  string `yaml:"session_id"`
		CommitHash string `yaml:"commit_hash"`
		Subject    string `yaml:"subject"`
		AuthorTime int64  `yaml:"author_time"`
		PushedAt   int64  `yaml:"pushed_at"`
	} `yaml:"request"`
	Seeds []struct {
		Name string `yaml:"name"`
		SQL  string `yaml:"sql"`
	} `yaml:"seeds"`
	FrozenMustLackColumn string `yaml:"frozen_must_lack_column"`
}

func decodeV39LegacyFixtureSource(source []byte) (v39LegacyFixtureSource, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(source))
	decoder.KnownFields(true)
	var fixture v39LegacyFixtureSource
	if err := decoder.Decode(&fixture); err != nil {
		return v39LegacyFixtureSource{}, fmt.Errorf("decode v39 legacy fixture source: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return v39LegacyFixtureSource{}, fmt.Errorf("v39 legacy fixture source must contain exactly one YAML document: %v", err)
	}
	seedNames := make(map[string]bool, len(fixture.Seeds))
	for _, seed := range fixture.Seeds {
		seedNames[seed.Name] = true
	}
	if err := testutil.RequireFixtureNames("v39 legacy fixture source", "seed", fixture.RequiredSeedNames, seedNames); err != nil {
		return v39LegacyFixtureSource{}, err
	}
	if fixture.Request.SessionID == "" || fixture.FrozenMustLackColumn == "" {
		return v39LegacyFixtureSource{}, errors.New("v39 legacy fixture source: request.session_id and frozen_must_lack_column must both be set")
	}
	return fixture, nil
}

// TestBuildV39E2EFixture_CopiesFromLatestSchemaSource proves the V39 fixture
// builder accepts an ingested source written by this build. The source carries
// every column the latest migration added; the destination is frozen before
// V40. Copying by the destination's own column list is what keeps the two in
// step, and this case fails the moment the builder copies every source column
// again.
func TestBuildV39E2EFixture_CopiesFromLatestSchemaSource(t *testing.T) {
	fixture, err := decodeV39LegacyFixtureSource(v39LegacyFixtureSourceBytes)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "ingested-latest.db")
	destination := filepath.Join(dir, "frozen-v39.db")

	latest, err := Open(source)
	if err != nil {
		t.Fatalf("open the ingested source at the latest schema: %v", err)
	}
	if err := latest.Close(); err != nil {
		t.Fatalf("close the ingested source: %v", err)
	}
	conn, err := sqlite.OpenConn(source, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatalf("reopen the ingested source: %v", err)
	}
	if got := upgradeUserVersion(t, conn); got != CurrentSchemaVersion() {
		t.Fatalf("ingested source user_version = %d, want the current schema %d", got, CurrentSchemaVersion())
	}
	if _, present := upgradeColumn(t, conn, "sessions", fixture.FrozenMustLackColumn); !present {
		t.Fatalf("ingested source lacks sessions.%s, so it does not carry the column this case exists to prove is dropped", fixture.FrozenMustLackColumn)
	}
	for _, seed := range fixture.Seeds {
		if err := sqlitex.ExecuteTransient(conn, seed.SQL, nil); err != nil {
			t.Fatalf("seed %q into the ingested source: %v", seed.Name, err)
		}
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close the seeded source: %v", err)
	}

	request := v39FixtureRequest{
		Destination:    destination,
		IngestedSource: source,
		SessionID:      fixture.Request.SessionID,
		CommitHash:     fixture.Request.CommitHash,
		Subject:        fixture.Request.Subject,
		AuthorTime:     fixture.Request.AuthorTime,
		PushedAt:       fixture.Request.PushedAt,
	}
	if err := request.validate(); err != nil {
		t.Fatalf("validate the fixture request: %v", err)
	}
	if err := buildV39E2EFixture(request); err != nil {
		t.Fatalf("build the V39 fixture from a latest-schema source: %v", err)
	}

	frozen, err := sqlite.OpenConn(destination, sqlite.OpenReadOnly)
	if err != nil {
		t.Fatalf("open the built V39 fixture: %v", err)
	}
	defer frozen.Close()
	if got := upgradeUserVersion(t, frozen); got != 39 {
		t.Fatalf("built fixture user_version = %d, want 39", got)
	}
	if _, present := upgradeColumn(t, frozen, "sessions", fixture.FrozenMustLackColumn); present {
		t.Fatalf("built fixture carries sessions.%s, so it is not frozen before the origin migration", fixture.FrozenMustLackColumn)
	}
	var copied int
	if err := sqlitex.ExecuteTransient(frozen, `SELECT COUNT(*) FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{fixture.Request.SessionID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			copied = stmt.ColumnInt(0)
			return nil
		},
	}); err != nil {
		t.Fatalf("count the copied session: %v", err)
	}
	if copied != 1 {
		t.Fatalf("copied sessions = %d, want 1", copied)
	}
}
