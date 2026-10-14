package store

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/schema"
)

func TestContentHarvestENOSPC(t *testing.T) {
	for _, c := range loadContentEnospcMigrationCases(t) {
		if c.Owner == "migration" {
			// Validated by the loader and run by TestContentMigrationENOSPC.
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			cfg := ingest.WriteConfig{BatchBytes: c.BatchBytes}.WithDefaults(1)
			s, _ := openGenerationStoreWith(t, WithWriteConfig(cfg))
			sid := gcSession(t, s, "e8e8e8e8-e8e8-48e8-88e8-e8e8e8e8e8e8")
			old, oldBlobs := crashCandidate(t, sid, "gen_full_old", "enospcold")
			if err := activateTestGeneration(t, s, old, oldBlobs); err != nil {
				t.Fatal(err)
			}
			if _, err := s.SweepSession(t.Context(), sid); err != nil {
				t.Fatal(err)
			}
			candidate, blobs := crashCandidate(t, sid, "gen_full_new", "enospcnew")
			var restore func()
			t.Cleanup(func() {
				clearHarmonizedFault()
				contentSweepSeam = nil
				if restore != nil {
					restore()
				}
			})
			switch c.Step {
			case "stage":
				if c.TextBytes <= 0 || c.BatchBytes <= 0 {
					t.Fatal("stage disk-full case needs textBytes and batchBytes")
				}
				candidate, blobs = buildTestGeneration(t, sid, "gen_full_new", strings.Repeat("x", c.TextBytes), strings.Repeat("y", c.TextBytes), strings.Repeat("z", c.TextBytes))
				harmonizedWriterSeam = func(stage string) error {
					if stage == harmonizedSeamBetweenStageTxns && restore == nil {
						restore = capMigratePages(t, s, 0)
					}
					return nil
				}
				_, err := s.StageGeneration(t.Context(), GenerationActivation{Generation: candidate, Blobs: blobs})
				assertMigrateFullError(t, err)
				clearHarmonizedFault()
				if got := orphanContentCount(t, s, sid); got == 0 {
					t.Fatal("stage failure left no durable orphan from the first transaction")
				}
			case "commit":
				candidate, blobs = harvestFullCatalogCandidate(t, sid, c.Entries)
				a := GenerationActivation{Generation: candidate, Blobs: blobs, IndexerVersion: 1, IndexedAtMs: 1}
				handle, err := s.StageGeneration(t.Context(), a)
				if err != nil {
					t.Fatal(err)
				}
				a.Prepared = handle
				restore = capMigratePages(t, s, 0)
				_, err = s.ActivateGeneration(t.Context(), a)
				assertMigrateFullError(t, err)
				if got := queryMigrateInt(t, s, `SELECT COUNT(*) FROM session_generations WHERE generation_id = 'gen_full_new'`); got != 0 {
					t.Fatal("disk-full activation left a partial catalog")
				}
			case "sweep":
				if err := activateTestGeneration(t, s, candidate, blobs); err != nil {
					t.Fatal(err)
				}
				before := orphanContentCount(t, s, sid)
				contentSweepSeam = func(stage string) error {
					if stage == contentSweepSeamFirstWrite {
						return sqlite.ResultFull.ToError()
					}
					return nil
				}
				_, err := s.SweepSession(t.Context(), sid)
				assertMigrateFullError(t, err)
				if !strings.Contains(err.Error(), "retry harvest") {
					t.Fatalf("sweep full error is not actionable: %v", err)
				}
				if orphanContentCount(t, s, sid) != before {
					t.Fatal("first-delete failure changed stored content")
				}
				contentSweepSeam = nil
			default:
				t.Fatalf("unknown harvest disk-full step %s", c.Step)
			}
			if !readSweepFlag(t, s, sid) {
				t.Fatal("disk-full operation cleared its recovery marker")
			}
			if c.Step != "sweep" {
				if visibleGeneration(t, s, sid) != "gen_full_old" {
					t.Fatal("disk-full operation replaced the old authority")
				}
				assertHarmonizedContent(t, s, sid, "gen_full_old", old, oldBlobs)
			}
			if restore != nil {
				restore()
				restore = nil
			}
			if _, warnings, err := s.SweepFlaggedSessionsForHarvest(context.Background()); err != nil || len(warnings) != 0 {
				t.Fatalf("next harvest orphan recovery: %v %v", err, warnings)
			}
			if orphanContentCount(t, s, sid) != 0 || readSweepFlag(t, s, sid) {
				t.Fatal("next harvest did not converge to zero orphans and a clear marker")
			}
			if err := activateTestGeneration(t, s, candidate, blobs); err != nil {
				t.Fatalf("retry after disk-full recovery: %v", err)
			}
			assertHarmonizedContent(t, s, sid, "gen_full_new", candidate, blobs)
		})
	}
}

func orphanContentCount(t *testing.T, s *Store, sid schema.SessionID) int64 {
	t.Helper()
	return queryMigrateInt(t, s, `SELECT COUNT(*) FROM session_entry_bodies b WHERE b.session_id = '`+string(sid)+`' AND NOT EXISTS (SELECT 1 FROM session_generation_entries m WHERE m.session_id=b.session_id AND m.body_digest=b.body_digest)`) + queryMigrateInt(t, s, `SELECT COUNT(*) FROM session_content c WHERE c.session_id = '`+string(sid)+`' AND NOT EXISTS (SELECT 1 FROM session_generation_content d WHERE d.session_id=c.session_id AND d.digest=c.digest)`)
}

func harvestFullCatalogCandidate(t *testing.T, sid schema.SessionID, count int) (indexformat.V2, map[schema.SourceEntryRef][]byte) {
	t.Helper()
	if count < 1 {
		t.Fatal("catalog disk-full case needs entries")
	}
	v2, blobs := crashCandidate(t, sid, "gen_full_new", "enospcnew")
	base := v2.Generation.Main.Entries[0]
	v2.Generation.Main.Entries = nil
	v2.Generation.Content = nil
	blobs = make(map[schema.SourceEntryRef][]byte)
	for i := range count {
		ref := schema.SourceEntryRef(fmt.Sprintf("e_full_%04d", i))
		text := fmt.Sprintf("enospcnew %04d", i)
		entry := base
		entry.EntryIndex, entry.SourceEntryRef, entry.ContentPreview = i, ref, &text
		v2.Generation.Main.Entries = append(v2.Generation.Main.Entries, entry)
		v2.Generation.Content = append(v2.Generation.Content, indexformat.ContentRecord{Ref: ref})
		blobs[ref] = []byte(text)
	}
	v2.Generation.TitleRefs = []schema.SourceEntryRef{v2.Generation.Main.Entries[0].SourceEntryRef}
	v2.Generation.Aliases = []indexformat.NativeAlias{{NativeKey: "native-0", Ref: v2.Generation.Main.Entries[0].SourceEntryRef}}
	v2.Generation.Metadata.Stats.TurnCount = count
	return v2, blobs
}
