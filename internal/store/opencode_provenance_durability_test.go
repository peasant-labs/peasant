package store

import (
	"bytes"
	"context"
	_ "embed"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/opencode_provenance_durability.yaml
var openCodeDurabilityYAML []byte

//go:embed testdata/opencode_provenance_durability.manifest.yaml
var openCodeDurabilityManifestYAML []byte

type openCodeDurabilityRow struct {
	ID          string `yaml:"id"`
	Type        string `yaml:"type"`
	Seq         int64  `yaml:"seq"`
	TimeCreated int64  `yaml:"time_created"`
	TimeUpdated int64  `yaml:"time_updated"`
	Data        string `yaml:"data"`
}

type openCodeDurabilityFork struct {
	SourceSessionID  string `yaml:"source_session_id"`
	ThroughSeq       *int64 `yaml:"through_seq"`
	ThroughCompleted bool   `yaml:"through_completed"`
}

type openCodeDurabilityCase struct {
	Name                string                  `yaml:"name"`
	SessionID           string                  `yaml:"session_id"`
	ParentID            string                  `yaml:"parent_id"`
	GenerationID        string                  `yaml:"generation_id"`
	RefreshGenerationID string                  `yaml:"refresh_generation_id"`
	Fork                openCodeDurabilityFork  `yaml:"fork"`
	ParentRows          []openCodeDurabilityRow `yaml:"parent_rows"`
}

type openCodeDurabilityFixture struct {
	Cases []openCodeDurabilityCase `yaml:"cases"`
}

func loadOpenCodeDurabilityFixture(t *testing.T) openCodeDurabilityFixture {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(openCodeDurabilityYAML))
	decoder.KnownFields(true)
	var fixture openCodeDurabilityFixture
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode OpenCode durable-prior fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("OpenCode durable-prior fixture must contain exactly one YAML document: %v", err)
	}
	manifest, err := decodeRecoveryRequiredNames(openCodeDurabilityManifestYAML)
	if err != nil {
		t.Fatalf("decode OpenCode durable-prior manifest: %v", err)
	}
	names := make([]string, 0, len(fixture.Cases))
	for _, tc := range fixture.Cases {
		if tc.Name == "" || tc.SessionID == "" || tc.ParentID == "" || tc.GenerationID == "" || tc.RefreshGenerationID == "" || tc.Fork.SourceSessionID == "" || tc.Fork.ThroughSeq == nil || len(tc.ParentRows) == 0 {
			t.Fatalf("OpenCode durable-prior case %q is incomplete", tc.Name)
		}
		names = append(names, tc.Name)
	}
	if err := validateRecoveryRequiredNames(manifest, names, "OpenCode durable prior"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// TestOpenCodeProvenanceDurablePriorRecovery proves captured child-prefix
// recovery from persisted managed storage. The initial generation is activated
// into the real store and the activation-owned prior document is written to
// disk; the store is then closed and reopened, the prior aliases are reloaded
// from the persisted generation and the retained prefix from the persisted
// document, the native parent and its rows are deleted, and the real refresh
// path must rebuild the captured refs and full bytes. Nothing is carried in
// memory across the reopen.
func TestOpenCodeProvenanceDurablePriorRecovery(t *testing.T) {
	fixture := loadOpenCodeDurabilityFixture(t)
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			dir := t.TempDir()
			s := openGenerationStoreAt(t, dir)
			seedOpenCodeDurabilitySession(t, s, tc)
			source := testfixture.MaterializeByName(t, "native-current-rows")
			seedOpenCodeDurabilityParent(t, source, tc)

			ctx := context.Background()
			adapter := ingest.NewOpenCodeAdapter(&ingest.OSFileSystem{}, nil, salt.Salt{})
			fork := &ingest.OpenCodeForkProof{SourceSessionID: tc.Fork.SourceSessionID, ThroughSeq: tc.Fork.ThroughSeq, ThroughCompleted: tc.Fork.ThroughCompleted}
			options := ingest.OpenCodeSnapshotOptions{Fork: fork}
			snapshot, err := adapter.SnapshotOpenCodeProvenance(ctx, source.Path, tc.SessionID, options)
			if err != nil {
				t.Fatalf("initial snapshot: %v", err)
			}
			if len(snapshot.Copied) != len(tc.ParentRows) {
				t.Fatalf("initial captured prefix = %d rows, want %d", len(snapshot.Copied), len(tc.ParentRows))
			}
			metadata := schema.UnifiedMetadata{SchemaVersion: ingest.CurrentSchemaVersion, SessionID: schema.SessionID(tc.SessionID), ModelHarness: schema.HarnessOpenCode}
			capture, err := ingest.BuildOpenCodeProvenanceCapture(snapshot, tc.GenerationID, metadata, ingest.OpenCodeProvenancePrior{Aliases: ingest.NewProjectionPriorState()})
			if err != nil {
				t.Fatalf("build initial capture: %v", err)
			}
			first, err := ingest.BuildV2(capture, ingest.RandomRefAllocator{})
			if err != nil {
				t.Fatalf("build initial generation: %v", err)
			}
			if len(first.Generation.Segments) != 1 || len(first.Generation.Segments[0].CapturedRefs) == 0 {
				t.Fatalf("initial generation has no captured prefix: %+v", first.Generation.Segments)
			}
			capturedRefs := append([]schema.SourceEntryRef(nil), first.Generation.Segments[0].CapturedRefs...)
			if err := activateTestGeneration(t, s, first, openCodeDurabilityBlobs(t, capture, first.Generation)); err != nil {
				t.Fatalf("activate initial generation: %v", err)
			}

			// The activation owns persisting the prior evidence: the alias and
			// completeness document plus the retained captured rows. The
			// indexer never invents this; it only consumes it.
			state, err := ingest.PriorStateFromGeneration(first.Generation)
			if err != nil {
				t.Fatalf("PriorStateFromGeneration: %v", err)
			}
			priorBytes, err := ingest.EncodeOpenCodeProvenancePrior(ingest.OpenCodeProvenancePrior{
				Aliases:               state,
				CapturedPrefix:        append([]ingest.OpenCodeHistoryRow(nil), snapshot.Copied...),
				HasCapturedPrefix:     true,
				HasCompleteGeneration: true,
			})
			if err != nil {
				t.Fatalf("persist prior evidence: %v", err)
			}
			priorPath := filepath.Join(dir, "opencode-prior.json")
			if err := os.WriteFile(priorPath, priorBytes, 0o600); err != nil {
				t.Fatalf("write persisted prior evidence: %v", err)
			}

			// Reopen the real store and the owned-artifact file store: every
			// assertion below reads durable rows and blobs, never the candidate.
			if err := s.Close(); err != nil {
				t.Fatalf("close store before reopen: %v", err)
			}
			s = openGenerationStoreAt(t, dir)
			defer func() { _ = s.Close() }()

			manifest, err := s.generationArtifacts.ReadManifest(ctx, schema.SessionID(tc.SessionID), tc.GenerationID)
			if err != nil {
				t.Fatalf("read persisted generation manifest: %v", err)
			}
			if err := manifest.Validate(); err != nil {
				t.Fatalf("persisted generation manifest is invalid: %v", err)
			}
			recoveredAliases, err := ingest.PriorStateFromGeneration(manifest)
			if err != nil {
				t.Fatalf("derive persisted aliases: %v", err)
			}
			if len(recoveredAliases.Entries) == 0 {
				t.Fatal("persisted generation carries no reusable native aliases")
			}
			storedPrior, err := ingest.DecodeOpenCodeProvenancePrior(readOpenCodeDurabilityFile(t, priorPath))
			if err != nil {
				t.Fatalf("reload persisted prior evidence: %v", err)
			}
			if !storedPrior.HasCapturedPrefix || len(storedPrior.CapturedPrefix) == 0 {
				t.Fatal("persisted prior evidence lost the retained captured prefix")
			}
			storedPrior.Aliases = recoveredAliases

			// Persisted content resolves for every captured ref before the
			// native parent disappears, proving the store owns the bytes.
			for _, ref := range capturedRefs {
				record := openCodeDurabilityRecord(t, manifest, ref)
				content, err := s.ReadFullContent(ctx, schema.SessionID(tc.SessionID), tc.GenerationID, record)
				if err != nil {
					t.Fatalf("read persisted captured content %q: %v", ref, err)
				}
				if int64(len(content)) != record.ByteLength {
					t.Fatalf("persisted content %q length = %d, want %d", ref, len(content), record.ByteLength)
				}
			}

			deleteOpenCodeDurabilityParent(t, source, tc)
			if openCodeDurabilityParentRows(t, source, tc.ParentID) != 0 {
				t.Fatal("native parent rows survived deletion")
			}

			config := ingest.OpenCodeProvenanceIndexerConfig{
				Enabled: true,
				Snapshot: func(ctx context.Context, _ ingest.DiscoveredSession) (ingest.OpenCodeHistorySnapshot, error) {
					return adapter.SnapshotOpenCodeProvenance(ctx, source.Path, tc.SessionID, options)
				},
				Metadata: func(_ ingest.DiscoveredSession) (schema.UnifiedMetadata, error) { return metadata, nil },
				GenerationID: func(_ ingest.DiscoveredSession) string {
					return tc.RefreshGenerationID
				},
				Prior: func(context.Context, ingest.DiscoveredSession) (ingest.OpenCodeProvenancePrior, error) {
					return storedPrior, nil
				},
			}
			indexer := ingest.NewOpenCodeIndexer(&ingest.OSFileSystem{}, ingest.WithOpenCodeProvenanceCapture(config))
			session := ingest.DiscoveredSession{
				SessionID:        ingest.SessionID(tc.SessionID),
				Harness:          ingest.HarnessOpenCode,
				TranscriptOrigin: ingest.TranscriptOriginOpenCodeCurrentSQLite,
			}
			result, err := indexer.IndexTranscriptResult(ctx, session)
			if err != nil {
				t.Fatalf("refresh after reopening persisted storage: %v", err)
			}
			refreshed, ok := result.(indexformat.V2)
			if !ok {
				t.Fatalf("refresh returned %T, want indexformat.V2", result)
			}
			if len(refreshed.Generation.Segments) != 1 {
				t.Fatalf("refreshed segments = %d, want 1", len(refreshed.Generation.Segments))
			}
			gotRefs := refreshed.Generation.Segments[0].CapturedRefs
			if !equalOpenCodeDurabilityRefs(gotRefs, capturedRefs) {
				t.Fatalf("refreshed captured refs = %v, want %v", gotRefs, capturedRefs)
			}
			for _, ref := range capturedRefs {
				persisted := openCodeDurabilityRecord(t, manifest, ref)
				refreshedRecord := openCodeDurabilityRecord(t, refreshed.Generation, ref)
				if persisted.Digest != refreshedRecord.Digest || persisted.ByteLength != refreshedRecord.ByteLength {
					t.Fatalf("refreshed content %q changed: %+v -> %+v", ref, persisted, refreshedRecord)
				}
			}
		})
	}
}

