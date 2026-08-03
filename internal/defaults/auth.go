package defaults

import (
	"os"
	"time"
)

// CredentialsFileName is a typed credentials file name.
type CredentialsFileName string

func (f CredentialsFileName) String() string { return string(f) }

// CredentialsFile is the name of the credentials file stored in the config directory.
const CredentialsFile CredentialsFileName = "credentials.json"

// VillageURL is a typed village URL.
type VillageURL string

func (u VillageURL) String() string { return string(u) }

// productionVillageURL is the fallback when PEASANT_VILLAGE_URL is not set.
const productionVillageURL = "https://api.village.peasantlabs.org"

// LocalVillageURL is the village URL for local development (make up).
const LocalVillageURL VillageURL = "https://localhost:8443"

// DefaultVillageURL is the village server URL, resolved from the
// PEASANT_VILLAGE_URL environment variable. Falls back to the production
// village URL when unset.
var DefaultVillageURL = ResolveVillageURL()

// ResolveVillageURL returns the village URL resolved from the
// environment at call time. Use this instead of DefaultVillageURL when the
// environment may have changed after init (e.g. in tests with t.Setenv).
func ResolveVillageURL() VillageURL {
	if u := os.Getenv("PEASANT_VILLAGE_URL"); u != "" {
		return VillageURL(u)
	}
	return VillageURL(productionVillageURL)
}

// LoginCallbackPort is the preferred port for the local OAuth callback server.
const LoginCallbackPort = 17249

// LoginTimeout is how long the login flow waits for the browser callback.
const LoginTimeout = 5 * time.Minute

// LoginHTTPTimeout is the HTTP client timeout for auth-related requests.
const LoginHTTPTimeout = 10 * time.Second
