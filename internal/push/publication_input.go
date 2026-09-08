package push

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
)

// LoadReadyPublicationInput is the shared publication and preview boundary.
// Recovery belongs to normal ingest, never to a publication-time file fallback.
func LoadReadyPublicationInput(ctx context.Context, reader ingest.PublicationInputReader, rawID string) (ingest.PublicationInputBundle, error) {
	id, err := ingest.NewSessionID(rawID)
	if err != nil {
		return ingest.PublicationInputBundle{}, fmt.Errorf("load publication input: %w", err)
	}
	input, err := reader.LoadPublicationInput(ctx, id)
	if err != nil {
		return ingest.PublicationInputBundle{}, fmt.Errorf("load publication input from peasant.db before publication: %w; nothing uploaded; run peasant ingest and retry", err)
	}
	if input.Readiness != ingest.PublicationReady {
		return ingest.PublicationInputBundle{}, fmt.Errorf("load publication input from peasant.db: %w; metadata and indexed entries are not a verified capture; nothing uploaded; run peasant ingest with the retained source available and retry", ErrMetadataMissing)
	}
	if input.Metadata.Model == "" {
		return ingest.PublicationInputBundle{}, fmt.Errorf("load publication input from peasant.db: %w; nothing uploaded; run peasant ingest with source model evidence and retry", ErrNoModel)
	}
	return input, nil
}
