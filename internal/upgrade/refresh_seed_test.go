package upgrade

import (
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// refreshSeed is one stored session row on a current-schema store. Versions
// are offsets from the claude-code managed-generation targets, so a case says
// "one behind" rather than restating a revision number. A nil adapter offset
// stores no adapter stamp.
type refreshSeed struct {
	ID            string
	Harness       ingest.Harness
	AdapterOffset *int
	IndexerOffset int
	MetadataOld   bool
	Proof         bool
}

// currentRefreshStore creates a current-schema store and seeds its rows.
func currentRefreshStore(t *testing.T, seeds ...refreshSeed) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "current.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	target := ingest.GenerationRepairTargets[ingest.HarnessClaudeCode]
	for _, seed := range seeds {
		metadata := int(ingest.CurrentSchemaVersion)
		if seed.MetadataOld {
			metadata = 1
		}
		var adapter, proof any
		if seed.AdapterOffset != nil {
			adapter = target.AdapterVersion + *seed.AdapterOffset
		}
		if seed.Proof {
			proof = "0000000000000000000000000000000000000000000000000000000000000000"
		}
		err := sqlitex.ExecuteTransient(conn, `INSERT INTO sessions(session_id,model_harness,model_id,opaque_host_id,project_hash,start_ms,end_ms,ingested_ms,source_path,source_format,schema_version,adapter_version,index_version,indexed_input_hash) VALUES (?, ?, 'model', 'host', 'project', 1, 2, 3, 'source', 'jsonl', ?, ?, ?, ?)`,
			&sqlitex.ExecOptions{Args: []any{seed.ID, string(seed.Harness), metadata, adapter, target.IndexerVersion + seed.IndexerOffset, proof}})
		if err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func offset(n int) *int { return &n }
