package store

import (
	"crypto/sha256"
	"fmt"
)

// SchemaFingerprint is a short hex digest of the migration list (each
// migration's position and SQL text) that keys the storetest golden-template
// cache. The schema version alone (the migration count) guards against
// released migrations, but a branch that edits a migration's SQL at the same
// version would otherwise reuse a stale cached template; the fingerprint makes
// that impossible while staying cheap (one hash over embedded strings).
//
// The returned string is the first 12 hex characters of the SHA-256 digest
// (48 bits). It is a cache key, not a security boundary.
func SchemaFingerprint() string {
	h := sha256.New()
	for i, migration := range dbSchema.Migrations {
		fmt.Fprintf(h, "V%d\x00%d\x00%s\x00", i+1, len(migration), migration)
	}
	return fmt.Sprintf("%x", h.Sum(nil))[:12]
}
