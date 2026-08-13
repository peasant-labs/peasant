//go:build capability_scan_negative

package village

import "github.com/peasant-labs/schema"

func MissingContentCapabilities(advertised, required []schema.ContentCapability) []schema.ContentCapability {
	for _, token := range advertised {
		if len(schema.KnownContentCapabilities([]schema.ContentCapability{token})) == 0 {
			return required
		}
	}
	return schema.MissingContentCapabilities(advertised, required)
}
