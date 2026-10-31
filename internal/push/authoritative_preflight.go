package push

import (
	"encoding/json"
	"strings"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/schema"
)

// Preflight the same current request that transport sends. The frozen legacy
// validator cannot accept newly published harness identifiers such as Pi.
func buildAuthoritativeRequest(metadata, content []byte) (schema.AuthoritativePublishRequest, error) {
	return buildAuthoritativeRequestWithPolicy(metadata, content, defaults.PushMetadataDocumentCapBytes, defaults.Version)
}

// authoritativeClient names this program and its version on the publish
// request, so village records which client built a publish. The version is the
// running binary's version without a leading "v". It returns nil when that is
// not a valid client version, such as the "dev" placeholder of a build with no
// version: village refuses the whole publish on an invalid client, and the
// field is optional. The client is outside the publish fingerprint, so an
// upgrade does not make a held transcript look changed.
func authoritativeClient(version defaults.AppVersion) *schema.AuthoritativeClientInfo {
	client := schema.AuthoritativeClientInfo{
		Name:    string(defaults.AppName),
		Version: strings.TrimPrefix(version.String(), "v"),
	}
	if client.Validate() != nil {
		return nil
	}
	return &client
}

// buildAuthoritativeRequestWithPolicy is buildAuthoritativeRequest with its
// caller-owned raw-document limit injected at the input scan and at a final scan
// of the promoted request, and with the application version injected for the
// client field. The final scan catches growth from field promotion and
// content-hash/visibility/client insertion BEFORE the pinned schema raw decoder,
// whose own policy is fixed at 128 MiB. Production passes
// defaults.PushMetadataDocumentCapBytes at depth 64 - the same limit the decoder
// applies - so this seam changes no production boundary.
func buildAuthoritativeRequestWithPolicy(metadata, content []byte, limitBytes int, version defaults.AppVersion) (schema.AuthoritativePublishRequest, error) {
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
	delete(document, "client")
	if client := authoritativeClient(version); client != nil {
		encoded, err := json.Marshal(client)
		if err != nil {
			return schema.AuthoritativePublishRequest{}, err
		}
		document["client"] = encoded
	}
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
