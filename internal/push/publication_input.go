package push

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/transcript"
	"github.com/peasant-labs/schema"
)

// LoadPublicationInput is the default input for review and upload.
// The store composes capture evidence with current-generation facts in one
// snapshot; this layer hydrates that same generation before releasing its lock.
// A legacy input returns a nil detail. A managed error never falls back to a
// different source. No blob reads are needed after this function returns.
func LoadPublicationInput(ctx context.Context, reader ingest.PublicationInputReader, rawID string) (input ingest.PublicationInputBundle, detail *schema.SessionDetailPayload, err error) {
	return LoadPublicationInputSelected(ctx, reader, rawID, ingest.SessionReadSelection{})
}

// LoadPublicationInputSelected is the explicit-selection form of
// LoadPublicationInput. A LastGood selection bypasses only the latest-attempt
// refusal and reads the last-good authority through the unchanged active read,
// so an explicit publication can still send the last-good full transcript while
// a newer incomplete attempt is kept beside it. The selector carries only the
// last-good intent: the store resolves the generation internally, so no
// caller-supplied identifier can select a full read, and every other
// publication check still applies.
func LoadPublicationInputSelected(ctx context.Context, reader ingest.PublicationInputReader, rawID string, selection ingest.SessionReadSelection) (input ingest.PublicationInputBundle, detail *schema.SessionDetailPayload, err error) {
	id, err := ingest.NewSessionID(rawID)
	if err != nil {
		return input, nil, fmt.Errorf("load committed publication input: %w", err)
	}
	err = reader.WithCommittedPublicationInputSelected(ctx, id, selection, func(captured ingest.PublicationInputBundle) error {
		input = captured
		if captured.Readiness != ingest.PublicationReady || captured.Generation == nil {
			return nil
		}
		var hydrateErr error
		detail, hydrateErr = transcript.SnapshotToDetailValidated(ctx, *captured.Generation, reader)
		return hydrateErr
	})
	if err != nil {
		return ingest.PublicationInputBundle{}, nil, fmt.Errorf("load publication input from peasant.db before publication: %w; nothing uploaded; run peasant ingest and retry", err)
	}
	// The fully hydrated detail is the owned result. Do not expose a snapshot
	// whose protected blob lifetime ended when the callback returned.
	input.Generation = nil
	return input, detail, nil
}

// ValidatePublicationInput checks the publication policy shared by upload,
// wizard preview and Share scan. The store checks database capture integrity.
func ValidatePublicationInput(input ingest.PublicationInputBundle) error {
	// The latest-attempt refusal is reported with its own actionable reason
	// before the generic not-ready text: the caller must know that the session
	// is readable and publishable as last-good, and exactly how to proceed.
	if input.LatestAttemptPending {
		return fmt.Errorf("load publication input from peasant.db: %w; latest capture is an incomplete preview; re-index the complete source, or publish the last-good explicitly with --last-good; nothing uploaded", ErrMetadataMissing)
	}
	if input.Readiness != ingest.PublicationReady {
		return fmt.Errorf("load publication input from peasant.db: %w; metadata and indexed entries are not a verified capture; nothing uploaded; "+
			"incomplete captures require positional omission placeholders or validated retained unknown payloads with complete source coordinates, and carry partial diagnostics; "+
			"run peasant ingest with the stored source available and retry", ErrMetadataMissing)
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
