package store

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// openUpgraded is the two-step path every command takes on a store that may
// be behind: the explicit upgrade, then Open. A migration test builds a
// store at an old schema and uses it where it once relied on Open to
// migrate. On a missing, new or current store it is Open alone.
func openUpgraded(path string, opts ...OpenOption) (*Store, error) {
	if _, err := Upgrade(context.Background(), path, UpgradeOptions{}); err != nil {
		return nil, err
	}
	return Open(path, opts...)
}

// searchRepairIsLong reports whether the upgrade plan of the store at path
// classifies the search trigger repair as a long step. A plan without that
// step reports false.
func searchRepairIsLong(t *testing.T, path string) (bool, error) {
	t.Helper()
	plan, err := PlanUpgrade(path)
	if err != nil {
		return false, err
	}
	for _, step := range plan.Steps {
		if step.Version == searchRepairSchemaVersion {
			return step.Cost.Long(), nil
		}
	}
	return false, nil
}

// backfillConversationTurnCountsForTest runs the fold on one pooled
// connection, as the upgrade step does inside its transaction.
func backfillConversationTurnCountsForTest(t *testing.T, s *Store) (ConversationTurnCountBackfillReport, error) {
	t.Helper()
	conn, err := s.Pool().Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Pool().Put(conn)
	return backfillConversationTurnCountsOnConn(conn, nil)
}

// openBehindStoreForTest wraps a store that is behind the head schema in a
// Store without upgrading it, which Open never does. A migration test uses
// it to seed rows through the store's own writers at an old schema, before
// the upgrade under test runs. Only the writers that exist at that schema
// work; the caller closes the store before it upgrades.
func openBehindStoreForTest(t *testing.T, path string) *Store {
	t.Helper()
	pool, err := newSQLitePool(path, sqlitex.PoolOptions{PoolSize: resolvePoolSize(0), PrepareConn: preparePragmas})
	if err != nil {
		t.Fatalf("open the pool of the behind store %s: %v", path, err)
	}
	formats, err := newIndexFormats(nil)
	if err != nil {
		_ = pool.Close()
		t.Fatal(err)
	}
	conversions, err := newIndexFormatConversions(nil, formats)
	if err != nil {
		_ = pool.Close()
		t.Fatal(err)
	}
	s, err := newStoreOnPool(pool, path, openOptions{}, formats, conversions)
	if err != nil {
		t.Fatalf("wrap the behind store %s: %v", path, err)
	}
	return s
}
