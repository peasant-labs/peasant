package ingest

import (
	"errors"
	"fmt"
	"io/fs"
)

// DiscoveryCodeSourceUnreadable is the DiscoveryDiagnostic code for a source
// directory discovery could not read.
const DiscoveryCodeSourceUnreadable = "source_unreadable"

// DiscoveryCodeHarnessFailed is the DiscoveryDiagnostic code for a harness
// whose whole Discover call failed while another harness succeeded. Without it
// the run loses every session of the failed harness with no message and exits 0.
const DiscoveryCodeHarnessFailed = "harness_discovery_failed"

// failedProviderDiagnostic reports a provider-level discovery failure on the run
// result, so a partial run says which harness it could not read instead of
// silently returning the other harnesses' sessions.
func failedProviderDiagnostic(harness Harness, err error) DiscoveryDiagnostic {
	return DiscoveryDiagnostic{
		Provider: harness,
		Code:     DiscoveryCodeHarnessFailed,
		Location: string(harness),
		Summary:  "discovery failed for this harness",
		Detail:   fmt.Sprintf("%s; no session from this harness was discovered this run; the run continued with the other harnesses", firstLine(err.Error())),
	}
}

// unreadableSourceDiagnostic turns a permission error met while walking a
// source tree into a discovery diagnostic. Other walk errors (a file removed
// mid-walk, a missing root) stay silent as before.
func unreadableSourceDiagnostic(harness Harness, path string, err error) (DiscoveryDiagnostic, bool) {
	if !errors.Is(err, fs.ErrPermission) {
		return DiscoveryDiagnostic{}, false
	}
	return DiscoveryDiagnostic{
		Provider: harness,
		Code:     DiscoveryCodeSourceUnreadable,
		Location: path,
		Summary:  "unreadable source directory",
		Detail:   fmt.Sprintf("%v; sessions under it were not read; fix its permissions and rerun harvest", err),
	}, true
}
