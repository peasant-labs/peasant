package store_test

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"io"
	"reflect"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/artifact_mirror.yaml
var artifactMirrorYAML []byte

type artifactMirrorCase struct {
	Name          string                `yaml:"name"`
	EventSeq      *int64                `yaml:"eventSeq"`
	Origin        *sessionorigin.Origin `yaml:"origin"`
	WantCursor    int64                 `yaml:"wantCursor"`
	WantOrigin    sessionorigin.Origin  `yaml:"wantOrigin"`
	InvalidCommit bool                  `yaml:"invalidCommit"`
	WantError     bool                  `yaml:"wantError"`
	Orphan        bool                  `yaml:"orphan"`
	ParentFirst   bool                  `yaml:"parentFirst"`
	StoredAdapter *int                  `yaml:"storedAdapter"`
	// JudgedOrigin records a resolver verdict, at the current rule version,
	// before the mirror runs. A row carrying one must keep it.
	JudgedOrigin sessionorigin.Origin `yaml:"judgedOrigin"`
	MissingStats bool                 `yaml:"missingStats"`
	// Harness overrides the fixture's default OpenCode harness so an
	// other-harness control can prove its existing orphan refusal is preserved.
	Harness ingest.Harness `yaml:"harness"`
}

type artifactMirrorFixtures struct {
	RequiredNames        []string             `yaml:"requiredNames"`
	SessionID            string               `yaml:"sessionID"`
	ParentID             string               `yaml:"parentID"`
	ProjectHash          string               `yaml:"projectHash"`
	HostSlug             string               `yaml:"hostSlug"`
	StartedAt            int64                `yaml:"startedAt"`
	Transcript           string               `yaml:"transcript"`
	OriginalCursor       int64                `yaml:"originalCursor"`
	OriginalOrigin       sessionorigin.Origin `yaml:"originalOrigin"`
	OriginalCommit       string               `yaml:"originalCommit"`
	ReplacementCommit    string               `yaml:"replacementCommit"`
	Cases                []artifactMirrorCase `yaml:"cases"`
	PrepareRequiredNames []string             `yaml:"prepareRequiredNames"`
	PrepareCases         []struct {
		Name      string `yaml:"name"`
		Missing   bool   `yaml:"missing"`
		Artifact  bool   `yaml:"artifact"`
		Proof     bool   `yaml:"proof"`
		WantProof bool   `yaml:"wantProof"`
	} `yaml:"prepareCases"`
}

