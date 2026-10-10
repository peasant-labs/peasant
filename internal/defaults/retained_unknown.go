package defaults

// RetainedUnknownPayloadCapBytes is the published transfer limit for one
// retained evidence payload. It mirrors the schema contract's retained-unknown
// per-payload raw-document bound: schema.ValidateRetainedUnknown scans each
// record.Payload with an inner 8 MiB raw-document cap at depth 64
// (schema/retained_unknown.go:72) and applies no outer document cap.
//
// Peasant states the value once here, so the in-place probe, the
// projection-time backstop, and the published refusal label cannot drift from
// the schema bound. It is deliberately separate from
// SessionDetailDocumentCapBytes: a session-detail document and one
// retained-unknown payload are different contracts, so the payload cap must not
// follow a change to the detail document cap.
const RetainedUnknownPayloadCapBytes int = 8 << 20
