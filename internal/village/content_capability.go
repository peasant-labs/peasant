package village

import "github.com/peasant-labs/schema"

// SupportsObservedModel reports whether the destination advertises the exact
// schema-owned observed-model contract token. Unknown and duplicate tokens are
// tolerated by this discovery reader and cannot authorize richer output.
func SupportsObservedModel(advertisements []schema.ContentCapability) bool {
	seen := make(map[schema.ContentCapability]struct{}, len(advertisements))
	for _, advertisement := range advertisements {
		if _, duplicate := seen[advertisement]; duplicate {
			continue
		}
		seen[advertisement] = struct{}{}
		if advertisement == schema.ContentCapabilityObservedModelV1 {
			return true
		}
	}
	return false
}
