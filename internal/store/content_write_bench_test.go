package store_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
)

// BenchmarkFullContentWriteLargeSession measures one serialized full-content
// session entry write, the operation that dominates the harvest index lane for
// large sessions. A large session writes one durable-prose manifest row per
// content-bearing entry plus one chunk row per 64 KiB, so the benchmark also
// exposes the per-row insert overhead the content writer pays.
func BenchmarkFullContentWriteLargeSession(b *testing.B) {
	for _, entries := range []int{2000, 20000} {
		b.Run(fmt.Sprintf("entries=%d", entries), func(b *testing.B) {
			// ast-grep-ignore: no-migrating-store-open-in-tests -- benchmark setup outside the measured section: storetest helpers require *testing.T, so the corpus is built with one migrating open.
			s, err := store.Open(filepath.Join(b.TempDir(), "bench.db"), store.WithPoolSize(1))
			if err != nil {
				b.Fatal(err)
			}
			defer s.Close()
			b.ReportAllocs()
			b.ResetTimer()
			// Each iteration writes a session the store has not seen, so the
			// canonical and durable-prose inserts actually run; re-writing one
			// session would take the hash-skip path after the first iteration.
			for i := 0; i < b.N; i++ {
				id, err := ingest.NewSessionID(fmt.Sprintf("bbbbbbbb-bbbb-4bbb-8bbb-%012d", i))
				if err != nil {
					b.Fatal(err)
				}
				b.StopTimer()
				seedBenchSession(b, s, id)
				b.StartTimer()
				write := ingest.SessionEntryWrite{
					SessionID:          id,
					Result:             indexformat.V1{Entries: largeFullContentEntries(id, entries)},
					IndexVersion:       1,
					IndexerVersion:     ingest.HarvesterVersionRegistry[ingest.HarnessClaudeCode].IndexerVersion,
					IndexedAtMs:        1700000005000,
					Mode:               ingest.SessionEntryWriteExplicitRebuild,
					RequireFullContent: true,
				}
				result := s.IndexSessionEntryBatch(context.Background(), []ingest.SessionEntryWrite{write})
				if result[0].Err != nil {
					b.Fatal(result[0].Err)
				}
			}
		})
	}
}

func seedBenchSession(b *testing.B, s *store.Store, id ingest.SessionID) {
	b.Helper()
	projectHash, err := ingest.NewProjectHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err != nil {
		b.Fatal(err)
	}
	hostSlug, err := ingest.NewHostSlug("github.com-test")
	if err != nil {
		b.Fatal(err)
	}
	model, err := ingest.NewModelID("claude-opus-4-6")
	if err != nil {
		b.Fatal(err)
	}
	resolvedPath, err := ingest.NewResolvedPath("/test/path/session.jsonl")
	if err != nil {
		b.Fatal(err)
	}
	ingested := int64(1700000120000)
	entry := ingest.StoreEntry{
		Metadata: &ingest.UnifiedMetadata{
			SchemaVersion: ingest.CurrentSchemaVersion,
			SessionID:     id,
			ModelHarness:  ingest.HarnessClaudeCode,
			Model:         model,
			HostSlug:      hostSlug,
			Timestamp:     ingest.TimestampInfo{Start: 1700000000000, End: 1700000060000, Ingested: &ingested},
			Source:        ingest.SourceInfo{FilePath: string(resolvedPath), Format: ingest.SourceFormatJSONL},
			Project:       ingest.ProjectInfo{Hash: projectHash, Name: "test-project", FilePath: "/home/test/project"},
			Stats:         ingest.StatsInfo{TurnCount: 10, ToolCallCount: 5, DurationMs: 60000, TokensIn: 100, TokensOut: 50},
		},
		Session: ingest.DiscoveredSession{SessionID: id, Harness: ingest.HarnessClaudeCode, SourcePath: resolvedPath, SourceFormat: ingest.SourceFormatJSONL},
	}
	if err := s.InsertSessions(context.Background(), []ingest.StoreEntry{entry}); err != nil {
		b.Fatal(err)
	}
}

func largeFullContentEntries(id ingest.SessionID, count int) []schema.SessionEntry {
	entries := make([]schema.SessionEntry, count)
	for i := range entries {
		preview := strings.Repeat("assistant explanation segment ", 8) + fmt.Sprintf("record %d", i)
		entries[i] = schema.SessionEntry{
			SessionID:      id,
			EntryIndex:     i,
			Harness:        ingest.HarnessClaudeCode,
			EntryType:      schema.EntryTypeText,
			Role:           schema.RoleAssistant,
			ContentPreview: &preview,
			TimestampMs:    int64Ptr(1700000000000 + int64(i)),
		}
	}
	return entries
}
