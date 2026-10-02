package store

import (
	"regexp"
	"testing"
)

// TestSchemaFingerprintContract pins the stamp contract the storetest cache
// keys on: a 12-character lowercase hex digest, deterministic within a
// build. The value itself is deliberately not asserted — it changes with
// every migration edit by design.
func TestSchemaFingerprintContract(t *testing.T) {
	t.Parallel()

	first, second := SchemaFingerprint(), SchemaFingerprint()
	if first != second {
		t.Fatalf("SchemaFingerprint is not deterministic: %q vs %q", first, second)
	}
	if matched, _ := regexp.MatchString(`^[0-9a-f]{12}$`, first); !matched {
		t.Fatalf("SchemaFingerprint %q is not 12 lowercase hex characters", first)
	}
	if CurrentSchemaVersion() != len(dbSchema.Migrations) {
		t.Fatalf("CurrentSchemaVersion %d disagrees with the migration list length %d", CurrentSchemaVersion(), len(dbSchema.Migrations))
	}
}