func TestPrepareArtifactInstall(t *testing.T) {
	t.Parallel()
	fixture := loadArtifactMirrorFixtures(t)
	names := make(map[string]bool)
	for _, row := range fixture.PrepareCases {
		if row.Name == "" || names[row.Name] {
			t.Fatalf("invalid preparation case %q", row.Name)
		}
		declared := false
		for _, required := range fixture.PrepareRequiredNames {
			if required == row.Name {
				declared = true
			}
		}
		if !declared {
			t.Fatalf("preparation case %q absent from manifest", row.Name)
		}
		names[row.Name] = true
	}
	if err := testutil.RequireFixtureNames("artifact_mirror.yaml", "preparation", fixture.PrepareRequiredNames, names); err != nil {
		t.Fatal(err)
	}
	for _, row := range fixture.PrepareCases {
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			db := openTestStore(t)
			entry := makeStoreEntry(t, fixture.SessionID, fixture.ProjectHash, fixture.HostSlug, defaults.HarnessOpenCode, fixture.StartedAt, 100, 50)
			if !row.Missing {
				if err := db.InsertSessions(t.Context(), []ingest.StoreEntry{entry}); err != nil {
					t.Fatal(err)
				}
				conn := takeConn(t, db.Pool())
				var artifact, proof any
				if row.Artifact {
					artifact = fixture.ProjectHash
				}
				if row.Proof {
					proof = fixture.ProjectHash
				}
				err := sqlitex.Execute(conn, `UPDATE sessions SET artifact_hash = ?, indexed_input_hash = ? WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{artifact, proof, fixture.SessionID}})
				db.Pool().Put(conn)
				if err != nil {
					t.Fatal(err)
				}
			}
			before := preparationSessionState(t, db.Pool(), fixture.SessionID)
			if err := db.PrepareArtifactInstall(t.Context(), entry.Metadata.SessionID); err != nil {
				t.Fatal(err)
			}
			if after := preparationSessionState(t, db.Pool(), fixture.SessionID); !reflect.DeepEqual(before, after) {
				t.Fatalf("preparation changed retained state: before=%+v after=%+v", before, after)
			}
			state, err := db.ReadIndexState(t.Context(), entry.Metadata.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			if row.Missing {
				if state != nil {
					t.Fatal("preparation created a missing row")
				}
				return
			}
			if (state.IndexedInputHash != nil) != row.WantProof {
				t.Fatalf("proof=%v, want presence %v", state.IndexedInputHash, row.WantProof)
			}
		})
	}
}

// Include every session column except the one preparation owns, so widening a
// later UPDATE cannot silently change publication, source or producer evidence.
func preparationSessionState(t *testing.T, pool *sqlitex.Pool, sid string) map[string]string {
	t.Helper()
	conn := takeConn(t, pool)
	defer pool.Put(conn)
	state := make(map[string]string)
	if err := sqlitex.Execute(conn, `SELECT * FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{sid}, ResultFunc: func(stmt *sqlite.Stmt) error {
			for i := 0; i < stmt.ColumnCount(); i++ {
				if name := stmt.ColumnName(i); name != "indexed_input_hash" {
					state[name] = stmt.ColumnType(i).String() + ":" + stmt.ColumnText(i)
				}
			}
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	return state
}

func loadArtifactMirrorFixtures(t *testing.T) artifactMirrorFixtures {
	t.Helper()
	var fixture artifactMirrorFixtures
	decoder := yaml.NewDecoder(bytes.NewReader(artifactMirrorYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatal("artifact mirror fixture requires one document")
	}
	required := []string{"acquired-evidence", "judged-origin-outranks-adapter-evidence", "retained-preserves-evidence", "explicit-zero-cursor", "association-failure-rolls-back", "orphan-refused", "opencode-orphan-retained", "parent-first", "future-stored-adapter-refused", "missing-stats-stay-unknown"}
	if !reflect.DeepEqual(required, fixture.RequiredNames) {
		t.Fatal("artifact mirror required-name manifest changed")
	}
	seen := make(map[string]bool)
	for _, row := range fixture.Cases {
		if row.Name == "" || seen[row.Name] {
			t.Fatalf("invalid mirror fixture %q", row.Name)
		}
		seen[row.Name] = true
	}
	for _, name := range required {
		if !seen[name] {
			t.Fatalf("missing mirror fixture %q", name)
		}
	}
	return fixture
}

func TestArtifactMirrorCommitsEvidenceTogether(t *testing.T) {
	t.Parallel()
	fixture := loadArtifactMirrorFixtures(t)
	for _, row := range fixture.Cases {
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			db := openTestStore(t)
			mirror, ok := any(db).(ingest.ArtifactMirrorStore)
			if !ok {
				t.Fatal("production store cannot transactionally mirror a committed artifact")
			}
			harness := defaults.HarnessOpenCode
			if row.Harness != "" {
				harness = row.Harness
			}
			entry := makeStoreEntry(t, fixture.SessionID, fixture.ProjectHash, fixture.HostSlug, harness, fixture.StartedAt, 100, 50)
			entry.Session.Origin = fixture.OriginalOrigin
			entry.Metadata.AdapterVersion = row.StoredAdapter
			seeded := !row.Orphan && !row.ParentFirst && !row.MissingStats
			if seeded {
				if err := db.InsertSessions(t.Context(), []ingest.StoreEntry{entry}); err != nil {
					t.Fatal(err)
				}
				if err := db.UpsertOpenCodeSeqCursor(t.Context(), entry.Metadata.SessionID, fixture.OriginalCursor); err != nil {
					t.Fatal(err)
				}
				if err := db.UpsertSessionCommits(t.Context(), entry.Metadata.SessionID, []ingest.CommitInfo{{Hash: fixture.OriginalCommit}}); err != nil {
					t.Fatal(err)
				}
				if row.JudgedOrigin != "" {
					// The resolver pass, through its own production writer, so
					// the row carries both halves of the verdict.
					if err := db.UpdateOriginState(t.Context(), entry.Metadata.SessionID, row.JudgedOrigin.String(), ingest.OriginRuleVersion); err != nil {
						t.Fatal(err)
					}
				}
			}
			before := mirrorDatabaseState(t, db.Pool(), fixture.SessionID)
			entry.Metadata.AdapterVersion = nil
			entry.Metadata.Stats.TokensIn++
			entry.Metadata.Git.Commits = []ingest.CommitInfo{{Hash: fixture.ReplacementCommit}}
			if row.InvalidCommit {
				entry.Metadata.Git.Commits[0].Hash = ""
			}
			if row.Orphan || row.ParentFirst {
				parent, err := ingest.NewSessionID(fixture.ParentID)
				if err != nil {
					t.Fatal(err)
				}
				entry.Metadata.ParentUUID = &parent
			}
			artifact := mirrorTestArtifact(t, entry.Metadata, fixture.Transcript)
			if row.MissingStats {
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(artifact.MetadataJSON, &fields); err != nil {
					t.Fatal(err)
				}
				delete(fields, "stats")
				delete(fields, "metadataHash")
				data, err := json.Marshal(fields)
				if err != nil {
					t.Fatal(err)
				}
				artifact, err = ingest.NewManagedArtifact(data, []byte(fixture.Transcript))
				if err != nil {
					t.Fatal(err)
				}
			}
			requests := []ingest.ArtifactMirrorRequest{{Artifact: artifact, EventSeq: row.EventSeq, Origin: row.Origin}}
			if row.ParentFirst {
				parent := makeStoreEntry(t, fixture.ParentID, fixture.ProjectHash, fixture.HostSlug, defaults.HarnessOpenCode, fixture.StartedAt, 100, 50)
				requests = append(requests, ingest.ArtifactMirrorRequest{Artifact: mirrorTestArtifact(t, parent.Metadata, fixture.Transcript)})
			}
			results := mirror.MirrorArtifacts(t.Context(), requests)
			if len(results) != len(requests) || results[0].SessionID != entry.Metadata.SessionID || results[0].Mirrored == row.WantError || (results[0].Err != nil) != row.WantError {
				t.Fatalf("incorrect per-session outcome: %+v", results)
			}
			after := mirrorDatabaseState(t, db.Pool(), fixture.SessionID)
			if row.WantError {
				if !reflect.DeepEqual(before, after) {
					t.Fatalf("failed mirror changed prior data: before=%v after=%v", before, after)
				}
				return
			}
			if after.Hash != artifact.ArtifactHash || after.AdapterVersion != nil || (after.Seed == "") != row.MissingStats {
				t.Fatalf("mirror did not store captured identity and seeds: %+v", after)
			}
			if row.MissingStats {
				seed, err := db.GetMetricSeed(t.Context(), artifact.Metadata.SessionID)
				if err != nil || seed != nil {
					t.Fatalf("missing historical Stats became known seeds: %+v %v", seed, err)
				}
				metrics, err := db.GetMetrics(t.Context(), artifact.Metadata.SessionID)
				if err != nil || metrics == nil || metrics.InputTokens != nil || metrics.ComputedAt != nil {
					t.Fatalf("missing input became a completed or known-token metric: %+v %v", metrics, err)
				}
			}
			if seeded && (after.Cursor != row.WantCursor || after.Origin != string(row.WantOrigin)) {
				t.Fatalf("acquired/absent evidence changed: %+v", after)
			}
			// The verdict moves as a PAIR or not at all. Checking the version
			// too is what catches a writer that changes the origin and leaves
			// the row above the resolver's watermark, which no assertion on the
			// origin alone can see.
			if seeded && after.OriginVersion != before.OriginVersion {
				t.Fatalf("mirror moved the origin rule version from %d to %d", before.OriginVersion, after.OriginVersion)
			}
			// The mirror commits an INCOMPLETE commit capture, because a
			// committed artifact proves the commits it names and not the
			// absence of the ones it does not. The newly observed commit is
			// therefore added to the current projection, and a commit a
			// complete earlier capture proved stays bound rather than being
			// dropped by a partial re-observation. Compare the exact set: a
			// count would not say WHICH binding survived.
			associations, err := db.ListCurrentSessionCommitAssociations(t.Context(), entry.Metadata.SessionID)
			if err != nil {
				t.Fatalf("current association projection unreadable: %v", err)
			}
			current := make(map[string]bool, len(associations))
			for _, association := range associations {
				current[association.ObservedCommitHash] = true
			}
			want := map[string]bool{fixture.ReplacementCommit: true}
			if seeded {
				want[fixture.OriginalCommit] = true
			}
			if !reflect.DeepEqual(current, want) {
				t.Fatalf("current commit associations=%v, want %v", current, want)
			}
			if seeded && after.Associations != before.Associations+1 {
				t.Fatal("mirror erased the prior durable association ledger")
			}
		})
	}
}

type mirrorState struct {
	Hash, Seed, Origin string
	AdapterVersion     *int
	Cursor             int64
	OriginVersion      int
	Associations       int
}

func mirrorDatabaseState(t *testing.T, pool *sqlitex.Pool, sid string) mirrorState {
	t.Helper()
	conn := takeConn(t, pool)
	defer pool.Put(conn)
	var state mirrorState
	if err := sqlitex.ExecuteTransient(conn, `SELECT artifact_hash, metric_seed_json, adapter_version, session_origin,
(SELECT last_seq FROM opencode_session_seq_cursor WHERE session_id = sessions.session_id),
(SELECT COUNT(*) FROM session_commit_associations WHERE session_id = sessions.session_id),
origin_version
FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{sid}, ResultFunc: func(stmt *sqlite.Stmt) error {
		state.Hash, state.Seed, state.Origin = stmt.ColumnText(0), stmt.ColumnText(1), stmt.ColumnText(3)
		if stmt.ColumnType(2) != sqlite.TypeNull {
			value := stmt.ColumnInt(2)
			state.AdapterVersion = &value
		}
		state.Cursor, state.Associations = stmt.ColumnInt64(4), stmt.ColumnInt(5)
		state.OriginVersion = stmt.ColumnInt(6)
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	return state
}

func mirrorTestArtifact(t *testing.T, meta *schema.UnifiedMetadata, transcript string) *ingest.ManagedArtifact {
	t.Helper()
	meta.ContentHash = schema.ComputeTranscriptHash([]byte(transcript))
	meta.MetadataHash = schema.ComputeMetadataHash(meta)
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := ingest.NewManagedArtifact(data, []byte(transcript))
	if err != nil {
		t.Fatal(err)
	}
	return artifact
}
