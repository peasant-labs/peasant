package village

import "fmt"

// ContentCapability is a closed identifier for an optional transcript-content
// behavior advertised by Village.
type ContentCapability string

const (
	// ContentCapabilityObservedModel guarantees exact observedModel survival
	// through Village's typed storage and re-emission paths.
	ContentCapabilityObservedModel ContentCapability = "observed_model"
)

// ContentCapabilityVersion versions one capability independently of the
// broader push envelope.
type ContentCapabilityVersion string

const ObservedModelCapabilityVersion ContentCapabilityVersion = "1.0.0"

// ContentCapabilityAdvertisement is the additive typed object returned in
// contentCapabilities by GET /api/v1/schema/version.
type ContentCapabilityAdvertisement struct {
	Capability ContentCapability        `json:"capability"`
	Version    ContentCapabilityVersion `json:"version"`
}

// Validate rejects advertisements outside the concrete contract Peasant knows
// how to consume. Unknown entries cannot accidentally authorize richer output.
func (a ContentCapabilityAdvertisement) Validate() error {
	if a.Capability != ContentCapabilityObservedModel || a.Version != ObservedModelCapabilityVersion {
		return fmt.Errorf("content capability advertisement validation failed because capability %q at version %q is outside the supported Peasant set in village.ContentCapabilityAdvertisement.Validate while negotiating GET /api/v1/schema/version; this advertisement cannot authorize enriched transcript emission; advertise %q at version %q after Village's preservation proof passes", a.Capability, a.Version, ContentCapabilityObservedModel, ObservedModelCapabilityVersion)
	}
	return nil
}

// SupportsObservedModel reports whether the exact capability/version pair is
// present. Malformed and unknown advertisements fail closed.
func SupportsObservedModel(advertisements []ContentCapabilityAdvertisement) bool {
	for _, advertisement := range advertisements {
		if advertisement.Validate() == nil {
			return true
		}
	}
	return false
}
