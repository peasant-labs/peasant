package store

import (
	_ "embed"
	"fmt"
	"path/filepath"
	"slices"
	"testing"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/refresh_boundaries.yaml
var refreshBoundaries []byte

func TestReadRefreshEvidenceVersionBoundaries(t *testing.T) {
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name             string
			Version, Indexer int
			Adapter, Proof   bool
		} `yaml:"cases"`
	}
	if err := yaml.Unmarshal(refreshBoundaries, &fixture); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range fixture.Cases {
		names = append(names, c.Name)
		t.Run(c.Name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "behind.db")
			conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite, sqlite.OpenCreate)
			if err != nil {
				t.Fatal(err)
			}
			columns := "session_id TEXT, model_harness TEXT, schema_version INTEGER"
			values := "'session', 'claude-code', 1"
			if c.Version >= 10 {
				columns += ", index_version INTEGER"
				values += ", 7"
			}
			if c.Version >= 55 {
				columns += ", adapter_version INTEGER"
				values += ", 8"
			}
			if c.Version >= 56 {
				columns += ", indexed_input_hash TEXT"
				values += ", 'proof'"
			}
			for _, sql := range []string{"CREATE TABLE sessions(" + columns + ")", "INSERT INTO sessions VALUES(" + values + ")", fmt.Sprintf("PRAGMA user_version = %d", c.Version)} {
				if err := sqlitex.ExecuteTransient(conn, sql, nil); err != nil {
					t.Fatal(err)
				}
			}
			if err := conn.Close(); err != nil {
				t.Fatal(err)
			}
			before := fileHash(t, path)
			rows, version, err := ReadRefreshEvidence(t.Context(), path)
			if err != nil {
				t.Fatal(err)
			}
			if version != c.Version || len(rows) != 1 {
				t.Fatalf("version %d rows %+v", version, rows)
			}
			row := rows[0]
			if row.IndexerVersion != c.Indexer || (row.AdapterVersion != nil) != c.Adapter || (row.IndexedInputHash != nil) != c.Proof {
				t.Fatalf("evidence = %+v", row)
			}
			if c.Adapter && *row.AdapterVersion != 8 {
				t.Fatalf("adapter = %d", *row.AdapterVersion)
			}
			if c.Proof && *row.IndexedInputHash != "proof" {
				t.Fatalf("proof = %q", *row.IndexedInputHash)
			}
			assertHashUnchanged(t, path, before, "ReadRefreshEvidence")
		})
	}
	for _, name := range fixture.RequiredNames {
		if !slices.Contains(names, name) {
			t.Errorf("missing fixture %s", name)
		}
	}
}
