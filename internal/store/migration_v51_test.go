package store

import (
	"bytes"
	"context"
	_ "embed"
	"io"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite/sqlitemigration"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/migrations/v50_pi_harness.yaml
var migrationPiYAML []byte

//go:embed testdata/migrations/v50_pi_harness.manifest.yaml
var migrationPiManifest []byte

func TestMigrationV51PiPreservesCurrentStore(t *testing.T) {
	var f struct {
		Cases []struct {
			Name       string   `yaml:"name"`
			Seed       []string `yaml:"seed"`
			Assertions []struct {
				Query string `yaml:"query"`
				Want  string `yaml:"want"`
			} `yaml:"assertions"`
			Rejects []string `yaml:"rejects"`
		} `yaml:"cases"`
	}
	decode := func(raw []byte, dest any) {
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
	decode(migrationPiYAML, &f)
	var manifest struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	decode(migrationPiManifest, &manifest)
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
			pool, err := sqlitex.NewPool(filepath.Join(t.TempDir(), "upgrade.db"), sqlitex.PoolOptions{PoolSize: 1, PrepareConn: preparePragmas})
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			conn, err := pool.Take(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Put(conn)
			before := sqlitemigration.Schema{Migrations: dbSchema.Migrations[:50], MigrationOptions: dbSchema.MigrationOptions[:50]}
			if err := sqlitemigration.Migrate(ctx, conn, before); err != nil {
				t.Fatal(err)
			}
			for _, query := range c.Seed {
				if err := sqlitex.ExecuteTransient(conn, query, nil); err != nil {
					t.Fatalf("seed: %v", err)
				}
			}
			if err := sqlitemigration.Migrate(ctx, conn, dbSchema); err != nil {
				t.Fatal(err)
			}
			for _, a := range c.Assertions {
				if got := scalarText(t, conn, a.Query); got != a.Want {
					t.Fatalf("query %s = %q want %q", a.Query, got, a.Want)
				}
			}
			// Exact canonical membership is schema-owned, not a copied harness count.
			for _, h := range schema.Harnesses() {
				if err := sqlitex.ExecuteTransient(conn, `UPDATE sessions SET model_harness=? WHERE session_id='fixture-session'`, &sqlitex.ExecOptions{Args: []any{string(h)}}); err != nil {
					t.Fatalf("sessions rejected %s: %v", h, err)
				}
				if err := sqlitex.ExecuteTransient(conn, `INSERT INTO daily_summary_harness(date_utc,model_harness) VALUES ('2026-01-02',?)`, &sqlitex.ExecOptions{Args: []any{string(h)}}); err != nil {
					t.Fatalf("daily summary rejected %s: %v", h, err)
				}
			}
			for _, query := range c.Rejects {
				if err := sqlitex.ExecuteTransient(conn, query, nil); err == nil {
					t.Fatalf("closed constraint accepted %s", query)
				}
			}
		})
	}
}
