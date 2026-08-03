package pull

import "github.com/peasant-labs/schema"

// ManifestFilename is the on-disk name of the per-transcript provenance manifest
// written into each pull directory (village-pulls/{host}/{id}/pull-manifest.json).
const ManifestFilename = "pull-manifest.json"

// TranscriptFilename is the on-disk name of the served transcript blob within a
// pull directory. The blob is written exactly as the village served it.
const TranscriptFilename = "transcript.jsonl"

// MetadataFilename is the on-disk name of the village metadata snapshot
// (PullTranscriptInfo) within a pull directory.
const MetadataFilename = "metadata.json"

// PullManifestVersion is the schema version of the pull-manifest.json structure
// Distinct from the blob's push-contract version and the pull-envelope
// version it records — this versions the MANIFEST format itself. Bump on a
// breaking change to PullManifest.
const PullManifestVersion = 1

// PullManifest is the provenance record written alongside each pulled transcript
// It captures WHERE the transcript came from, WHO owns it, WHICH bytes
// were served (servedBlobHash — the pull-idempotency key), under WHICH contracts
// the blob and the pull envelope were transferred, WHEN it was pulled, and the
// content-hashes + author identities of the annotations committed with it. It is
// the on-disk source of truth that the V34 tables index; transcript context reads it for
// `transcripts context` (legacy-blob detection via BlobContractVersion) and the
// e2e harness asserts on it.
//
// It is JSON-serialized; the field order here is the documented manifest order.
type PullManifest struct {
	// ManifestVersion is PullManifestVersion at write time.
	ManifestVersion int `json:"manifestVersion"`

	// VillageURL is the full base URL the transcript was pulled from (e.g.
	// https://village.example.com). VillageHost is its host component, the
	// on-disk namespace key.
	VillageURL  string `json:"villageURL"`
	VillageHost string `json:"villageHost"`

	// TranscriptID is the village-side transcript identifier.
	TranscriptID schema.TranscriptID `json:"transcriptId"`
	// LocalSessionID is the peasant SessionID this transcript was published under,
	// when the village reported it (round-trip correlation); empty otherwise.
	LocalSessionID string `json:"localSessionId,omitempty"`

	// OwnerUserID / OwnerUsername are the village account identity of the owner.
	OwnerUserID   string `json:"ownerUserId"`
	OwnerUsername string `json:"ownerUsername"`

	// ServedETag is the VERBATIM ETag string the village returned for the served
	// blob (the village quotes it, e.g. "\"<hash>\""). It is the conditional-GET
	// token: a re-pull echoes it UNMODIFIED as If-None-Match so the server can
	// match byte-for-byte and answer 304. Empty when the village returned no ETag.
	// Distinct from ServedBlobHash (the raw unquoted content-identity hash): one field
	// is the transport token (verbatim, possibly quoted), the other the content
	// identity key (always raw).
	ServedETag string `json:"servedETag,omitempty"`

	// ServedBlobHash is the RAW (unquoted) content-identity hash of the served
	// blob bytes — the pull-idempotency key. ALWAYS raw: when the village serves a
	// quoted ETag the surrounding quotes are stripped; when it serves no ETag this
	// is the locally-computed schema.ComputeTranscriptHash(blob). This consistent
	// raw identity is what the metadata fast-path DIFF compares against the
	// village's raw PullTranscriptInfo.ContentHash, and what is persisted into
	// pulled_transcripts.content_hash. Empty only when no blob was fetched.
	ServedBlobHash string `json:"servedBlobHash,omitempty"`

	// BlobContractVersion is the PUSH content-contract version the stored blob was
	// published under (carried by the blob; the pull envelope does not version the
	// blob). Empty/unknown ⇒ a legacy pre-envelope blob (context errors
	// actionably on these). PullEnvelopeVersion is the pull-contract version the
	// transfer was negotiated under.
	BlobContractVersion schema.ContractVersion `json:"blobContractVersion,omitempty"`
	PullEnvelopeVersion schema.ContractVersion `json:"pullEnvelopeVersion"`

	// PulledAt is the unix-millis timestamp of this pull (the clock value at WRITE
	// time).
	PulledAt int64 `json:"pulledAt"`

	// Annotations records the provenance of each annotation committed with the
	// transcript: its content-hash (the dedup key) and authoring village account.
	Annotations []ManifestAnnotation `json:"annotations"`
}

// ManifestAnnotation is the per-annotation provenance entry in a PullManifest:
// the content-hash (pulled_annotations dedup key) plus the authoring village
// account identity.
type ManifestAnnotation struct {
	ContentHash    string `json:"contentHash"`
	AuthorUserID   string `json:"authorUserId"`
	AuthorUsername string `json:"authorUsername"`
}
