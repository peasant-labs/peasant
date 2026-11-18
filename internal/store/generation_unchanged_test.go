package store

import (
	"context"
	_ "embed"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/managed_generation_unchanged_proof.yaml
var unchangedProofYAML []byte

//go:embed testdata/managed_generation_unchanged_proof.manifest.yaml
var unchangedProofManifestYAML []byte

type unchangedProofCase struct {
	Name     string `yaml:"name"`
	Mutation string `yaml:"mutation"`
	Mismatch string `yaml:"mismatch"`
}

func LoadUnchangedProofFixtures(t *testing.T) []unchangedProofCase {
	t.Helper()
	var f struct {
		Cases []unchangedProofCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(unchangedProofYAML, &f); err != nil {
		t.Fatal(err)
	}
	required, err := decodeRecoveryRequiredNames(unchangedProofManifestYAML)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range f.Cases {
		names = append(names, c.Name)
	}
	if err = validateRecoveryRequiredNames(required, names, "unchanged proof"); err != nil {
		t.Fatal(err)
	}
	return f.Cases
}

func unchangedProofSetup(t *testing.T) (*Store, ManagedGenerationUnchangedProof) {
	t.Helper()
	sid, err := schema.NewSessionID("d4d4d4d4-d4d4-44d4-84d4-d4d4d4d4d4d4")
	if err != nil {
		t.Fatal(err)
	}
	s, _ := openGenerationStoreWith(t, WithPoolSize(1), WithWALAutocheckpointDisabled())
	seedGenerationSession(t, s, string(sid))
	v, blobs := buildTestGeneration(t, sid, "unchanged_base", "body", "mutation-input", "output")
	v.Generation.Metadata.ContentHash = strings.Repeat("b", 64)
	revision := 3
	v.Generation.Metadata.AdapterVersion = &revision
	v = filledCandidateForValidation(t, v, blobs)
	_, err = s.ActivateGeneration(context.Background(), GenerationActivation{Generation: v, Blobs: blobs, IndexerVersion: 1, IndexedAtMs: 100})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	err = sqlitex.Execute(conn, `UPDATE sessions SET indexed_input_hash=? WHERE session_id=?`, &sqlitex.ExecOptions{Args: []any{skipInputHash, string(sid)}})
	s.pool.Put(conn)
	if err != nil {
		t.Fatal(err)
	}
	state, err := s.ReadIndexState(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	return s, ManagedGenerationUnchangedProof{SessionID: sid, GenerationID: v.Generation.ID, Metadata: v.Generation.Metadata, InputDigest: skipInputHash, IndexerTarget: 1, FormatTarget: 2, Expected: state, UpdatedAtMs: 200}
}

func TestManagedGenerationUnchangedSettle(t *testing.T) {
	s, p := unchangedProofSetup(t)
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	reads := map[string]int{}
	err = conn.SetAuthorizer(sqlite.AuthorizeFunc(func(action sqlite.Action) sqlite.AuthResult {
		if action.Type() == sqlite.OpRead {
			switch action.Table() {
			case "session_generation_entries", "session_generation_title_refs", "session_generation_content":
				reads[action.Table()]++
			}
		}
		return sqlite.AuthResultOK
	}))
	s.pool.Put(conn)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := s.ProveManagedGenerationUnchanged(context.Background(), p)
	if err != nil || !proof.Proven {
		t.Fatalf("proof = %+v, error = %v", proof, err)
	}
	count := int64(42)
	p.Metadata.Stats.InputSubmissionCount = &count
	result, err := s.SettleManagedGenerationUnchanged(context.Background(), p)
	if err != nil || !result.Unchanged {
		t.Fatalf("settlement = %+v, error = %v", result, err)
	}
	if len(reads) != 0 {
		t.Fatalf("generation mapping reads: %v", reads)
	}
	stats, err := s.ReadCapturedStats(context.Background(), p.SessionID)
	if err != nil || stats.InputSubmissionCount == nil || *stats.InputSubmissionCount != count {
		t.Fatalf("stats = %+v, error = %v", stats, err)
	}
}

func TestManagedGenerationUnchangedSettleRefusesMovedPointer(t *testing.T) {
	s, p := unchangedProofSetup(t)
	proof, err := s.ProveManagedGenerationUnchanged(context.Background(), p)
	if err != nil || !proof.Proven {
		t.Fatalf("proof = %+v, error = %v", proof, err)
	}
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	err = sqlitex.Execute(conn, `UPDATE sessions SET active_generation_id=NULL WHERE session_id=?`, &sqlitex.ExecOptions{Args: []any{string(p.SessionID)}})
	s.pool.Put(conn)
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.SettleManagedGenerationUnchanged(context.Background(), p)
	if err == nil || result.Unchanged {
		t.Fatalf("moved generation settled: %+v, %v", result, err)
	}
}

func TestManagedGenerationUnchangedProof(t *testing.T) {
	for _, tc := range LoadUnchangedProofFixtures(t) {
		t.Run(tc.Name, func(t *testing.T) {
			s, p := unchangedProofSetup(t)
			switch tc.Mutation {
			case "mutation-none":
			case "adapter":
				n := 4
				p.Metadata.AdapterVersion = &n
			case "redaction-provenance":
				n := int64(123)
				p.Metadata.Redaction.RedactedAtMs = &n
				p.Metadata.Redaction.RuleSetVersion = "new"
			case "mutation-active":
				p.GenerationID = "different"
			case "mutation-unsupported", "latest-attempt":
				conn, err := s.pool.Take(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				query := `UPDATE sessions SET latest_attempt_generation_id=? WHERE session_id=?`
				id := p.GenerationID
				if tc.Mutation == "mutation-unsupported" {
					query = `UPDATE sessions SET active_generation_id=? WHERE session_id=?`
					id = "file_backed"
					err = sqlitex.Execute(conn, `INSERT INTO session_projection_generations(session_id,generation_id,metadata_json,title_refs_json,source_evidence_digest,completeness,index_format_version,installed_at_ms) VALUES(?,?,'{}','[]',?,'complete',2,1)`, &sqlitex.ExecOptions{Args: []any{string(p.SessionID), id, strings.Repeat("b", 64)}})
					if err != nil {
						t.Fatal(err)
					}
					p.GenerationID = id
				}
				err = sqlitex.Execute(conn, query, &sqlitex.ExecOptions{Args: []any{id, string(p.SessionID)}})
				s.pool.Put(conn)
				if err != nil {
					t.Fatal(err)
				}
			case "mutation-content":
				p.Metadata.ContentHash = strings.Repeat("c", 64)
			case "mutation-source-format":
				p.Metadata.Source.Format = schema.SourceFormatJSON
			case "mutation-harness":
				p.Metadata.ModelHarness = defaults.HarnessCodex
			case "mutation-metadata":
				p.Metadata.Project.Name = "changed"
			case "redaction":
				p.Metadata.Redaction.ContentHashAtRedact = "changed"
			case "indexer":
				p.IndexerTarget = 2
			case "mutation-input":
				p.InputDigest = strings.Repeat("d", 64)
			case "mutation-completeness", "mutation-source-evidence", "zero-metadata", "forged-metadata", "seal":
				conn, err := s.pool.Take(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if err = sqlitex.Execute(conn, `DROP TRIGGER session_generations_immutable`, nil); err != nil {
					t.Fatal(err)
				}
				field, value := "", ""
				switch tc.Mutation {
				case "mutation-completeness":
					field = "completeness"
					value = "incomplete_new"
				case "mutation-source-evidence":
					field = "source_evidence_digest"
					value = strings.Repeat("0", 64)
				case "zero-metadata":
					field = "metadata_hash"
					value = strings.Repeat("0", 64)
				case "forged-metadata":
					field = "metadata_hash"
					value = strings.Repeat("f", 64)
				case "seal":
					field = "candidate_digest"
					value = strings.Repeat("0", 64)
				}
				if err = sqlitex.Execute(conn, `UPDATE session_generations SET `+field+`=? WHERE session_id=?`, &sqlitex.ExecOptions{Args: []any{value, string(p.SessionID)}}); err != nil {
					t.Fatal(err)
				}
				s.pool.Put(conn)
			default:
				t.Fatalf("unknown mutation %q", tc.Mutation)
			}
			got, err := s.ProveManagedGenerationUnchanged(context.Background(), p)
			if err != nil {
				t.Fatal(err)
			}
			expected, err := ingest.NewUnchangedProofMismatch(tc.Mismatch)
			if err != nil {
				t.Fatal(err)
			}
			if got.Mismatch != expected || got.Proven != (expected == ingest.UnchangedMismatchNone) {
				t.Fatalf("proof = %+v, mismatch %q", got, expected)
			}
		})
	}
}
