package store

import (
	_ "embed"
	"testing"
)

//go:embed testdata/migrations/v78_harness_check_widening.yaml
var v78Fixture []byte

//go:embed testdata/migrations/v78_harness_check_widening.manifest.yaml
var v78Manifest []byte

// TestMigrationV78WidensHarnessChecks holds V78 to a pure widening of the
// stored harness CHECK from the frozen slot-73 list to the sixteen known
// values: antigravity-cli and antigravity-ide are added, and the archived
// antigravity and gemini-cli stay. A store at schema 77 with a row for every
// stored harness migrates with every row, the parent lane,
// store_state.revision, foreign keys and every sessions index and trigger
// unchanged, and ends with the DDL of a store built fresh at schema 78. The
// test is pinned at frozenSchema(78).
func TestMigrationV78WidensHarnessChecks(t *testing.T) {
	t.Parallel()
	runHarnessCheckWidening(t, harnessCheckWidening{
		slot:            78,
		from:            storedHarnessCheckCaseNamed(t, "v73-fourteen-values"),
		to:              storedHarnessCheckCaseNamed(t, "v78-sixteen-values"),
		migration:       migrationV78,
		requiredObjects: loadWideningObjects(t, v78Fixture, v78Manifest, "v78_harness_check_widening"),
	})
}
