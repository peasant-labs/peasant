package store

import (
	_ "embed"
	"testing"
)

//go:embed testdata/migrations/v73_harness_check_widening.yaml
var v73Fixture []byte

//go:embed testdata/migrations/v73_harness_check_widening.manifest.yaml
var v73Manifest []byte

// TestMigrationV73WidensHarnessChecks holds V73 to a pure widening of the
// stored harness CHECK from the frozen slot-72 list to the frozen slot-73 list.
// Both lists come from testdata/stored_harness_check.yaml, not from the
// contract's current known set: a shipped migration's CHECK does not move when
// the contract grows. The test is pinned at frozenSchema(73).
func TestMigrationV73WidensHarnessChecks(t *testing.T) {
	t.Parallel()
	runHarnessCheckWidening(t, harnessCheckWidening{
		slot:            73,
		from:            storedHarnessCheckCaseNamed(t, "v72-eight-values"),
		to:              storedHarnessCheckCaseNamed(t, "v73-fourteen-values"),
		migration:       migrationV73,
		requiredObjects: loadWideningObjects(t, v73Fixture, v73Manifest, "v73_harness_check_widening"),
	})
}
