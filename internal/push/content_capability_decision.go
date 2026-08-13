//go:build !capability_scan_negative

package push

import "github.com/peasant-labs/schema"

func missingContentCapabilities(advertised, required []schema.ContentCapability) []schema.ContentCapability {
	return schema.MissingContentCapabilities(advertised, required)
}

const capabilityScanMutation = false
