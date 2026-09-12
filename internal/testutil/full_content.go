package testutil

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// FullContentWriter is the production atomic capture-write boundary.
type FullContentWriter interface {
	IndexSessionEntryBatch(context.Context, []ingest.SessionEntryWrite) []ingest.SessionEntryWriteResult
}

// WriteFullEntries seeds synthetic complete source entries, never bounded previews.
//
// The stored index format is the one this harness DECLARES in
// HarvesterVersionRegistry rather than a literal, so the fixture cannot keep
// claiming a format the registry stopped targeting. The harness is taken from
// the entries, which is the only place the seed states it.
func WriteFullEntries(ctx context.Context, db FullContentWriter, id ingest.SessionID, entries []schema.SessionEntry) error {
	indexVersion, err := seedIndexVersion(entries)
	if err != nil {
		return err
	}
	results := db.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{
		SessionID: id, Result: indexformat.V1{Entries: entries}, IndexVersion: indexVersion, RequireFullContent: true,
		ContentCapture: ingest.SessionContentCaptureWrite{Status: ingest.ContentCaptureComplete,
			SourceAuthority: ingest.ContentSourceNewIngest, CaptureFormat: ingest.ContentCaptureFormatFull},
	}})
	if len(results) != 1 {
		return fmt.Errorf("seed full content: expected one atomic write result")
	}
	return results[0].Err
}

// seedIndexVersion reads the stored index format this seed's harness declares.
//
// A seed that names its harness gets that harness's declared format. A seed that
// names none — an empty capture, or entries written before the field existed —
// gets the format every registered harness agrees on. The moment two harnesses
// declare different formats, which is the staggered-adoption case the registry
// exists for, there is no single right answer for a harness-less seed and this
// refuses instead of guessing one.
func seedIndexVersion(entries []schema.SessionEntry) (int, error) {
	for _, entry := range entries {
		if entry.Harness == "" {
			continue
		}
		versions, registered := ingest.HarvesterVersionRegistry[entry.Harness]
		if !registered {
			return 0, fmt.Errorf(
				"seed full content: harness %q is not in ingest.HarvesterVersionRegistry, so this seed has no declared stored index format. "+
					"Where: testutil.WriteFullEntries, arranging a synthetic full capture. When: before any production write ran. "+
					"Means: writing it would store a format no reader supports. "+
					"Fix: seed entries whose Harness is a registered harness",
				entry.Harness)
		}
		return versions.IndexVersion, nil
	}
	return agreedSeedIndexVersion()
}

// agreedSeedIndexVersion returns the stored index format shared by every
// registered harness, or an error when they have diverged.
func agreedSeedIndexVersion() (int, error) {
	agreed, decided := 0, false
	for harness, versions := range ingest.HarvesterVersionRegistry {
		if !decided {
			agreed, decided = versions.IndexVersion, true
			continue
		}
		if versions.IndexVersion != agreed {
			return 0, fmt.Errorf(
				"seed full content: this seed names no harness, and registered harnesses no longer declare one stored index format (%q declares %d, another declares %d). "+
					"Where: testutil.WriteFullEntries, arranging a synthetic full capture. When: before any production write ran. "+
					"Means: any format chosen here would be a guess about which harness recorded the seed. "+
					"Fix: set Harness on the seeded entries so the format comes from that harness's registry entry",
				harness, versions.IndexVersion, agreed)
		}
	}
	if !decided {
		return 0, fmt.Errorf(
			"seed full content: ingest.HarvesterVersionRegistry is empty, so no stored index format is declared. " +
				"Where: testutil.WriteFullEntries. When: before any production write ran. " +
				"Means: there is nothing to seed against. Fix: register the harness this seed represents")
	}
	return agreed, nil
}
