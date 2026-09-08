package store

import (
	"bytes"
	"context"
	_ "embed"
	"io"
	"path/filepath"
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

// captureFormatPredecessorMigrations is the count of migrations that shipped
// before the capture-format rebuild. A database frozen there still carries the
// unconstrained capture_revision text column.
const captureFormatPredecessorMigrations = 58

type migrationCaptureFormatCase struct {
	Name           string   `yaml:"name"`
	MigrationFails bool     `yaml:"migrationFails"`
	Seed           []string `yaml:"seed"`
	Assertions     []struct {
		Query string `yaml:"query"`
		Want  string `yaml:"want"`
	} `yaml:"assertions"`
	Rejects []string `yaml:"rejects"`
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
	if captureFormatPredecessorMigrations+1 != CurrentSchemaVersion() {
		t.Fatalf("capture-format rebuild is no longer the newest migration: predecessor=%d current=%d", captureFormatPredecessorMigrations, CurrentSchemaVersion())
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
			if err := sqlitemigration.Migrate(ctx, conn, frozenSchema(captureFormatPredecessorMigrations)); err != nil {
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

			err = sqlitemigration.Migrate(ctx, conn, dbSchema)
			if c.MigrationFails {
				if err == nil {
					t.Fatal("an unmapped stored capture value was silently coerced instead of failing the migration")
				}
				if got := scalarText(t, conn, `PRAGMA user_version`); got != "58" {
					t.Fatalf("failed migration did not leave the predecessor schema intact: user_version=%q", got)
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
			// The stored set is exactly the canonical Go set: every member is
			// accepted, and every accepted member round-trips through the
			// boundary constructor.
			for _, format := range ingest.AllContentCaptureFormats() {
				if err := sqlitex.ExecuteTransient(conn, `UPDATE session_content_captures SET capture_format=? WHERE session_id='33333333-3333-4333-8333-333333333333'`, &sqlitex.ExecOptions{Args: []any{string(format)}}); err != nil {
					t.Fatalf("stored set rejected canonical format %s: %v", format, err)
				}
				stored := scalarText(t, conn, `SELECT capture_format FROM session_content_captures WHERE session_id='33333333-3333-4333-8333-333333333333'`)
				parsed, parseErr := ingest.NewContentCaptureFormat(stored)
				if parseErr != nil || parsed != format {
					t.Fatalf("stored %q did not round-trip to %s: %v", stored, format, parseErr)
				}
			}
			if _, err := ingest.NewContentCaptureFormat("not-a-capture-format"); err == nil {
				t.Fatal("boundary constructor cast an unknown capture format instead of failing")
			}
		})
	}
}
