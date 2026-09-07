package store

import (
	"bytes"
	_ "embed"
	"io"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/migrations/v39_replay_seed.yaml
var migrationV39ReplaySeedYAML []byte

func seedMigrationV39ReplaySession(t *testing.T, db *Store, row migrationV40SessionFixture) {
	t.Helper()
	var fixture struct {
		RequiredNames []string          `yaml:"requiredNames"`
		Statements    map[string]string `yaml:"statements"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(migrationV39ReplaySeedYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatal("frozen replay seed requires exactly one YAML document")
	}
	required := []string{"project", "host", "session", "metrics"}
	if !reflect.DeepEqual(fixture.RequiredNames, required) {
		t.Fatal("frozen replay seed manifest changed")
	}
	for _, name := range required {
		if fixture.Statements[name] == "" {
			t.Fatalf("missing frozen seed statement %q", name)
		}
	}
	entry := migrationV40StoreEntry(t, row)
	meta := entry.Metadata
	conn, err := db.PoolForTest().Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer db.PoolForTest().Put(conn)
	if err := sqlitex.ExecuteTransient(conn, fixture.Statements["project"], &sqlitex.ExecOptions{Args: []any{string(meta.Project.Hash), meta.Project.FilePath}}); err != nil {
		t.Fatal(err)
	}
	// A stable fixture-local opaque key suffices for the frozen FK relationship;
	// no producer implementation or installation salt is involved in this seed.
	hostID := "replay-fixture-" + string(meta.HostSlug)
	if err := sqlitex.ExecuteTransient(conn, fixture.Statements["host"], &sqlitex.ExecOptions{Args: []any{hostID, string(meta.HostSlug)}}); err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteTransient(conn, fixture.Statements["session"], &sqlitex.ExecOptions{Args: []any{
		string(meta.SessionID), string(meta.ModelHarness), string(meta.Model), hostID, string(meta.Project.Hash),
		meta.Timestamp.Start, meta.Timestamp.End, *meta.Timestamp.Ingested, meta.Source.FilePath, string(meta.Source.Format), 9, meta.Version,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteTransient(conn, fixture.Statements["metrics"], &sqlitex.ExecOptions{Args: []any{string(meta.SessionID), meta.Stats.TurnCount, meta.Stats.TokensIn, meta.Stats.TokensOut, float64(meta.Stats.DurationMs) / 60000}}); err != nil {
		t.Fatal(err)
	}
}
