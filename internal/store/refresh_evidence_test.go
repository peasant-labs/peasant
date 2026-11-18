package store

import (
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitemigration"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

func TestReadRefreshEvidenceBeforeProofColumns(t *testing.T) {
	const version = 54
	path := filepath.Join(t.TempDir(), "behind.db")
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite, sqlite.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlitemigration.Migrate(t.Context(), conn, frozenSchema(version)); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO projects(project_hash) VALUES ('project')`,
		`INSERT INTO host_slugs(opaque_id,host_slug) VALUES ('host','host')`,
		`INSERT INTO sessions(session_id,model_harness,model_id,opaque_host_id,project_hash,start_ms,end_ms,ingested_ms,source_path,source_format,schema_version,index_version) VALUES ('session','claude-code','model','host','project',1,2,3,'source','jsonl',1,7)`,
	} {
		if err := sqlitex.ExecuteTransient(conn, statement, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	before := fileHash(t, path)
	rows, gotVersion, err := ReadRefreshEvidence(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if gotVersion != version || len(rows) != 1 {
		t.Fatalf("read version %d with %d rows, want version %d with one row", gotVersion, len(rows), version)
	}
	if rows[0].IndexedInputHash != nil || rows[0].AdapterVersion != nil || rows[0].IndexerVersion != 7 {
		t.Fatalf("behind evidence = %+v; absent columns must remain unproven and existing stamps must be read", rows[0])
	}
	assertHashUnchanged(t, path, before, "ReadRefreshEvidence")
}
