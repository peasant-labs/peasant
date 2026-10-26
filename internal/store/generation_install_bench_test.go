package store

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// benchV2InstallGeneration builds one valid complete managed generation with
// the given number of main projection entries. The entry rows dominate a native
// session install, so the benchmark writes one projection row per entry.
func benchV2InstallGeneration(sid schema.SessionID, genID string, entries int) indexformat.V2 {
	inputCount := int64(entries)
	sessionEntries := make([]schema.SessionEntry, entries)
	for i := range sessionEntries {
		preview := fmt.Sprintf("bench projection entry %d", i)
		sessionEntries[i] = schema.SessionEntry{
			SessionID:      sid,
			EntryIndex:     i,
			Harness:        defaults.HarnessClaudeCode,
			EntryType:      ingest.EntryTypeText,
			Role:           ingest.RoleAssistant,
			ContentPreview: &preview,
			SourceEntryRef: schema.SourceEntryRef(fmt.Sprintf("e_%06d", i)),
		}
	}
	generation := indexformat.Generation{
		ID:           genID,
		Completeness: indexformat.GenerationCompletenessComplete,
		Metadata: schema.UnifiedMetadata{
			SchemaVersion: ingest.CurrentSchemaVersion,
			SessionID:     sid,
			ModelHarness:  defaults.HarnessClaudeCode,
			Stats:         schema.SessionStats{TurnCount: entries, InputSubmissionCount: &inputCount},
		},
		Main:                 indexformat.Partition{Entries: sessionEntries},
		Aliases:              []indexformat.NativeAlias{{NativeKey: "native-0", Ref: schema.SourceEntryRef("e_000000")}},
		SourceEvidenceDigest: strings.Repeat("a", 64),
		TitleRefs:            []schema.SourceEntryRef{schema.SourceEntryRef("e_000000")},
	}
	return indexformat.V2{Generation: generation}
}

// openBenchGenerationStore opens a real V2 store for a benchmark. It mirrors
// openGenerationStore but accepts testing.TB.
func openBenchGenerationStore(tb testing.TB) *Store {
	tb.Helper()
	dir := tb.TempDir()
	root := filepath.Join(dir, "artifacts")
	artifacts, err := NewOSGenerationArtifactStore(root)
	if err != nil {
		tb.Fatal(err)
	}
	locker, err := NewFileSessionLocker(root)
	if err != nil {
		tb.Fatal(err)
	}
	s, err := Open(filepath.Join(dir, "generations.db"), WithPoolSize(1), WithIndexFormats(generationIndexFormat{}), WithGenerationArtifacts(artifacts, locker))
	if err != nil {
		tb.Fatal(err)
	}
	return s
}

// seedBenchGenerationSession stores the host, project and session rows the V2
// install needs before a generation can be activated.
func seedBenchGenerationSession(tb testing.TB, s *Store, sid schema.SessionID) {
	tb.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		tb.Fatal(err)
	}
	defer s.pool.Put(conn)
	if err := sqlitex.ExecuteScript(conn, `
INSERT OR IGNORE INTO host_slugs(opaque_id, host_slug) VALUES('host-gen','host-gen');
INSERT OR IGNORE INTO projects(project_hash, canonical_cwd, canonical_remote) VALUES('proj-gen','/tmp/gen','github.com/gen/gen');
INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format, schema_version)
VALUES('`+string(sid)+`','claude-code','model-gen','host-gen','proj-gen',1,2,3,'/tmp/gen/source.jsonl','jsonl',11);
`, nil); err != nil {
		tb.Fatalf("seed benchmark session %s: %v", sid, err)
	}
}

// BenchmarkV2GenerationActivate measures the real managed-generation install
// entry point, ActivateGeneration, for a fresh session. It includes the
// lock-free staging and the one activation transaction that installs the
// generation rows, so it shows the change on the production path end to end.
func BenchmarkV2GenerationActivate(b *testing.B) {
	for _, entries := range []int{2000, 20000} {
		b.Run(fmt.Sprintf("entries=%d", entries), func(b *testing.B) {
			s := openBenchGenerationStore(b)
			defer func() { _ = s.Close() }()
			ctx := context.Background()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				sid, err := schema.NewSessionID(fmt.Sprintf("cccccccc-cccc-4ccc-8ccc-%012d", i))
				if err != nil {
					b.Fatal(err)
				}
				b.StopTimer()
				seedBenchGenerationSession(b, s, sid)
				b.StartTimer()
				activation := GenerationActivation{
					Generation:     benchV2InstallGeneration(sid, fmt.Sprintf("g_activate_%d", i), entries),
					IndexerVersion: ingest.HarvesterVersionRegistry[ingest.HarnessClaudeCode].IndexerVersion,
					IndexedAtMs:    1700000005000,
				}
				outcome, err := s.ActivateGeneration(ctx, activation)
				if err != nil {
					b.Fatal(err)
				}
				if outcome.Disposition != ingest.ActivationCommittedNow {
					b.Fatalf("activation disposition = %v, want committed now", outcome.Disposition)
				}
			}
		})
	}
}

// BenchmarkV2GenerationInstall measures one serialized managed-generation
// database install for a fresh session: the production DB install path that
// ActivateGeneration reaches through IndexSessionEntryBatch, writing one body
// row per entry plus the mapping, section, alias, segment, content and
// relationship-evidence rows. A fresh session is written every iteration so
// the install actually runs instead of taking a skip path.
func BenchmarkV2GenerationInstall(b *testing.B) {
	for _, entries := range []int{2000, 20000} {
		b.Run(fmt.Sprintf("entries=%d", entries), func(b *testing.B) {
			s := openBenchGenerationStore(b)
			defer func() { _ = s.Close() }()
			ctx := context.Background()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				sid, err := schema.NewSessionID(fmt.Sprintf("bbbbbbbb-bbbb-4bbb-8bbb-%012d", i))
				if err != nil {
					b.Fatal(err)
				}
				b.StopTimer()
				seedBenchGenerationSession(b, s, sid)
				b.StartTimer()
				write := ingest.SessionEntryWrite{
					SessionID:      ingest.SessionID(sid),
					Result:         benchV2InstallGeneration(sid, fmt.Sprintf("g_bench_%d", i), entries),
					IndexVersion:   2,
					IndexerVersion: ingest.HarvesterVersionRegistry[ingest.HarnessClaudeCode].IndexerVersion,
					IndexedAtMs:    1700000005000,
				}
				result := s.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{write})
				if result[0].Err != nil {
					b.Fatal(result[0].Err)
				}
			}
		})
	}
}
