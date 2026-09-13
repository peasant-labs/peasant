package ingest

import "fmt"

// A legacy slice-returning parser cannot distinguish a valid empty transcript
// from discarded malformed records or an unavailable native message tree.
type unverifiedEmptyIndexError struct {
	session DiscoveredSession
}

func (err *unverifiedEmptyIndexError) Error() string {
	return fmt.Sprintf("index %s session %s: the legacy indexer returned no entries without proof that parsing completed; empty output can also mean malformed or unavailable source records, so the previous index and producer stamps were preserved; restore readable input or use an indexer that verifies completed-empty input before retrying", err.session.Harness, err.session.SessionID)
}
