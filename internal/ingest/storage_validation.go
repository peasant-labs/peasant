package ingest

import (
	"fmt"

	"github.com/peasant-labs/schema"
)

// ValidateEntriesForStorage is the shared storage validator both writers call:
// the prepare phase (P1) and the store boundary (S0) run it over the same
// entries, so hostile input stops before any object is staged. It refuses a
// carrier row that is not a well-formed private Pi row (system role and type
// with no searchable content or token counts) and an entry that names a
// session other than the one being written. Peasant always computes digests
// over the bytes it stores and never reads one from input, so a forged
// content claim cannot pass this boundary either: the digest is recomputed
// from the stored columns on every full read.
func ValidateEntriesForStorage(sessionID SessionID, entries []schema.SessionEntry) error {
	for i := range entries {
		entry := entries[i]
		if entry.SessionID != SessionID(sessionID) {
			return fmt.Errorf("store carrier validation failed during index replacement: entry %d names session %s, not the written session %s; existing entries were not replaced; build entries for the owning session", entry.EntryIndex, entry.SessionID, sessionID)
		}
		if _, _, err := DecodePiEntryExtra(entry); err != nil {
			return err
		}
		if !IsPiCarrier(entry) {
			continue
		}
		if _, pi, err := DecodePiExtra(entry.Extra); err != nil || !pi || entry.Role != schema.RoleSystem || entry.EntryType != schema.EntryTypeSystem || entry.ContentPreview != nil || entry.ToolInput != nil || entry.ToolOutput != nil || entry.TokensIn != nil || entry.TokensOut != nil {
			return fmt.Errorf("store carrier validation failed during index replacement: private Pi rows must have system role/type and no searchable content or token counts (decode: %v); existing entries were not replaced; repair the Pi indexer and re-index", err)
		}
	}
	return nil
}
