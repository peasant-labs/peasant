package store

import "zombiezen.com/go/sqlite/sqlitex"

// PoolForTest exposes the connection pool for test assertions.
// This function is only available in test builds.
func (s *Store) PoolForTest() *sqlitex.Pool {
	return s.pool
}
