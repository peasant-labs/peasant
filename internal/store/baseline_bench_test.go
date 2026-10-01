package store

import (
	"fmt"
	"path/filepath"
	"testing"
)

// BenchmarkFreshDatabaseOpen measures the production Open path against a
// brand-new database file: pool creation, schema creation (migrations or the
// baseline on a fresh install), and installation-salt initialization. Each
// iteration uses a distinct path under one temporary directory so no iteration
// reuses another's file. It is the measurement vehicle for fresh-install cost;
// it is intentionally identical before and after the baseline schema lands so
// the two revisions are comparable.
func BenchmarkFreshDatabaseOpen(b *testing.B) {
	dir := b.TempDir()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// ast-grep-ignore: no-migrating-store-open-in-tests -- the benchmark's subject IS the production fresh-open path.
		s, err := Open(filepath.Join(dir, fmt.Sprintf("fresh-%d.db", i)))
		if err != nil {
			b.Fatalf("Open fresh database %d: %v", i, err)
		}
		if err := s.Close(); err != nil {
			b.Fatalf("Close fresh database %d: %v", i, err)
		}
	}
}
