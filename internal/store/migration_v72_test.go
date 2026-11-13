package store

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitemigration"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/migrations/v71_identity_facts.yaml
var v71IdentityFactsYAML []byte

type v71Record struct {
	SessionID    string `yaml:"sessionId" json:"sessionId"`
	UUID         string `yaml:"uuid" json:"uuid,omitempty"`
	ParentUUID   string `yaml:"parentUuid" json:"parentUuid,omitempty"`
	Type         string `yaml:"type" json:"type,omitempty"`
	Subtype      string `yaml:"subtype" json:"subtype,omitempty"`
	SpawnAgentID string `yaml:"spawnAgentId" json:"spawnAgentId,omitempty"`
	AgentID      string `yaml:"agentId" json:"agentId,omitempty"`
	Sidechain    *bool  `yaml:"isSidechain" json:"isSidechain,omitempty"`
}

type v71Row struct {
	Name              string      `yaml:"name"`
	Scope             string      `yaml:"scope"`
	ProofVersion      int         `yaml:"proofVersion"`
	NativeAgentID     string      `yaml:"nativeAgentId"`
	NativeAgentValid  bool        `yaml:"nativeAgentValid"`
	Records           []v71Record `yaml:"records"`
	RecordsJSON       string      `yaml:"recordsJSON"`
	WantChain         bool        `yaml:"wantChain"`
	WantLegacy        bool        `yaml:"wantLegacy"`
	WantContradictory bool        `yaml:"wantContradictory"`
	WantProofVersion  int         `yaml:"wantProofVersion"`
}

type v71Fixture struct {
	RequiredNames []string `yaml:"requiredNames"`
	Rows          []v71Row `yaml:"rows"`
}