// openCodeDurabilityBlobs maps each classified block's full content to the ref
// the shared projection allocated for its native key, so the store can stage
// the exact bytes the emitted and retained records address.
func openCodeDurabilityBlobs(t *testing.T, capture ingest.ClassifiedCapture, generation indexformat.Generation) map[schema.SourceEntryRef][]byte {
	t.Helper()
	byNative := make(map[string]schema.SourceEntryRef, len(generation.Aliases))
	for _, alias := range generation.Aliases {
		if strings.HasPrefix(alias.NativeKey, "block:") {
			byNative[strings.TrimPrefix(alias.NativeKey, "block:")] = alias.Ref
		}
	}
	blobs := make(map[schema.SourceEntryRef][]byte, len(generation.Content))
	for _, block := range capture.Blocks {
		ref, ok := byNative[block.NativeKey]
		if !ok {
			continue
		}
		content := block.Content
		switch block.EntryType {
		case schema.EntryTypeToolUse:
			content = block.ToolArguments
		case schema.EntryTypeToolResult:
			content = block.ToolResult
		}
		blobs[ref] = []byte(content)
	}
	for _, record := range generation.Content {
		if _, ok := blobs[record.Ref]; !ok {
			t.Fatalf("captured content for ref %q has no classified block; the candidate is not self-contained", record.Ref)
		}
	}
	return blobs
}

