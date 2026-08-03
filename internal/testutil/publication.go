package testutil

import (
	"encoding/json"
	"fmt"

	"github.com/peasant-labs/schema"
)

// AuthoritativePublishReceipt builds a complete terminal receipt from the exact
// successor request bytes received by a test Village dependency.
func AuthoritativePublishReceipt(metadata []byte, created bool) ([]byte, error) {
	request, err := schema.DecodeAuthoritativePublishRequest(metadata)
	if err != nil {
		return nil, fmt.Errorf("decode authoritative publish request fixture: %w", err)
	}
	return AuthoritativePublishReceiptFromRequest(request, created)
}

func AuthoritativePublishReceiptFromRequest(request schema.AuthoritativePublishRequest, created bool) ([]byte, error) {
	operation, err := schema.CanonicalizePublishRequest(request)
	if err != nil {
		return nil, fmt.Errorf("canonicalize authoritative publish request fixture: %w", err)
	}
	fingerprint, err := schema.FingerprintPublishOperation(operation)
	if err != nil {
		return nil, fmt.Errorf("fingerprint authoritative publish request fixture: %w", err)
	}
	var license *schema.License
	if operation.License.License != nil {
		value := *operation.License.License
		license = &value
	}
	transcriptID, err := schema.NewTranscriptID(TestSessionUUID)
	if err != nil {
		return nil, fmt.Errorf("convert fixture session identity to transcript identity: %w", err)
	}
	receipt := schema.AuthoritativePublishResponse{
		TranscriptID:                transcriptID,
		TranscriptURL:               "https://village.example/transcripts/" + transcriptID.String(),
		Visibility:                  schema.VisibilityPrivate,
		ContentHash:                 request.ContentHash,
		RequestOperationFingerprint: fingerprint,
		Applied:                     schema.PublishAppliedState{License: license, Associations: operation.Associations.Associations, NormalizedValues: schema.PublishNormalizedValues{RootHarness: schema.Harness(request.Model.Harness), EntryHarnesses: []schema.Harness{}, DerivedTitle: nil, Visibility: schema.VisibilityPrivate, SchemaVersion: fmt.Sprint(request.Identity.SchemaVersion)}},
		BlobKey:                     "transcripts/fixture", BlobSizeBytes: 1, PublishedAt: 1, UpdatedAt: 1, Created: created,
	}
	if err := receipt.Validate(); err != nil {
		return nil, fmt.Errorf("validate authoritative publish receipt fixture: %w", err)
	}
	return json.Marshal(receipt)
}
