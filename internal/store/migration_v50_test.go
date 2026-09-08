package store

import (
	"context"
	_ "embed"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitemigration"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/publication-migration.yaml
var publicationMigrationYAML []byte

type publicationMigrationFixture struct {
	AcceptedKinds []string `yaml:"accepted_kinds"`
	LegacySeed    string   `yaml:"legacy_seed"`
	Constraints   []struct {
		Name string `yaml:"name"`
		SQL  string `yaml:"sql"`
	} `yaml:"constraints"`
}

func TestMigrationV50UpgradesLegacyWithoutInventingMetadata(t *testing.T) {
	t.Parallel()
	var fixture publicationMigrationFixture
	if err := yaml.Unmarshal(publicationMigrationYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, tc := range fixture.Constraints {
		if seen[tc.Name] {
			t.Fatalf("duplicate fixture %s", tc.Name)
		}
		seen[tc.Name] = true
	}
	for _, name := range strings.Fields("unknown-cwd-kind negative-capture negative-indexed-capture malformed-json uppercase-digest zero-capture zero-schema zero-capture-time missing-session") {
		if !seen[name] {
			t.Fatalf("missing fixture %s", name)
		}
	}
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	conn, err := sqlite.OpenConn(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = preparePragmas(conn); err != nil {
		t.Fatal(err)
	}
	frozen := sqlitemigration.Schema{Migrations: dbSchema.Migrations[:49], MigrationOptions: dbSchema.MigrationOptions[:49]}
	if err = sqlitemigration.Migrate(ctx, conn, frozen); err != nil {
		t.Fatal(err)
	}
	if err = sqlitex.ExecuteScript(conn, fixture.LegacySeed, nil); err != nil {
		t.Fatal(err)
	}
	if err = conn.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path, WithPoolSize(1))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id, err := ingest.NewSessionID("11111111-1111-4111-8111-111111111111")
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := s.LoadPublicationInput(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Readiness != ingest.PublicationNeedsIngest || bundle.CaptureRevision != 0 || bundle.Metadata.SessionID != "" {
		t.Fatalf("legacy capture invented: %+v", bundle)
	}
	conn, err = s.pool.Take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	if got := upgradeUserVersion(t, conn); got != CurrentSchemaVersion() {
		t.Fatalf("schema version %d", got)
	}
	for _, tc := range fixture.Constraints {
		t.Run(tc.Name, func(t *testing.T) {
			if err := sqlitex.ExecuteTransient(conn, tc.SQL, nil); err == nil {
				t.Fatal("invalid publication metadata accepted")
			}
		})
	}
	// Accept exactly the public provenance menu; its complete membership must
	// remain aligned with the migration's CHECK (not merely the menu's size).
	wantKinds := map[ingest.CWDProvenanceKind]bool{ingest.CWDSourceExact: false, ingest.CWDSourceAbsent: false, ingest.CWDSourceWorkspace: false, ingest.CWDSourceWorktree: false, ingest.CWDNotRecovered: false}
	for _, kind := range fixture.AcceptedKinds {
		if _, err := ingest.NewCWDProvenanceKind(kind); err != nil {
			t.Fatal(err)
		}
		parsed, _ := ingest.NewCWDProvenanceKind(kind)
		if seen, ok := wantKinds[parsed]; !ok || seen {
			t.Fatalf("unexpected/duplicate provenance %s", kind)
		}
		wantKinds[parsed] = true
		if err := sqlitex.ExecuteTransient(conn, `UPDATE sessions SET cwd_provenance_kind=?`, &sqlitex.ExecOptions{Args: []any{kind}}); err != nil {
			t.Fatal(err)
		}
	}
	for kind, seen := range wantKinds {
		if !seen {
			t.Fatalf("missing accepted provenance %s", kind)
		}
	}
}