func openCodeDurabilityRecord(t *testing.T, generation indexformat.Generation, ref schema.SourceEntryRef) indexformat.ContentRecord {
	t.Helper()
	for _, record := range generation.Content {
		if record.Ref == ref {
			return record
		}
	}
	t.Fatalf("generation %s carries no content record for captured ref %q", generation.ID, ref)
	return indexformat.ContentRecord{}
}

func equalOpenCodeDurabilityRefs(left, right []schema.SourceEntryRef) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func readOpenCodeDurabilityFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read persisted prior evidence: %v", err)
	}
	return data
}

func seedOpenCodeDurabilitySession(t *testing.T, s *Store, tc openCodeDurabilityCase) {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatalf("Pool.Take: %v", err)
	}
	defer s.pool.Put(conn)
	statements := []struct {
		sql  string
		args []any
	}{
		{sql: "INSERT OR IGNORE INTO host_slugs(opaque_id, host_slug) VALUES('host-gen','host-gen')"},
		{sql: "INSERT OR IGNORE INTO projects(project_hash, canonical_cwd, canonical_remote) VALUES('proj-gen','/tmp/gen','github.com/gen/gen')"},
		{sql: "INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format, schema_version) VALUES(?1,'opencode','model-durable','host-gen','proj-gen',1,2,3,'/tmp/durable/source.json','json',?2)", args: []any{tc.SessionID, ingest.CurrentSchemaVersion}},
	}
	for _, statement := range statements {
		if err := sqlitex.Execute(conn, statement.sql, &sqlitex.ExecOptions{Args: statement.args}); err != nil {
			t.Fatalf("seed durable store session: %v", err)
		}
	}
}