func TestMigrationV72CompactIdentityFacts(t *testing.T) {
	var fixture v71Fixture
	decoder := yaml.NewDecoder(bytes.NewReader(v71IdentityFactsYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]struct{}, len(fixture.RequiredNames))
	for _, name := range fixture.RequiredNames {
		if name == "" {
			t.Fatal("v71 fixture declares a blank required name")
		}
		if _, duplicate := names[name]; duplicate {
			t.Fatalf("v71 fixture repeats required name %q", name)
		}
		names[name] = struct{}{}
	}
	if len(names) != len(fixture.Rows) {
		t.Fatalf("v71 fixture has %d rows and %d required names", len(fixture.Rows), len(names))
	}
	for _, row := range fixture.Rows {
		if _, ok := names[row.Name]; !ok {
			t.Fatalf("v71 row %q is not in the required name list", row.Name)
		}
		delete(names, row.Name)
	}
	for name := range names {
		t.Errorf("required v71 row %q has no fixture", name)
	}

	dbPath := filepath.Join(t.TempDir(), "identity-facts.db")
	conn, err := sqlite.OpenConn(dbPath, sqlite.OpenReadWrite, sqlite.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlitemigration.Migrate(t.Context(), conn, frozenSchema(71)); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	for _, row := range fixture.Rows {
		raw := row.RecordsJSON
		if raw == "" {
			encoded, encodeErr := json.Marshal(row.Records)
			if encodeErr != nil {
				conn.Close()
				t.Fatal(encodeErr)
			}
			raw = string(encoded)
		}
		err = sqlitex.ExecuteTransient(conn, `INSERT INTO claude_transcript_evidence
			(source_path, scope, mod_time_unix_nano, size_bytes, has_conversation, spawns_json, title, branch, cwd, origin,
			 proof_version, native_agent_id, native_agent_valid, content_digest, identity_records_json)
			VALUES (?, ?, 1, 1, 1, '[]', '', '', '', 'unknown', ?, ?, ?, '', ?)`, &sqlitex.ExecOptions{
			Args: []any{"/synthetic/" + row.Name + ".jsonl", row.Scope, row.ProofVersion, row.NativeAgentID, boolToInt(row.NativeAgentValid), raw},
		})
		if err != nil {
			conn.Close()
			t.Fatalf("seed %s: %v", row.Name, err)
		}
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if got := CurrentSchemaVersion(); got != 79 {
		t.Fatalf("CurrentSchemaVersion() = %d, want 79", got)
	}
	store, err := Open(dbPath, WithPoolSize(1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	conn, err = store.pool.Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer store.pool.Put(conn)
	for _, row := range fixture.Rows {
		t.Run(row.Name, func(t *testing.T) {
			var gotProof int
			var gotRecords, gotSessions, gotSpawns, gotChain string
			var gotLegacy, gotContradictory int
			err := sqlitex.ExecuteTransient(conn, `SELECT proof_version, identity_records_json, identity_session_ids_json,
				identity_spawn_agent_ids_json, identity_legacy_agent_proof, identity_contradictory_sidechain, identity_chain_json
				FROM claude_transcript_evidence WHERE source_path = ?`, &sqlitex.ExecOptions{
				Args: []any{"/synthetic/" + row.Name + ".jsonl"},
				ResultFunc: func(stmt *sqlite.Stmt) error {
					gotProof = stmt.ColumnInt(0)
					gotRecords = stmt.ColumnText(1)
					gotSessions = stmt.ColumnText(2)
					gotSpawns = stmt.ColumnText(3)
					gotLegacy = stmt.ColumnInt(4)
					gotContradictory = stmt.ColumnInt(5)
					gotChain = stmt.ColumnText(6)
					return nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if gotRecords != "[]" {
				t.Fatalf("identity_records_json = %s, want []", gotRecords)
			}
			wantProof := row.ProofVersion
			if row.RecordsJSON != "" {
				wantProof = row.WantProofVersion
			}
			if gotProof != wantProof {
				t.Fatalf("proof_version = %d, want %d", gotProof, wantProof)
			}
			if row.RecordsJSON != "" {
				if gotSessions != "[]" || gotSpawns != "[]" || gotChain != "[]" || gotLegacy != 0 || gotContradictory != 0 {
					t.Fatalf("unparseable row was backfilled: sessions=%s spawns=%s chain=%s legacy=%d contradictory=%d", gotSessions, gotSpawns, gotChain, gotLegacy, gotContradictory)
				}
				return
			}
			evidence := ingest.ClaudeTranscriptEvidence{
				Scope: ingest.ClaudeEvidenceScope(row.Scope), NativeAgentID: row.NativeAgentID,
				Origin: sessionorigin.Unknown,
			}
			for _, record := range row.Records {
				evidence.Records = append(evidence.Records, ingest.ClaudeIdentityRecord{
					SessionID: record.SessionID, UUID: record.UUID, ParentUUID: record.ParentUUID,
					Type: record.Type, Subtype: record.Subtype, SpawnAgentID: record.SpawnAgentID,
					AgentID: record.AgentID, Sidechain: record.Sidechain,
				})
			}
			ingest.SealClaudeIdentity(&evidence)
			sessions, err := json.Marshal(evidence.SessionIDs)
			if err != nil {
				t.Fatal(err)
			}
			spawns, err := json.Marshal(evidence.SpawnAgentIDs)
			if err != nil {
				t.Fatal(err)
			}
			if gotSessions != string(sessions) {
				t.Fatalf("session ids = %s, want %s", gotSessions, sessions)
			}
			if gotSpawns != string(spawns) {
				t.Fatalf("spawn ids = %s, want %s", gotSpawns, spawns)
			}
			if (gotLegacy == 1) != evidence.LegacyAgentProof {
				t.Fatalf("legacy proof = %d, sealed = %t", gotLegacy, evidence.LegacyAgentProof)
			}
			if row.WantLegacy && gotLegacy != 1 {
				t.Fatalf("legacy proof = %d, want 1", gotLegacy)
			}
			if row.WantContradictory && gotContradictory != 1 {
				t.Fatalf("contradictory sidechain = %d, want 1", gotContradictory)
			}
			if (gotContradictory == 1) != evidence.ContradictorySidechain {
				t.Fatalf("contradictory sidechain = %d, sealed = %t", gotContradictory, evidence.ContradictorySidechain)
			}
			wantChain := "[]"
			if row.WantChain {
				encoded, encodeErr := json.Marshal(evidence.Chain)
				if encodeErr != nil {
					t.Fatal(encodeErr)
				}
				wantChain = string(encoded)
			}
			if gotChain != wantChain {
				t.Fatalf("chain = %s, want %s", gotChain, wantChain)
			}
			if row.WantChain && reflect.DeepEqual(evidence.Chain, []ingest.ClaudeIdentityChainLink{}) {
				t.Fatal("root row sealed an empty chain; the fixture records were not kept")
			}
		})
	}
}
