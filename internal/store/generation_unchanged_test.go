package store

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testkit/wal"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/managed_generation_unchanged_proof.yaml
var unchangedProofYAML []byte

//go:embed testdata/managed_generation_unchanged_proof.manifest.yaml
var unchangedProofManifestYAML []byte

//go:embed testdata/unchanged_missing_counts.yaml
var unchangedMissingCountsYAML []byte

//go:embed testdata/unchanged_missing_counts.manifest.yaml
var unchangedMissingCountsManifestYAML []byte

type unchangedProofCase struct {
	Name     string `yaml:"name"`
	Mutation string `yaml:"mutation"`
	Mismatch string `yaml:"mismatch"`
}

type unchangedProofFixtures struct {
	Cases       []unchangedProofCase  `yaml:"cases"`
	SettleCases []unchangedSettleCase `yaml:"settleCases"`
	ReadColumns []string              `yaml:"readColumns"`
}

type unchangedSettleCase struct {
	Name            string   `yaml:"name"`
	MaxCommitFrames int      `yaml:"maxCommitFrames"`
	ChangedTables   []string `yaml:"changedTables"`
}

func LoadUnchangedProofFixtures(t *testing.T) unchangedProofFixtures {
	t.Helper()
	var f unchangedProofFixtures
	if err := yaml.Unmarshal(unchangedProofYAML, &f); err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		RequiredNames       []string `yaml:"requiredNames"`
		SettleRequiredNames []string `yaml:"settleRequiredNames"`
	}
	if err := yaml.Unmarshal(unchangedProofManifestYAML, &manifest); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range f.Cases {
		names = append(names, c.Name)
	}
	if err := validateRecoveryRequiredNames(manifest.RequiredNames, names, "unchanged proof"); err != nil {
		t.Fatal(err)
	}
	var settleNames []string
	for _, c := range f.SettleCases {
		settleNames = append(settleNames, c.Name)
	}
	if err := validateRecoveryRequiredNames(manifest.SettleRequiredNames, settleNames, "unchanged settlement"); err != nil {
		t.Fatal(err)
	}
	return f
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
	fixture := LoadUnchangedProofFixtures(t)
	var maxCommits int
	var changedTables []string
	for _, c := range fixture.SettleCases {
		switch c.Name {
		case "settle-commits-at-most-two-wal-frames":
			maxCommits = c.MaxCommitFrames
		case "settle-writes-only-stats-and-binding":
			changedTables = c.ChangedTables
		}
	}
	if maxCommits < 1 || len(changedTables) == 0 {
		t.Fatal("settlement fixture does not define its commit budget and changed tables")
	}
	s, p := unchangedProofSetup(t)
	prior, err := s.ReadCapturedStats(context.Background(), p.SessionID)
	if err != nil || prior.TurnCount == nil || prior.ConversationTurnCount == nil || prior.InputSubmissionCount == nil {
		t.Fatalf("initial parse counts = %+v, error = %v", prior, err)
	}
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// A refreshed publication capture is newer than the index binding and
	// the content capture; settle must move both bindings to this revision.
	err = sqlitex.Execute(conn, `INSERT INTO session_publication_metadata(session_id,capture_revision,schema_version,metadata_json,metadata_hash,content_hash,captured_at) VALUES(?,2,12,'{}',?,?,1)`, &sqlitex.ExecOptions{Args: []any{string(p.SessionID), strings.Repeat("a", 64), strings.Repeat("b", 64)}})
	if err != nil {
		t.Fatal(err)
	}
	err = sqlitex.Execute(conn, `UPDATE sessions SET cwd_provenance_kind='source_absent',publication_capture_revision=2,indexed_publication_capture_revision=1,adapter_version=4 WHERE session_id=?`, &sqlitex.ExecOptions{Args: []any{string(p.SessionID)}})
	if err != nil {
		t.Fatal(err)
	}
	err = sqlitex.Execute(conn, `INSERT INTO session_content_captures(session_id,status,source_authority,transcript_origin,capture_format,entry_count,content_row_count,full_capture_sha256,captured_at_ms,publication_capture_revision) VALUES(?,'complete','new_ingest',0,'full',0,0,?,1,1) ON CONFLICT(session_id) DO UPDATE SET publication_capture_revision=1`, &sqlitex.ExecOptions{Args: []any{string(p.SessionID), strings.Repeat("b", 64)}})
	if err != nil {
		t.Fatal(err)
	}
	var dbPath string
	if err = sqlitex.Execute(conn, `PRAGMA database_list`, &sqlitex.ExecOptions{ResultFunc: func(st *sqlite.Stmt) error {
		if st.ColumnText(1) == "main" {
			dbPath = st.ColumnText(2)
		}
		return nil
	}}); err != nil || dbPath == "" {
		t.Fatalf("database path = %q, error = %v", dbPath, err)
	}
	// Temporary triggers count actual affected rows, not table-size deltas.
	if err = sqlitex.Execute(conn, `CREATE TEMP TABLE observed_writes (table_name TEXT NOT NULL)`, nil); err != nil {
		t.Fatal(err)
	}
	var tables []string
	if err = sqlitex.Execute(conn, `SELECT name FROM pragma_table_list WHERE schema='main' AND type='table' AND name NOT LIKE 'sqlite_%'`, &sqlitex.ExecOptions{ResultFunc: func(st *sqlite.Stmt) error {
		tables = append(tables, st.ColumnText(0))
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	for _, table := range tables {
		for _, operation := range []string{"INSERT", "UPDATE", "DELETE"} {
			query := fmt.Sprintf("CREATE TEMP TRIGGER watch_%s_%s AFTER %s ON main.%s BEGIN INSERT INTO observed_writes VALUES ('%s'); END", table, operation, operation, table, table)
			if err = sqlitex.Execute(conn, query, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	s.pool.Put(conn)
	p.Expected, err = s.ReadIndexState(context.Background(), p.SessionID)
	if err != nil || p.Expected == nil || p.Expected.PublicationCaptureRevision != 2 || p.Expected.PublicationBound {
		t.Fatalf("pre-settle index state = %+v, error = %v", p.Expected, err)
	}
	conn, err = s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	reads := map[string]int{}
	accesses := map[string]bool{}
	err = conn.SetAuthorizer(sqlite.AuthorizeFunc(func(action sqlite.Action) sqlite.AuthResult {
		if action.Type() == sqlite.OpRead {
			accesses[action.Table()+"."+action.Column()] = true
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
	before, err := wal.CountWALCommitFrames(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := s.ProveManagedGenerationUnchanged(context.Background(), p)
	if err != nil || !proof.Proven {
		t.Fatalf("proof = %+v, error = %v", proof, err)
	}
	count := int64(42)
	conversation := int64(43)
	p.Metadata.Stats.TurnCount = 44
	p.Metadata.Stats.ConversationTurnCount = &conversation
	p.Metadata.Stats.InputSubmissionCount = &count
	result, err := s.SettleManagedGenerationUnchanged(context.Background(), p)
	if err != nil || result.GenerationID != p.GenerationID {
		t.Fatalf("settlement = %+v, error = %v", result, err)
	}
	after, err := wal.CountWALCommitFrames(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if commits := after - before; commits < 1 || commits > maxCommits {
		t.Fatalf("settle committed %d WAL transactions, want 1..%d", commits, maxCommits)
	}
	t.Logf("settle WAL commit frames = %d", after-before)
	if len(reads) != 0 {
		t.Fatalf("generation mapping reads: %v", reads)
	}
	var observed []string
	for name := range accesses {
		observed = append(observed, name)
	}
	sort.Strings(observed)
	// SetAuthorizer invalidates cached prepared statements. Every read of a
	// table column in every subsequently prepared statement (including trigger
	// statements) is authorized here. Pinning the entire read-column set and
	// observing zero authorizations for these three tables proves that no
	// executed statement could read even one of their rows.
	expectedReads := fixture.ReadColumns
	if !slices.Equal(observed, expectedReads) {
		t.Fatalf("prepared read-column set = %q, want %q", observed, expectedReads)
	}
	conn, err = s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var written []string
	err = sqlitex.Execute(conn, `SELECT table_name FROM observed_writes ORDER BY table_name`, &sqlitex.ExecOptions{ResultFunc: func(st *sqlite.Stmt) error {
		written = append(written, st.ColumnText(0))
		return nil
	}})
	s.pool.Put(conn)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(written, changedTables) {
		t.Fatalf("affected table rows = %v, want %v", written, changedTables)
	}
	conn, err = s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var mirror int64
	err = sqlitex.Execute(conn, `SELECT input_submission_count FROM sessions WHERE session_id=?`, &sqlitex.ExecOptions{Args: []any{string(p.SessionID)}, ResultFunc: func(st *sqlite.Stmt) error {
		mirror = st.ColumnInt64(0)
		return nil
	}})
	s.pool.Put(conn)
	if err != nil || mirror != *prior.InputSubmissionCount {
		t.Fatalf("input count mirror = %d, error = %v; want %d", mirror, err, *prior.InputSubmissionCount)
	}
	state, err := s.ReadIndexState(context.Background(), p.SessionID)
	if err != nil || state == nil || !state.PublicationBound || state.PublicationCaptureRevision != 2 {
		t.Fatalf("post-settle binding state = %+v, error = %v", state, err)
	}
	capture, found, err := s.GetSessionContentCapture(context.Background(), p.SessionID)
	if err != nil || !found || capture.PublicationCaptureRevision != 2 {
		t.Fatalf("post-settle capture = %+v, found = %t, error = %v", capture, found, err)
	}
	stats, err := s.ReadCapturedStats(context.Background(), p.SessionID)
	if err != nil || stats.InputSubmissionCount == nil || stats.TurnCount == nil || stats.ConversationTurnCount == nil ||
		*stats.InputSubmissionCount != *prior.InputSubmissionCount || *stats.TurnCount != *prior.TurnCount || *stats.ConversationTurnCount != *prior.ConversationTurnCount {
		t.Fatalf("stats = %+v, error = %v", stats, err)
	}
}

func TestManagedGenerationUnchangedSettleRefusesMissingCounts(t *testing.T) {
	var fixture struct {
		Cases []struct {
			Name   string `yaml:"name"`
			Column string `yaml:"column"`
		} `yaml:"cases"`
	}
	var manifest struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	if err := yaml.Unmarshal(unchangedMissingCountsYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(unchangedMissingCountsManifestYAML, &manifest); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range fixture.Cases {
		names = append(names, c.Name)
		t.Run(c.Name, func(t *testing.T) {
			if !slices.Contains([]string{"turn_count", "conversation_turn_count", "input_submission_count"}, c.Column) {
				t.Fatalf("unknown count column %q", c.Column)
			}
			s, request := unchangedProofSetup(t)
			conn, err := s.pool.Take(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if c.Column == "conversation_turn_count" {
				// The current schema forbids NULL here. Exercise a legacy or
				// damaged row in this isolated test database.
				var version int
				err = sqlitex.Execute(conn, `PRAGMA schema_version`, &sqlitex.ExecOptions{ResultFunc: func(st *sqlite.Stmt) error {
					version = st.ColumnInt(0)
					return nil
				}})
				if err == nil {
					err = sqlitex.ExecuteScript(conn, fmt.Sprintf(`PRAGMA writable_schema=ON;
UPDATE sqlite_schema SET sql=replace(sql, 'conversation_turn_count INTEGER NOT NULL DEFAULT 0', 'conversation_turn_count INTEGER DEFAULT 0') WHERE type='table' AND name='session_captured_stats';
PRAGMA schema_version=%d;
PRAGMA writable_schema=OFF;`, version+1), nil)
				}
				if err != nil {
					s.pool.Put(conn)
					t.Fatal(err)
				}
			}
			err = sqlitex.Execute(conn, fmt.Sprintf(`UPDATE session_captured_stats SET %s=NULL WHERE session_id=?`, c.Column), &sqlitex.ExecOptions{Args: []any{string(request.SessionID)}})
			s.pool.Put(conn)
			if err != nil {
				t.Fatal(err)
			}
			proof, err := s.ProveManagedGenerationUnchanged(context.Background(), request)
			if err != nil || !proof.Proven {
				t.Fatalf("proof = %+v, error = %v", proof, err)
			}
			before, err := s.ReadCapturedStats(context.Background(), request.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			beforeState, err := s.ReadIndexState(context.Background(), request.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.SettleManagedGenerationUnchanged(context.Background(), request)
			var refusal *ManagedGenerationUnchangedSettleError
			if !errors.As(err, &refusal) {
				t.Fatalf("settlement error = %v, want missing-count refusal", err)
			}
			stats, err := s.ReadCapturedStats(context.Background(), request.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			afterState, err := s.ReadIndexState(context.Background(), request.SessionID)
			if err != nil || !reflect.DeepEqual(beforeState, afterState) || !reflect.DeepEqual(before, stats) {
				t.Fatalf("refused settlement changed stats or index state: error = %v", err)
			}
			switch c.Column {
			case "turn_count":
				if stats.TurnCount != nil {
					t.Fatal("refused settlement wrote turn count")
				}
			case "conversation_turn_count":
				if stats.ConversationTurnCount != nil {
					t.Fatal("refused settlement wrote conversation count")
				}
			case "input_submission_count":
				if stats.InputSubmissionCount != nil {
					t.Fatal("refused settlement wrote input count")
				}
			}
		})
	}
	if err := validateRecoveryRequiredNames(manifest.RequiredNames, names, "unchanged missing counts"); err != nil {
		t.Fatal(err)
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
	if err == nil {
		t.Fatalf("moved generation settled: %+v, %v", result, err)
	}
}

func TestManagedGenerationUnchangedProof(t *testing.T) {
	for _, tc := range LoadUnchangedProofFixtures(t).Cases {
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
			case "null-input-proof":
				conn, err := s.pool.Take(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				err = sqlitex.Execute(conn, `UPDATE sessions SET indexed_input_hash=NULL WHERE session_id=?`, &sqlitex.ExecOptions{Args: []any{string(p.SessionID)}})
				s.pool.Put(conn)
				if err != nil {
					t.Fatal(err)
				}
				p.Expected, err = s.ReadIndexState(context.Background(), p.SessionID)
				if err != nil || p.Expected == nil || p.Expected.IndexedInputHash != nil {
					t.Fatalf("expected NULL input proof: %+v, %v", p.Expected, err)
				}
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
