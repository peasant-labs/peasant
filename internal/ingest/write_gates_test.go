package ingest

import (
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
)

// TestWriteHoldUnderBusyTimeout asserts the writer-lane invariant against the
// open path's busy_timeout: no resolved configuration may hold the single
// SQLite writer past the wait every other process honors, or every other
// process stalls past its wait. It pins the relation, not the constants, so
// a future default that outgrows the wait fails here rather than in
// production.
func TestWriteHoldUnderBusyTimeout(t *testing.T) {
	t.Parallel()
	for _, workers := range []int{1, 8} {
		cfg, err := WriteConfig{}.validatedWithDefaults(workers)
		if err != nil {
			t.Fatalf("workers=%d: the shipped budgets must resolve: %v", workers, err)
		}
		if cfg.HoldTarget > defaults.SQLiteBusyTimeout {
			t.Fatalf("workers=%d: hold %s exceeds the open path's busy_timeout %s",
				workers, cfg.HoldTarget, defaults.SQLiteBusyTimeout)
		}
	}
	// A hold exactly at the wait stays valid and still honors the invariant:
	// the lane may equal the wait, never exceed it.
	atLimit, err := WriteConfig{HoldTarget: defaults.SQLiteBusyTimeout}.validatedWithDefaults(8)
	if err != nil {
		t.Fatalf("a hold at the busy_timeout must resolve: %v", err)
	}
	if atLimit.HoldTarget != defaults.SQLiteBusyTimeout {
		t.Fatalf("HoldTarget=%s, want the configured %s", atLimit.HoldTarget, defaults.SQLiteBusyTimeout)
	}
}
