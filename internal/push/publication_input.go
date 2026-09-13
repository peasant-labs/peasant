package push

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
)

// LoadPublicationInput reads a coherent database capture. Callers must explicitly
// validate its publication eligibility with ValidatePublicationInput before use.
// Recovery belongs to normal ingest, never to a publication-time file fallback.
func LoadPublicationInput(ctx context.Context, reader ingest.PublicationInputReader, rawID string) (ingest.PublicationInputBundle, error) {
	id, err := ingest.NewSessionID(rawID)
	if err != nil {
		return ingest.PublicationInputBundle{}, fmt.Errorf("load publication input: %w", err)
	}
	input, err := reader.LoadPublicationInput(ctx, id)
	if err != nil {
		return ingest.PublicationInputBundle{}, fmt.Errorf("load publication input from peasant.db before publication: %w; nothing uploaded; run peasant ingest and retry", err)
	}
	return input, nil
}

// ValidatePublicationInput checks the publication policy shared by upload,
// wizard preview and Share scan. The store checks database capture integrity.
func ValidatePublicationInput(input ingest.PublicationInputBundle) error {
	if input.Readiness != ingest.PublicationReady {
		return fmt.Errorf("load publication input from peasant.db: %w; metadata and indexed entries are not a verified capture; nothing uploaded; "+
			"the one incompleteness that may still be published is a capture whose only gap is oversized source records that ingest omitted, which travels with its placeholders and its partial diagnostics; "+
			"run peasant ingest with the retained source available and retry", ErrMetadataMissing)
	}
	if input.Metadata.Model == "" {
		return fmt.Errorf("load publication input from peasant.db: %w; nothing uploaded; run peasant ingest with source model evidence and retry", ErrNoModel)
	}
	return nil
}

// LoadPublicationMetadata prepares list candidates for one lightweight store
// read. Invalid IDs remain unavailable; they cannot broaden the requested set.
func LoadPublicationMetadata(ctx context.Context, reader ingest.PublicationMetadataReader, rows []ingest.PushSessionRow) (map[string]ingest.PublicationMetadata, error) {
	ids := make([]ingest.SessionID, 0, len(rows))
	for _, row := range rows {
		id, err := ingest.NewSessionID(row.SessionID)
		if err == nil {
			ids = append(ids, id)
		}
	}
	projections, err := reader.LoadPublicationMetadata(ctx, ids)
	if err != nil {
		return nil, err
	}
	result := make(map[string]ingest.PublicationMetadata, len(projections))
	for id, projection := range projections {
		result[id.String()] = projection
	}
	return result, nil
}

// PublicationMetadataReady adds the consumer's mandatory model check to the
// store's capture-integrity verdict, just as ValidatePublicationInput does.
func PublicationMetadataReady(value ingest.PublicationMetadata) bool {
	return value.Error == nil && value.Readiness == ingest.PublicationReady && value.Metadata.Model != ""
}
