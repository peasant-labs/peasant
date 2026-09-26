package push

import (
	"fmt"

	"github.com/peasant-labs/schema"
)

// This file owns the OFFLINE publication-capability preflight shared by the CLI
// push command, the durable publish path, and the mounted sync scan.
//
// The split is deliberate and is the whole point of the preflight:
//
//   - ScanPublication validates a durable payload locally and derives the exact
//     capability inventory that payload requires. It makes NO network call, so
//     it can never refuse for a receiver it did not query and can never be
//     mistaken for a receiver's answer.
//   - RemoteCapabilitySupport is a receiver's freshly obtained answer. It is
//     meaningful only at the moment of a real upload, fetched immediately
//     before it, and is never cached across runs: a scan's local requirement is
//     not a statement about any receiver's support.
//
// A payload that requires a capability is either uploaded whole to a receiver
// that advertises it or refused whole. Nothing in this path removes evidence to
// make an older receiver accept it.

// PublicationScan is the offline, receiver-independent result of validating one
// durable publication payload and deriving the receiver capabilities that
// payload requires.
type PublicationScan struct {
	// Required is the exact, lexicographically sorted, deduplicated capability
	// inventory the validated payload requires from its receiver.
	Required []schema.ContentCapability
}

// RemoteCapabilitySupport is the result of a fresh capability negotiation
// against the upload target.
type RemoteCapabilitySupport struct {
	// Capabilities is the exact known set the receiver advertised. It is
	// meaningful only when Known is true; omitted and [] both mean the receiver
	// advertises no optional support.
	Capabilities []schema.ContentCapability
	// Known reports whether a fresh advertisement was actually obtained. When
	// false, the receiver's support could not be established — the negotiation
	// was unavailable, returned no body, or was malformed — and no payload that
	// requires a capability may be uploaded on an assumption.
	Known bool
	// Unavailable carries the transport or decode failure that left support
	// unknown. It is nil whenever Known is true.
	Unavailable error
}

// ScanPublication validates content locally and derives the exact receiver
// capabilities it requires.
//
// It performs NO network access: the receiver is deliberately left unchecked.
// An invalid payload is returned as an error before any requirement is
// reported, so a caller cannot upload on a payload that failed local
// validation. The error names the failed module/function, the step, the
// consequence for the caller, and the recovery.
func ScanPublication(content schema.TranscriptContent) (PublicationScan, error) {
	if content.SessionDetail == nil {
		return PublicationScan{}, fmt.Errorf("publication scan failed at push.ScanPublication during local capability derivation: the transcript envelope carries no sessionDetail, so the payload cannot be validated or attributed; nothing was scanned, no receiver was contacted, and no bytes were uploaded; rebuild the durable payload from its capture and retry")
	}
	if err := schema.ValidateSessionDetailPayload(*content.SessionDetail); err != nil {
		return PublicationScan{}, fmt.Errorf("publication scan failed at push.ScanPublication during local durable validation: %w; nothing was scanned and no receiver was contacted; repair the local capture and retry", err)
	}
	return PublicationScan{Required: requiredContentCapabilities(content)}, nil
}

// requiredContentCapabilities derives the receiver capability inventory a
// durable payload requires. The payload must already have passed the producer
// trust boundary; this is the pure derivation half of the scan.
func requiredContentCapabilities(content schema.TranscriptContent) []schema.ContentCapability {
	if content.SessionDetail == nil {
		return nil
	}
	return schema.RequiredContentCapabilities(*content.SessionDetail)
}

// CapabilityShortfall returns the exact required tokens a receiver's
// advertisement does not cover. Unknown advertised tokens never satisfy a
// requirement, and a later revision token never satisfies the v1 token it would
// replace.
func CapabilityShortfall(advertised, required []schema.ContentCapability) []schema.ContentCapability {
	return missingContentCapabilities(advertised, required)
}

// Shortfall returns the tokens this receiver's advertisement does not cover.
// When support is unknown it reports every required token: without a freshly
// obtained advertisement, no optional capability can be assumed.
func (s RemoteCapabilitySupport) Shortfall(required []schema.ContentCapability) []schema.ContentCapability {
	if !s.Known {
		return schema.KnownContentCapabilities(required)
	}
	return CapabilityShortfall(s.Capabilities, required)
}

// UnsupportedReason is the "why:" line for a capability refusal. It separates a
// receiver that answered and lacks support from a preflight that could not be
// completed, because the two are different facts with different recoveries.
func (s RemoteCapabilitySupport) UnsupportedReason() string {
	if !s.Known {
		if s.Unavailable != nil {
			return fmt.Sprintf("the receiver's capability advertisement could not be obtained: %v", s.Unavailable)
		}
		return "the receiver's capability advertisement could not be obtained"
	}
	return "the target Village did not advertise the exact required capability tokens"
}

// capabilityRefusalRecovery is the "fix:" line for a capability refusal. It
// names the recovery for the fact that actually caused the refusal: an
// unanswered preflight is fixed by restoring the connection and retrying, while
// a receiver that answered without support is fixed only by a receiver whose
// preservation proof covers the tokens.
func capabilityRefusalRecovery(support RemoteCapabilitySupport) string {
	if !support.Known {
		return "restore connectivity to the Village and retry so a fresh capability advertisement can be read; do not publish without confirming the receiver preserves this evidence"
	}
	return "use a Village target that advertises these capabilities after its preservation proof passes, then retry"
}
