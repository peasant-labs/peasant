package ingest

import (
	"errors"
	"fmt"
	"io/fs"
)

// DiscoveryCodeSourceUnreadable is the DiscoveryDiagnostic code for a source
// directory discovery could not read.
const DiscoveryCodeSourceUnreadable = "source_unreadable"

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
