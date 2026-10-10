package push

import (
	"encoding/json"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/schema"
)

// Preflight the same current request that transport sends. The frozen legacy
// validator cannot accept newly published harness identifiers such as Pi.
func buildAuthoritativeRequest(metadata, content []byte) (schema.AuthoritativePublishRequest, error) {
	return buildAuthoritativeRequestWithPolicy(metadata, content, defaults.PushMetadataDocumentCapBytes)
}

// buildAuthoritativeRequestWithPolicy is buildAuthoritativeRequest with its
// caller-owned raw-document limit injected at the input scan and at a final scan
// of the promoted request. The final scan catches growth from field promotion
// and content-hash/visibility insertion BEFORE the pinned schema raw decoder,
// whose own policy is fixed at 128 MiB. Production passes
// defaults.PushMetadataDocumentCapBytes at depth 64 - the same limit the decoder
// applies - so this seam changes no production boundary.
func buildAuthoritativeRequestWithPolicy(metadata, content []byte, limitBytes int) (schema.AuthoritativePublishRequest, error) {
	if err := schema.ScanRawJSONDocument(metadata, schema.RawJSONPathPolicy{MaxDocumentBytes: limitBytes, MaxDocumentDepth: 64}); err != nil {
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
	if err := schema.ScanRawJSONDocument(raw, schema.RawJSONPathPolicy{MaxDocumentBytes: limitBytes, MaxDocumentDepth: 64}); err != nil {
		return schema.AuthoritativePublishRequest{}, err
	}
	request, err := schema.DecodeAuthoritativePublishMetadataRaw(raw)
	if err != nil {
		return request, err
	}
	return request, schema.ValidatePublicationRequest(request)
}
