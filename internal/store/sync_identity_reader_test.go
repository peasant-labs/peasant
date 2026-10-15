package store_test

import (
	_ "embed"
	"encoding/json"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

//go:embed testdata/push_identity.yaml
var pushIdentityYAML []byte

//go:embed testdata/push_identity.manifest.yaml
var pushIdentityManifestYAML []byte

type pushIdentityCase struct {
	Name             string `yaml:"name"`
	Kind             string `yaml:"kind"`
	State            string `yaml:"state"`
	Target           string `yaml:"target"`
	Snapshot         string `yaml:"snapshot"`
	Cache            string `yaml:"cache"`
	InactiveEvidence bool   `yaml:"inactiveEvidence"`
	ExpectParent     string `yaml:"expectParent"`
	ExpectUnresolved bool   `yaml:"expectUnresolved"`
}

func TestSyncParentIdentities(t *testing.T) {
	var fixture struct {
		Cases []pushIdentityCase `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(pushIdentityYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(pushIdentityManifestYAML, "push identity")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tc := range fixture.Cases {
		names = append(names, tc.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "push identity"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			s := openTestStore(t)
			ctx := t.Context()
			hash := strings.Repeat("a", 64)
			parent := makeStoreEntry(t, "sess_cacheparent", hash, "fixture-project", defaults.HarnessOpenCode, 1000, 2000, 3000)
			child := makeStoreEntry(t, "sess_child", hash, "fixture-project", defaults.HarnessOpenCode, 1000, 2000, 3000)
			if err := s.InsertSessions(ctx, []ingest.StoreEntry{parent, child}); err != nil {
				t.Fatal(err)
			}
			conn := takeConn(t, s.PoolForTest())
			defer func() {
				if conn != nil {
					s.PoolForTest().Put(conn)
				}
			}()
			exec := func(query string, args ...any) {
				t.Helper()
				if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{Args: args}); err != nil {
					t.Fatal(err)
				}
			}
			exec(`INSERT OR IGNORE INTO session_metrics(session_id) VALUES ('sess_child')`)
			exec(`DELETE FROM session_metrics WHERE session_id='sess_cacheparent'`)
			exec(`UPDATE sessions SET active_generation_id='active', parent_id=NULLIF(?, '') WHERE session_id='sess_child'`, tc.Cache)
			if tc.State != "" {
				kind := tc.Kind
				if kind == "" {
					kind = "started_by"
				}
				exec(`INSERT INTO session_relationship_evidence(session_id,generation_id,ordinal,kind,target_state,target_local_id,evidence) VALUES ('sess_child','active',0,?,?,NULLIF(?,''),'existing_adapter')`, kind, tc.State, tc.Target)
			}
			if tc.InactiveEvidence {
				exec(`INSERT INTO session_relationship_evidence(session_id,generation_id,ordinal,kind,target_state,target_local_id,evidence) VALUES ('sess_child','old-one',0,'started_by','explicit_none',NULL,'existing_adapter'), ('sess_child','old-two',0,'started_by','target_known','sess_staleparent','existing_adapter')`)
			}
			if tc.Snapshot != "" {
				metadata, err := json.Marshal(map[string]string{"parentUuid": tc.Snapshot})
				if err != nil {
					t.Fatal(err)
				}
				exec(`INSERT INTO session_publication_metadata(session_id,capture_revision,schema_version,metadata_json,metadata_hash,content_hash,captured_at) VALUES ('sess_child',1,1,?,?,?,1)`, string(metadata), hash, hash)
			}
			s.PoolForTest().Put(conn)
			conn = nil
			sessions, err := s.AllPushableSessions(ctx)
			if err != nil {
				t.Fatal(err)
			}
			rows, err := s.SyncParentIdentities(ctx, sessions)
			if err != nil {
				t.Fatal(err)
			}
			// Cardinality is observed on raw rows before any caller's map collapse.
			if len(rows) != 1 {
				t.Fatalf("one pushable child expected exactly once, got %v", rows)
			}
			row := rows[0]
			if row.SessionID != "sess_child" || row.ParentIdentity != tc.ExpectParent || row.ParentUnresolved != tc.ExpectUnresolved {
				t.Fatalf("identity = %+v; want parent %q unresolved %t", row, tc.ExpectParent, tc.ExpectUnresolved)
			}
		})
	}
}
