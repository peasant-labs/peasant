package push

import (
	"encoding/json"

	"github.com/peasant-labs/schema"
)

// Preflight the same current request that transport sends. The frozen legacy
// validator cannot accept newly published harness identifiers such as Pi.
func buildAuthoritativeRequest(metadata, content []byte) (schema.AuthoritativePublishRequest, error) {
	if err := schema.ScanRawJSONDocument(metadata, schema.RawJSONPathPolicy{MaxDocumentBytes: 4 << 20, MaxDocumentDepth: 64}); err != nil {
		return schema.AuthoritativePublishRequest{}, err
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(metadata, &document); err != nil {
		return schema.AuthoritativePublishRequest{}, err
	}
	if err := promoteAuthoritativePublishFields(document); err != nil {
		return schema.AuthoritativePublishRequest{}, err
	}
	hash, err := json.Marshal(schema.ComputeTranscriptContentHash(content))
	if err != nil {
		return schema.AuthoritativePublishRequest{}, err
	}
	visibility, err := json.Marshal(schema.VisibilityIntentPrivate)
	if err != nil {
		return schema.AuthoritativePublishRequest{}, err
	}
	document["contentHash"] = hash
	document["visibilityIntent"] = visibility
	raw, err := json.Marshal(document)
	if err != nil {
		return schema.AuthoritativePublishRequest{}, err
	}
	request, err := schema.DecodeAuthoritativePublishMetadataRaw(raw)
	if err != nil {
		return request, err
	}
	return request, schema.ValidatePublicationRequest(request)
}
