package store

import (
	"github.com/peasant-labs/peasant/internal/ingest"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// PoolForTest exposes the connection pool for test assertions.
// This function is only available in test builds.
func (s *Store) PoolForTest() *sqlitex.Pool {
	return s.pool
}

// SetSessionMembershipChunk installs a per-store replacement for one chunk's
// membership read. It exists so a test can fail a LATER chunk after earlier
// chunks succeeded, which is how the all-or-nothing contract is exercised
// against the real chunk loop. Production never calls it.
// This function is only available in test builds.
func (s *Store) SetSessionMembershipChunk(fn func(conn *sqlite.Conn, chunk []ingest.SessionID, record func(ingest.SessionID)) error) {
	s.sessionMembershipChunk = fn
}

// SessionsWithoutEntriesChunkSizeForTest exposes the membership-read chunk size
// so a fixture can assert that its generated population spans more than one
// chunk instead of hard-coding the boundary.
// This function is only available in test builds.
func SessionsWithoutEntriesChunkSizeForTest() int {
	return sessionsWithoutEntriesChunkSize
}
