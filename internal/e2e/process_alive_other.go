//go:build !unix

package e2e

// processAlive is conservative on platforms where the harness cannot probe a
// PID without signalling it. Normal testing cleanup still removes containers.
func processAlive(_ int) bool { return true }
