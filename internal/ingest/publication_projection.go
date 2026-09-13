package ingest

import (
	"context"

	"github.com/peasant-labs/schema"
)

// PublicationMetadata is the lightweight capture projection for lists. Error
// belongs to one requested session; database read failures fail the whole call.
type PublicationMetadata struct {
	Metadata        schema.UnifiedMetadata
	Readiness       PublicationReadiness
	CaptureRevision int64
	Error           error
}

type PublicationMetadataReader interface {
	LoadPublicationMetadata(context.Context, []SessionID) (map[SessionID]PublicationMetadata, error)
}
