package defaults

import "github.com/peasant-labs/schema"

// PublishSchemaVersion is the CURRENT push CONTENT contract version peasant emits
// (the TranscriptContent envelope + embedded SessionDetailPayload shape). It is
// the single source of truth for the push wire version — both the envelope's
// ContractVersion and the embedded SchemaVersion are stamped from this constant
// in lockstep. Typed as schema.PushContractVersion (newtype) so the version
// flows typed across the codebase, mirroring the ingest.CurrentIndexVersion
// precedent. Bump for breaking changes; minor/patch for additions.
//
// 0.1.1: PATCH bump signalling the publish contract strengthening
// (ModelInfo.Harness/.Model + PublishRequest.Model became `required` in
// VillageAPIVersion 0.3.0). Non-breaking — compliant clients already send
// harness/model — so MinPushContractVersion stays 0.1.0 (acceptance window
// [0.1.0, 0.1.1]). The village's ADVERTISED Current must move to 0.1.1 in lockstep
// or negotiate.go classifies a 0.1.1-emitting client as AheadOfCurrent
// against a still-0.1.0 village.
const PublishSchemaVersion schema.PushContractVersion = "0.1.1"

// MinPushContractVersion is the OLDEST push contract version the village must
// still ACCEPT on the publish path (the push-acceptance floor, advertised in
// SchemaVersionResponse). It is distinct from the village's display
// migrate-on-read floor: the village may render stored blobs older than
// this, but will not accept a fresh push below it. Held equal to the current
// version until a breaking contract change introduces a back-compat window.
const MinPushContractVersion schema.PushContractVersion = "0.1.0"

// PullContractVersion is the CURRENT pull ENVELOPE contract version the village
// advertises and the CLI negotiates against (the PullTranscriptInfo /
// PullListResponse / PullAnnotation shapes + /api/v1/pull/* endpoint semantics +
// ETag behaviour). It versions the ENVELOPE ONLY — never the stored blob, which
// carries its own publish-time PushContractVersion. Distinct window from the push
// contract because pull is where the village's migrate-on-read floor becomes
// wire-visible. Typed as schema.PushContractVersion (the shared semver newtype)
// so the version flows typed; the PullContractVersion NAME disambiguates the axis.
const PullContractVersion schema.PushContractVersion = "0.1.0"

// MinPullContractVersion is the OLDEST pull envelope version the village still
// supports on the pull surface (the pull-negotiation floor, advertised in
// SchemaVersionResponse alongside PullContractVersion). The CLI treats an ABSENT
// advertisement as "village too old for pull" (actionable error). Held equal to
// the current version until a breaking pull-envelope change introduces a window.
const MinPullContractVersion schema.PushContractVersion = "0.1.0"
