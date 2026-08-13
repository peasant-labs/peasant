//go:build capability_scan_negative

package push

import "github.com/peasant-labs/schema"

func missingContentCapabilities(advertised, required []schema.ContentCapability) []schema.ContentCapability {
	for _, token := range advertised {
		if len(schema.KnownContentCapabilities([]schema.ContentCapability{token})) == 0 {
			return required
		}
	}
	return schema.MissingContentCapabilities(advertised, required)
}

const capabilityScanMutation = true
