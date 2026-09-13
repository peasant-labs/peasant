package store_test

import "testing"

func TestMigrationV50AddsNullableSourceFingerprint(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	if got := queryInt(t, conn, `PRAGMA user_version`); got < 50 {
		t.Fatalf("user_version: expected >= 50, got %d", got)
	}
	if got := queryInt(t, conn, `SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name='source_fingerprint' AND type='BLOB' AND "notnull"=0`); got != 1 {
		t.Fatalf("nullable sessions.source_fingerprint column count=%d, want 1", got)
	}
}
