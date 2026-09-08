package store

import (
	"bytes"
	"context"
	_ "embed"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite/sqlitemigration"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/migrations/v59_capture_format.yaml
var migrationCaptureFormatYAML []byte

//go:embed testdata/migrations/v59_capture_format.manifest.yaml
var migrationCaptureFormatManifest []byte

type migrationCaptureFormatQuery struct {
	Query string `yaml:"query"`
	Want  string `yaml:"want"`
}

type migrationCaptureFormatCase struct {
	Name             string                        `yaml:"name"`
	MigrationFails   bool                          `yaml:"migrationFails"`
	Seed             []string                      `yaml:"seed"`
	Assertions       []migrationCaptureFormatQuery `yaml:"assertions"`
	FailedAssertions []migrationCaptureFormatQuery `yaml:"failedAssertions"`
	Rejects          []string                      `yaml:"rejects"`
	// FailedOpenMessage are the substrings a refusing upgrade must show the
	// user when the production store opens the unupgradable database.
	FailedOpenMessage []string `yaml:"failedOpenMessage"`
}

func decodeCaptureFormatYAML(t *testing.T, raw []byte, dest any) {
	t.Helper()
	d := yaml.NewDecoder(bytes.NewReader(raw))
	d.KnownFields(true)
	if err := d.Decode(dest); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := d.Decode(&trailing); err != io.EOF {
		t.Fatalf("trailing YAML: %v", err)
	}
}

func TestMigrationV59CaptureFormatClosesTheStoredSet(t *testing.T) {
	var f struct {
		Cases []migrationCaptureFormatCase `yaml:"cases"`
	}
	decodeCaptureFormatYAML(t, migrationCaptureFormatYAML, &f)
	var manifest struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	decodeCaptureFormatYAML(t, migrationCaptureFormatManifest, &manifest)
	names := make(map[string]bool)
	for _, c := range f.Cases {
		if c.Name == "" || names[c.Name] {
			t.Fatal("empty or duplicate migration scenario name")
		}
		names[c.Name] = true
	}
	for _, name := range manifest.RequiredNames {
		if !names[name] {
			t.Fatalf("missing migration scenario %q", name)
		}
		delete(names, name)
	}
	if len(names) > 0 {
		t.Fatal("unmanifested migration scenario")
	}
	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			ctx := context.Background()
			dbPath := filepath.Join(t.TempDir(), "capture-format.db")
			pool, err := sqlitex.NewPool(dbPath, sqlitex.PoolOptions{PoolSize: 1, PrepareConn: preparePragmas})
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			conn, err := pool.Take(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Put(conn)
			if err := sqlitemigration.Migrate(ctx, conn, frozenSchema(captureFormatPredecessorSchemaVersion)); err != nil {
				t.Fatalf("freeze predecessor schema: %v", err)
			}
			if got := scalarText(t, conn, `SELECT COUNT(*) FROM pragma_table_info('session_content_captures') WHERE name='capture_revision'`); got != "1" {
				t.Fatalf("predecessor schema does not carry the unconstrained capture_revision column: %q", got)
			}
			for _, query := range c.Seed {
				if err := sqlitex.ExecuteTransient(conn, query, nil); err != nil {
					t.Fatalf("seed %s: %v", query, err)
				}
			}

			// The migration under test is the frozen successor of the frozen
			// predecessor, so this stays a description of one migration when
			// later migrations ship.
			err = sqlitemigration.Migrate(ctx, conn, frozenSchema(captureFormatPredecessorSchemaVersion+1))
			if c.MigrationFails {
				if err == nil {
					t.Fatal("an unmapped stored capture value was silently coerced instead of failing the migration")
				}
				if got := scalarText(t, conn, `PRAGMA user_version`); got != strconv.Itoa(captureFormatPredecessorSchemaVersion) {
					t.Fatalf("failed migration did not leave the predecessor schema version intact: user_version=%q", got)
				}
				if len(c.FailedAssertions) == 0 {
					t.Fatal("a refusing migration case must prove what survived the rollback")
				}
				for _, a := range c.FailedAssertions {
					if got := scalarText(t, conn, a.Query); got != a.Want {
						t.Fatalf("rollback query %s = %q want %q", a.Query, got, a.Want)
					}
				}
				// The production entry point must say which row blocks the
				// upgrade and how to clear it, not just that a constraint failed.
				refused, openErr := Open(dbPath, WithPoolSize(1))
				if openErr == nil {
					_ = refused.Close()
					t.Fatal("the production store opened a database the capture-format upgrade cannot map")
				}
				if len(c.FailedOpenMessage) == 0 {
					t.Fatal("a refusing migration case must state what the user is told")
				}
				for _, want := range c.FailedOpenMessage {
					if !strings.Contains(openErr.Error(), want) {
						t.Fatalf("upgrade refusal does not mention %q: %v", want, openErr)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("migrate: %v", err)
			}
			for _, a := range c.Assertions {
				if got := scalarText(t, conn, a.Query); got != a.Want {
					t.Fatalf("query %s = %q want %q", a.Query, got, a.Want)
				}
			}
			for _, query := range c.Rejects {
				if err := sqlitex.ExecuteTransient(conn, query, nil); err == nil {
					t.Fatalf("closed constraint accepted %s", query)
				}
			}
			// The stored set is EXACTLY the canonical Go set, in both
			// directions. Accepting every member proves the CHECK is not
			// narrower; the rendered constraint text proves it is not wider,
			// which no list of hand-picked rejections can establish.
			for _, format := range ingest.AllContentCaptureFormats() {
				if err := sqlitex.ExecuteTransient(conn, `UPDATE session_content_captures SET capture_format=? WHERE session_id='33333333-3333-4333-8333-333333333333'`, &sqlitex.ExecOptions{Args: []any{string(format)}}); err != nil {
					t.Fatalf("stored set rejected canonical format %s: %v", format, err)
				}
			}
			quoted := make([]string, 0, len(ingest.AllContentCaptureFormats()))
			for _, format := range ingest.AllContentCaptureFormats() {
				quoted = append(quoted, "'"+format.String()+"'")
			}
			want := "capture_format TEXT NOT NULL CHECK(capture_format IN (" + strings.Join(quoted, ",") + "))"
			ddl := scalarText(t, conn, `SELECT sql FROM sqlite_master WHERE type='table' AND name='session_content_captures'`)
			if !strings.Contains(ddl, want) {
				t.Fatalf("stored capture-format set is not exactly the canonical set %s: %s", want, ddl)
			}
		})
	}
}

// The pre-migration guard and the migration must recognise the same tags, or a
// database would pass the guard and then fail inside the rebuild.
func TestCaptureFormatUpgradeMappingsMatchTheMigration(t *testing.T) {
	for _, m := range captureFormatUpgradeMappings() {
		arm := "WHEN '" + m.StoredTag + "' THEN '" + m.Format.String() + "'"
		if !strings.Contains(migrationV59, arm) {
			t.Fatalf("capture-format migration has no arm %s", arm)
		}
		if _, err := ingest.NewContentCaptureFormat(m.Format.String()); err != nil {
			t.Fatalf("capture-format mapping targets a format outside the canonical set: %v", err)
		}
	}
}