func seedOpenCodeDurabilityParent(t *testing.T, source testfixture.MaterializedSource, tc openCodeDurabilityCase) {
	t.Helper()
	connection := openOpenCodeDurabilityNative(t, source)
	defer func() { _ = connection.Close() }()
	if err := sqlitex.Execute(connection, "INSERT INTO session (id, parent_id, time_created, time_updated) VALUES (?1, '', 500, 600)", &sqlitex.ExecOptions{Args: []any{tc.ParentID}}); err != nil {
		t.Fatalf("insert durable native parent session: %v", err)
	}
	for _, row := range tc.ParentRows {
		if err := sqlitex.Execute(connection, "INSERT INTO session_message (id, session_id, type, time_created, time_updated, data, seq) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7)", &sqlitex.ExecOptions{
			Args: []any{row.ID, tc.ParentID, row.Type, row.TimeCreated, row.TimeUpdated, row.Data, row.Seq},
		}); err != nil {
			t.Fatalf("insert durable native parent row %q: %v", row.ID, err)
		}
	}
	if err := sqlitex.Execute(connection, "UPDATE session SET parent_id = ?1 WHERE id = ?2", &sqlitex.ExecOptions{Args: []any{tc.ParentID, tc.SessionID}}); err != nil {
		t.Fatalf("link durable child session to its native parent: %v", err)
	}
}

func deleteOpenCodeDurabilityParent(t *testing.T, source testfixture.MaterializedSource, tc openCodeDurabilityCase) {
	t.Helper()
	connection := openOpenCodeDurabilityNative(t, source)
	defer func() { _ = connection.Close() }()
	if err := sqlitex.Execute(connection, "DELETE FROM session_message WHERE session_id = ?1", &sqlitex.ExecOptions{Args: []any{tc.ParentID}}); err != nil {
		t.Fatalf("delete durable native parent rows: %v", err)
	}
	if err := sqlitex.Execute(connection, "DELETE FROM session WHERE id = ?1", &sqlitex.ExecOptions{Args: []any{tc.ParentID}}); err != nil {
		t.Fatalf("delete durable native parent session: %v", err)
	}
}

func openCodeDurabilityParentRows(t *testing.T, source testfixture.MaterializedSource, parentID string) int {
	t.Helper()
	connection := openOpenCodeDurabilityNative(t, source)
	defer func() { _ = connection.Close() }()
	count := 0
	if err := sqlitex.Execute(connection, "SELECT COUNT(*) FROM session_message WHERE session_id = ?1", &sqlitex.ExecOptions{
		Args: []any{parentID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			count = int(stmt.ColumnInt64(0))
			return nil
		},
	}); err != nil {
		t.Fatalf("count durable native parent rows: %v", err)
	}
	return count
}

func openOpenCodeDurabilityNative(t *testing.T, source testfixture.MaterializedSource) *sqlite.Conn {
	t.Helper()
	_ = testfixture.SnapshotSource(t, source)
	connection, err := sqlite.OpenConn(source.Path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatalf("open durable synthetic native source: %v", err)
	}
	return connection
}
