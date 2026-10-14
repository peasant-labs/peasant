package store

import (
	_ "embed"
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

//go:embed testdata/migration_v65.yaml
var claudeIdentityEvidenceYAML []byte

type claudeIdentityRecordFixture struct {
	SessionID    string `yaml:"sessionId"`
	UUID         string `yaml:"uuid"`
	ParentUUID   string `yaml:"parentUuid"`
	Type         string `yaml:"type"`
	Subtype      string `yaml:"subtype"`
	SpawnAgentID string `yaml:"spawnAgentId"`
	AgentID      string `yaml:"agentId"`
	Sidechain    *bool  `yaml:"isSidechain"`
}

type claudeIdentityEvidenceCase struct {
	Name             string                        `yaml:"name"`
	Scope            string                        `yaml:"scope"`
	ProofVersion     int                           `yaml:"proofVersion"`
	NativeAgentID    string                        `yaml:"nativeAgentId"`
	NativeAgentValid bool                          `yaml:"nativeAgentValid"`
	ContentDigest    string                        `yaml:"contentDigest"`
	RecordsJSON      string                        `yaml:"recordsJson"`
	Records          []claudeIdentityRecordFixture `yaml:"records"`
}

func (c claudeIdentityEvidenceCase) evidence() ingest.ClaudeTranscriptEvidence {
	e := ingest.ClaudeTranscriptEvidence{
		SourcePath: ingest.ResolvedPath("/synthetic/" + c.Name + ".jsonl"),
		Scope:      ingest.ClaudeEvidenceScope(c.Scope), ModTimeUnixNano: 1234, SizeBytes: 200,
		HasConversationRecord: true, Origin: sessionorigin.Unknown,
		Title: "synthetic title", Branch: "synthetic-branch", CWD: "/synthetic",
		Identity:     &ingest.ClaudeTeammateIdentity{Team: "synthetic-team", Name: "worker"},
		Spawns:       []ingest.ClaudeTeammateIdentity{{Team: "synthetic-team", Name: "child"}},
		ProofVersion: c.ProofVersion, NativeAgentID: c.NativeAgentID,
		NativeAgentValid: c.NativeAgentValid, ContentDigest: c.ContentDigest,
	}
	for _, r := range c.Records {
		e.Records = append(e.Records, ingest.ClaudeIdentityRecord{
			SessionID: r.SessionID, UUID: r.UUID, ParentUUID: r.ParentUUID, Type: r.Type, Subtype: r.Subtype, SpawnAgentID: r.SpawnAgentID, AgentID: r.AgentID, Sidechain: r.Sidechain,
		})
	}
	return e
}

type claudeIdentityMalformedCase struct {
	Name string `yaml:"name"`
	JSON string `yaml:"json"`
}

type claudeIdentityRejectedWrite struct {
	Name      string `yaml:"name"`
	SessionID string `yaml:"sessionId"`
}

type claudeIdentityEvidenceFixtures struct {
	RequiredNames              []string                      `yaml:"requiredNames"`
	RequiredMalformedNames     []string                      `yaml:"requiredMalformedNames"`
	Cases                      []claudeIdentityEvidenceCase  `yaml:"cases"`
	Malformed                  []claudeIdentityMalformedCase `yaml:"malformed"`
	RequiredRejectedWriteNames []string                      `yaml:"requiredRejectedWriteNames"`
	RejectedWrites             []claudeIdentityRejectedWrite `yaml:"rejectedWrites"`
}

func LoadClaudeIdentityEvidenceFixtures(t *testing.T) claudeIdentityEvidenceFixtures {
	t.Helper()
	var doc claudeIdentityEvidenceFixtures
	if err := yaml.Unmarshal(claudeIdentityEvidenceYAML, &doc); err != nil {
		t.Fatal(err)
	}
	var names, malformedNames []string
	for _, c := range doc.Cases {
		names = append(names, c.Name)
	}
	for _, c := range doc.Malformed {
		malformedNames = append(malformedNames, c.Name)
	}
	if err := validateRecoveryRequiredNames(doc.RequiredNames, names, "Claude identity evidence"); err != nil {
		t.Fatal(err)
	}
	if err := validateRecoveryRequiredNames(doc.RequiredMalformedNames, malformedNames, "malformed Claude identity evidence"); err != nil {
		t.Fatal(err)
	}
	var rejectedNames []string
	for _, c := range doc.RejectedWrites {
		rejectedNames = append(rejectedNames, c.Name)
	}
	if err := validateRecoveryRequiredNames(doc.RequiredRejectedWriteNames, rejectedNames, "rejected Claude identity writes"); err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestClaudeEvidenceRejectsRecordedIDsAndRollsBack(t *testing.T) {
	fixtures := LoadClaudeIdentityEvidenceFixtures(t)
	for _, c := range fixtures.RejectedWrites {
		t.Run(c.Name, func(t *testing.T) {
			s, err := Open(filepath.Join(t.TempDir(), "evidence.db"), WithPoolSize(2))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := s.Close(); err != nil {
					t.Error(err)
				}
			})
			seed := fixtures.Cases[0].evidence()
			if err := s.SaveClaudeEvidence(t.Context(), []ingest.ClaudeTranscriptEvidence{seed}, nil); err != nil {
				t.Fatal(err)
			}
			before, err := s.LoadClaudeEvidence(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			changed := seed
			changed.ContentDigest = "must-roll-back"
			invalid := fixtures.Cases[1].evidence()
			invalid.Records[0].SessionID = c.SessionID
			if err := s.SaveClaudeEvidence(t.Context(), []ingest.ClaudeTranscriptEvidence{changed, invalid}, []ingest.ResolvedPath{seed.SourcePath}); err == nil {
				t.Fatal("invalid recorded session ID persisted")
			}
			after, err := s.LoadClaudeEvidence(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatal("rejected identity write changed cache")
			}
			conn, err := s.pool.Take(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer s.pool.Put(conn)
			if err := sqlitex.ExecuteTransient(conn, `SELECT source_path FROM claude_transcript_evidence WHERE source_path=?`, &sqlitex.ExecOptions{Args: []any{invalid.SourcePath.String()}, ResultFunc: func(*sqlite.Stmt) error {
				t.Error("rejected row remains on disk despite being skipped on load")
				return nil
			}}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMigrationV65ClaudeEvidence(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "evidence.db")
	conn, err := sqlite.OpenConn(dbPath, sqlite.OpenReadWrite, sqlite.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlitemigration.Migrate(t.Context(), conn, frozenSchema(64)); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	const seed = `INSERT INTO claude_transcript_evidence
	(source_path, scope, mod_time_unix_nano, size_bytes, has_conversation, spawns_json, title, branch, cwd, origin)
	VALUES('/synthetic/legacy.jsonl', 'root', 7, 9, 1, '[]', 'old title', 'old branch', '/synthetic/old', 'unknown')`
	if err := sqlitex.ExecuteTransient(conn, seed, nil); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	// Open runs the real upgrade over the populated predecessor.
	s, err := Open(dbPath, WithPoolSize(2))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	loaded, err := s.LoadClaudeEvidence(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	legacy, ok := loaded["/synthetic/legacy.jsonl"]
	if !ok {
		t.Fatal("legacy cache row lost during upgrade")
	}
	wantLegacy := ingest.ClaudeTranscriptEvidence{
		SourcePath: "/synthetic/legacy.jsonl", Scope: ingest.ClaudeEvidenceScopeRoot,
		ModTimeUnixNano: 7, SizeBytes: 9, HasConversationRecord: true,
		Title: "old title", Branch: "old branch", CWD: "/synthetic/old", Origin: sessionorigin.Unknown,
	}
	if !reflect.DeepEqual(legacy, wantLegacy) {
		t.Fatalf("migration changed legacy evidence or fabricated proof:\ngot  %+v\nwant %+v", legacy, wantLegacy)
	}
	fixtures := LoadClaudeIdentityEvidenceFixtures(t)
	for _, c := range fixtures.Cases {
		t.Run(c.Name, func(t *testing.T) {
			want := c.evidence()
			if err := s.SaveClaudeEvidence(t.Context(), []ingest.ClaudeTranscriptEvidence{want}, nil); err != nil {
				t.Fatal(err)
			}
			loaded, err := s.LoadClaudeEvidence(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if got := loaded[want.SourcePath]; !reflect.DeepEqual(got, want) {
				t.Fatalf("round trip changed evidence:\ngot  %+v\nwant %+v", got, want)
			}
			conn, err := s.pool.Take(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer s.pool.Put(conn)
			if err := sqlitex.ExecuteTransient(conn, `SELECT identity_records_json FROM claude_transcript_evidence WHERE source_path=?`, &sqlitex.ExecOptions{
				Args: []any{want.SourcePath.String()}, ResultFunc: func(stmt *sqlite.Stmt) error {
					if got := stmt.ColumnText(0); got != c.RecordsJSON {
						t.Errorf("stored record JSON=%s, want %s", got, c.RecordsJSON)
					}
					return nil
				},
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, c := range fixtures.Malformed {
		t.Run(c.Name, func(t *testing.T) {
			conn, err := s.pool.Take(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			// Simulate corruption, including torn JSON which the write CHECK normally refuses.
			if err := sqlitex.ExecuteTransient(conn, `PRAGMA ignore_check_constraints=ON`, nil); err != nil {
				s.pool.Put(conn)
				t.Fatal(err)
			}
			err = sqlitex.ExecuteTransient(conn, `UPDATE claude_transcript_evidence SET identity_records_json=? WHERE source_path='/synthetic/legacy.jsonl'`, &sqlitex.ExecOptions{Args: []any{c.JSON}})
			resetErr := sqlitex.ExecuteTransient(conn, `PRAGMA ignore_check_constraints=OFF`, nil)
			s.pool.Put(conn)
			if err != nil {
				t.Fatal(err)
			}
			if resetErr != nil {
				t.Fatal(resetErr)
			}
			loaded, err := s.LoadClaudeEvidence(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := loaded[legacy.SourcePath]; ok {
				t.Fatal("malformed proof was trusted instead of skipped for re-mining")
			}
			for _, healthy := range fixtures.Cases {
				if !reflect.DeepEqual(loaded[healthy.evidence().SourcePath], healthy.evidence()) {
					t.Fatalf("corrupt row affected healthy fixture %q", healthy.Name)
				}
			}
		})
	}
	// A rejected update and delete must leave the full cache unchanged.
	before, err := s.LoadClaudeEvidence(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	invalid := fixtures.Cases[0].evidence()
	invalid.ProofVersion = -1
	changed := fixtures.Cases[1].evidence()
	changed.ContentDigest = "must-roll-back"
	if err := s.SaveClaudeEvidence(t.Context(), []ingest.ClaudeTranscriptEvidence{changed, invalid}, []ingest.ResolvedPath{changed.SourcePath}); err == nil {
		t.Fatal("negative proof version admitted")
	}
	after, err := s.LoadClaudeEvidence(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("failed write changed the cache")
	}
}
