package defaults

import (
	"os"
	"strings"
)

// CommonsWebURL is a typed commons WEB URL (the browser-facing host that serves
// the Village frontend, e.g. the privacy notice page). It is DISTINCT from
// VillageURL: DefaultVillageURL resolves to the API host
// (https://api.village.peasantlabs.org), which serves the wire API and does not
// serve the notice page. A user-facing link must point at the web host, so it
// cannot derive from the village URL.
type CommonsWebURL string

func (u CommonsWebURL) String() string { return string(u) }

// productionCommonsWebURL is the fallback web host when PEASANT_COMMONS_URL is
// unset. It matches the web app's own NEXT_PUBLIC_COMMONS_URL default.
const productionCommonsWebURL = "https://village.peasantlabs.org"

// DefaultCommonsWebURL is the commons web URL resolved at init. Prefer
// ResolveCommonsWebURL (or CommonsNoticeURL) when the environment may change
// after init, e.g. in tests with t.Setenv.
var DefaultCommonsWebURL = ResolveCommonsWebURL()

// ResolveCommonsWebURL returns the commons web URL resolved from the
// PEASANT_COMMONS_URL environment variable at call time, falling back to the
// production web host when unset. Mirrors ResolveVillageURL.
func ResolveCommonsWebURL() CommonsWebURL {
	if u := os.Getenv("PEASANT_COMMONS_URL"); u != "" {
		return CommonsWebURL(u)
	}
	return CommonsWebURL(productionCommonsWebURL)
}

// CommonsNoticeURL returns the absolute URL of the privacy notice page: the
// resolved commons web base with a single "/privacy" suffix. Any trailing "/"
// on the base is trimmed first so the result never carries a double slash.
// Resolved at call time so a caller (or test) that overrides PEASANT_COMMONS_URL
// sees the override.
func CommonsNoticeURL() string {
	base := strings.TrimRight(ResolveCommonsWebURL().String(), "/")
	return base + "/privacy"
}
