package api

import (
	"fmt"
	"sort"

	schema "github.com/peasant-labs/schema"
)

// UICapability is one Peasant-owned, revisioned UI capability token as
// advertised on GET /api/v1/config/capabilities. The wire envelope
// (schema.UICapabilitiesResponse) is owned by the schema module; token
// identity, meaning, and lifecycle are owned here.
//
// Tokens are revisioned: an incompatible change in a capability's meaning gets
// a new suffix (e.g. _v2), so a client that understands _v1 never misreads a
// _v2 server. Consumers compare tokens by exact membership and ignore unknown
// tokens.
type UICapability string

const (
	// UICapabilityCodeMapNavigationV1 makes code-map entry points discoverable
	// in persistent navigation and the command palette (the top-nav tab, the
	// "go to code map" palette command, and per-project "· map" jumps). It does
	// not gate the direct /map or /projects routes, which stay mounted
	// regardless of advertised capabilities.
	UICapabilityCodeMapNavigationV1 UICapability = "code_map_navigation_v1"
)

// allUICapabilities is the closed inventory of every token Peasant may
// advertise. Producers must not emit tokens outside it; newUICapabilitiesResponse
// enforces that at the response-construction boundary.
var allUICapabilities = map[UICapability]struct{}{
	UICapabilityCodeMapNavigationV1: {},
}

// Validate reports whether the token is a member of the closed inventory.
// The error is actionable: it names the offending token, points at the
// inventory that owns membership, and states the fix.
func (c UICapability) Validate() error {
	if _, ok := allUICapabilities[c]; !ok {
		return fmt.Errorf(
			"validate UI capability token: token %q is not in Peasant's closed capability inventory; "+
				"this happened while constructing the GET /api/v1/config/capabilities response in "+
				"internal/api/uicapability.go; it means a producer proposed a token the server does not own, "+
				"so the advertisement would be un-contracted and clients could not interpret it; "+
				"fix: advertise only tokens declared in allUICapabilities (currently %q), or add the new "+
				"revisioned token to that inventory before emitting it",
			string(c), string(UICapabilityCodeMapNavigationV1),
		)
	}
	return nil
}

// uiCapabilitiesForConfig derives the advertised capability set from the server
// configuration. This is the single flag->token policy point: future flags or
// tokens extend exactly this function, and nothing else decides which tokens a
// running server advertises.
//
// Currently, --experimental (ServerConfig.Experimental) advertises exactly the
// code-map navigation capability; a default server advertises nothing.
func uiCapabilitiesForConfig(cfg ServerConfig) []UICapability {
	var caps []UICapability
	if cfg.Experimental {
		caps = append(caps, UICapabilityCodeMapNavigationV1)
	}
	return caps
}

// newUICapabilitiesResponse validates every token against the closed inventory,
// removes duplicates, sorts the survivors lexicographically, and returns the
// schema-owned envelope. It errors (never silently skips) on any out-of-inventory
// token so a producer bug surfaces as an actionable construction failure rather
// than a stale or bogus advertisement. An empty input yields an empty envelope
// (the uiCapabilities field is omitted on the wire).
func newUICapabilitiesResponse(caps []UICapability) (schema.UICapabilitiesResponse, error) {
	seen := make(map[UICapability]struct{}, len(caps))
	tokens := make([]string, 0, len(caps))
	for _, c := range caps {
		if err := c.Validate(); err != nil {
			return schema.UICapabilitiesResponse{}, err
		}
		if _, dup := seen[c]; dup {
			continue
		}
		seen[c] = struct{}{}
		tokens = append(tokens, string(c))
	}
	if len(tokens) == 0 {
		return schema.UICapabilitiesResponse{}, nil
	}
	sort.Strings(tokens)
	return schema.UICapabilitiesResponse{UICapabilities: tokens}, nil
}
