//go:build !capability_scan_negative

package village

import "github.com/peasant-labs/schema"

func MissingContentCapabilities(advertised, required []schema.ContentCapability) []schema.ContentCapability {
	return schema.MissingContentCapabilities(advertised, required)
}
