// Package observationinventory adapts held-file provenance to metadata fetch.
package observationinventory

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/pull"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
)

// Held selects by the canonical manifest's full VillageURL, never host alone.
// No transcript content body is read or transferred.
func Held(db *store.Store) func(context.Context, schema.CollectiveObservationScope) ([]schema.TranscriptID, error) {
	return func(ctx context.Context, scope schema.CollectiveObservationScope) ([]schema.TranscriptID, error) {
		rows, err := db.ListPulledTranscripts(ctx)
		if err != nil {
			return nil, err
		}
		ids := map[schema.TranscriptID]bool{}
		owned, err := db.HeldPublishedTranscriptIDs(ctx, scope)
		if err != nil {
			return nil, err
		}
		for _, id := range owned {
			ids[id] = true
		}
		for _, row := range rows {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			file, err := os.Open(filepath.Join(row.PullDir, pull.ManifestFilename))
			if err != nil {
				return nil, fmt.Errorf("read held transcript manifest: %w; no association prefix selected; repair the pulled inventory with an explicit re-pull", err)
			}
			raw, readErr := io.ReadAll(io.LimitReader(file, (1<<20)+1))
			closeErr := file.Close()
			if readErr != nil {
				return nil, readErr
			}
			if closeErr != nil {
				return nil, closeErr
			}
			if len(raw) > 1<<20 {
				return nil, fmt.Errorf("held transcript manifest exceeds metadata bound; no prefix selected; repair its provenance manifest")
			}
			var manifest pull.PullManifest
			if err := json.Unmarshal(raw, &manifest); err != nil {
				return nil, fmt.Errorf("decode held transcript provenance: %w; no prefix selected; explicitly re-pull to repair the manifest", err)
			}
			base, err := auth.NormalizeVillageURL(manifest.VillageURL)
			if err != nil {
				return nil, err
			}
			if base != scope.VillageURL {
				continue
			}
			if manifest.TranscriptID != row.TranscriptID {
				return nil, fmt.Errorf("held manifest identity differs from derived index; no metadata prefix selected; explicitly re-pull to repair the index")
			}
			ids[row.TranscriptID] = true
			if len(ids) > schema.MaxCollectiveFetchItems {
				return nil, fmt.Errorf("held transcript inventory exceeds metadata bound; no prefix selected; narrow held transcript inventory")
			}
		}
		result := make([]schema.TranscriptID, 0, len(ids))
		for id := range ids {
			result = append(result, id)
		}
		sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
		return result, nil
	}
}
