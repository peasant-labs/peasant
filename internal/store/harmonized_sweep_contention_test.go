package store

import (
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/testkit/testwait"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// The sweep's writer contention. The pipeline runs the after-commit sweep
// outside its write lane, so another connection (staging, the next
// activation, another peasant process) can write while the sweep runs. These
// tests put a second SQLite connection at the sweep's named stages and prove
// the sweep waits for the writer instead of failing with "database is
// locked". Nothing here runs parallel: the sweep's seam is process-global.

// competingWriterProbeTable is the scratch table the competing connection
// writes. It is unrelated to the swept session, so the only interaction
// between the two connections is the SQLite writer itself.
const competingWriterProbeTable = "sweep_competing_writer_probe"

// openCompetingWriter opens a second connection to the store's database file,
// as a writer in another goroutine or process would, and creates the probe
// table it writes. Its busy timeout is zero so that a probe for the writer
// reports at once whether the sweep holds it.
func openCompetingWriter(t *testing.T, root string) *sqlite.Conn {
	t.Helper()
	conn, err := sqlite.OpenConn(filepath.Join(filepath.Dir(root), "generations.db"), sqlite.OpenReadWrite)
	if err != nil {
		t.Fatalf("open the competing writer connection: %v", err)
	}
	t.Cleanup(func() {
		if !conn.AutocommitEnabled() {
			_ = sqlitex.ExecuteTransient(conn, "ROLLBACK", nil)
		}
		if err := conn.Close(); err != nil {
			t.Errorf("close the competing writer connection: %v", err)
		}
	})
	conn.SetBusyTimeout(0)
	if err := sqlitex.ExecuteTransient(conn, "CREATE TABLE "+competingWriterProbeTable+" (n INTEGER NOT NULL)", nil); err != nil {
		t.Fatalf("create the competing writer's probe table: %v", err)
	}
	return conn
}

// competingWriteBegin takes the SQLite writer on the competing connection and
// writes one probe row, leaving the transaction open. It returns the begin
// error unchanged so a caller can tell a held writer (BUSY) from a failure.
func competingWriteBegin(conn *sqlite.Conn, n int) error {
	if err := sqlitex.ExecuteTransient(conn, "BEGIN IMMEDIATE", nil); err != nil {
		return err
	}
	if err := sqlitex.ExecuteTransient(conn, "INSERT INTO "+competingWriterProbeTable+" (n) VALUES (?)", &sqlitex.ExecOptions{Args: []any{n}}); err != nil {
		_ = sqlitex.ExecuteTransient(conn, "ROLLBACK", nil)
		return err
	}
	return nil
}

func competingProbeRows(t *testing.T, s *Store) int64 {
	t.Helper()
	return queryMigrateInt(t, s, "SELECT COUNT(*) FROM "+competingWriterProbeTable)
}

// sweepContentionExpect is what a sweep contention case expects of the
// competing writer and the sweep, and so what the competitor does at the seam.
type sweepContentionExpect string

const (
	// sweepContentionWriterWaits: the competitor tries to commit at the seam
	// and must find the writer held by the sweep (BUSY); it writes once the
	// sweep has committed. The seam lies inside a sweep transaction that has
	// already read, so a deferred transaction would let the commit land and
	// then fail its delete with BUSY (a stale snapshot cannot upgrade).
	sweepContentionWriterWaits sweepContentionExpect = "writer-waits-then-swept"
	// sweepContentionWriterCommits: the seam lies between sweep transactions,
	// so the competitor's commit lands there and the sweep still completes.
	sweepContentionWriterCommits sweepContentionExpect = "writer-commits-then-swept"
	// sweepContentionSweepWaits: the competitor takes the writer at the seam
	// and holds it; the sweep must wait for the commit instead of failing.
	sweepContentionSweepWaits sweepContentionExpect = "sweep-waits-then-swept"
)

// sweepContentionHold is how long a holding competitor keeps the writer. It
// stays well under busy_timeout so that a waiting sweep is never timed out.
const sweepContentionHold = 250 * time.Millisecond

// runConcurrencySweepVsWriter proves the sweep and a competing writer on
// another connection serialize on the SQLite writer. The case names the sweep
// path (bounded, or the batch fallback a one-row budget forces), the seam
// where the competitor acts, and the expectation that decides what it does.
// In every case the sweep deletes everything it should and clears its flag,
// and the competitor's one write is durable beside it.
//
// The holding cases release the writer on a timer, not on a signal that the
// sweep is blocked: a sweep waiting on SQLite's busy handler has no Go-side
// observation point. So a holding case proves the sweep never fails beside a
// held writer and returns only after the release, but if the sweep were slow
// enough to reach the writer only after the timer fired, the case would pass
// without exercising the wait. The commit cases carry no timer and are exact.
func runConcurrencySweepVsWriter(t *testing.T, c contentConcurrencyCase) {
	s, root := openSweepPathStore(t, c.Path)
	writer := openCompetingWriter(t, root)
	ctx := testwait.Context(t)
	sid := gcSession(t, s, "e3e3e3e3-e3e3-43e3-83e3-e3e3e3e3e3e3")
	gcActivate(t, s, sid, "gc_contend_old", []string{"gccontendo0", "gccontendo1", "gccontendo2"}, nil)
	gcActivate(t, s, sid, "gc_contend_active", []string{"gccontendn0", "gccontendn1", "gccontendn2"}, nil)
	if !readSweepFlag(t, s, sid) {
		t.Fatal("the superseding activation did not flag the session for the sweep")
	}
	t.Cleanup(func() { contentSweepSeam = nil })

	expect := sweepContentionExpect(c.Expect)
	var fired atomic.Int64
	// probeErr is written by the seam hook, which runs on the sweep's own
	// goroutine, and read only after SweepSession returns: one goroutine, no
	// race.
	var probeErr error
	released := make(chan error, 1)
	var releasedBeforeReturn atomic.Bool
	// The releaser owns the competing connection until it commits; this
	// cleanup runs before the connection's own, so a failed sweep never
	// closes the connection under it.
	var releaser sync.WaitGroup
	t.Cleanup(releaser.Wait)
	var act func() error
	switch expect {
	case sweepContentionWriterWaits, sweepContentionWriterCommits:
		act = func() error {
			probeErr = competingWriteBegin(writer, 1)
			if probeErr == nil {
				// The writer was free: commit, as the next activation would.
				probeErr = sqlitex.ExecuteTransient(writer, "COMMIT", nil)
			}
			return nil
		}
	case sweepContentionSweepWaits:
		act = func() error {
			if err := competingWriteBegin(writer, 1); err != nil {
				return err
			}
			releaser.Add(1)
			go func() {
				defer releaser.Done()
				// The timer is the wake source for the bounded hold.
				hold := time.NewTimer(sweepContentionHold)
				defer hold.Stop()
				<-hold.C
				releasedBeforeReturn.Store(true)
				released <- sqlitex.ExecuteTransient(writer, "COMMIT", nil)
			}()
			return nil
		}
	default:
		t.Fatalf("unknown sweep contention expectation %q", c.Expect)
	}
	contentSweepSeam = func(stage string) error {
		if stage != c.Seam || fired.Add(1) != 1 {
			return nil
		}
		return act()
	}

	got, err := s.SweepSession(ctx, sid)
	returnedAfterRelease := releasedBeforeReturn.Load()
	if fired.Load() < 1 {
		t.Fatalf("the %s seam never fired on the %s path", c.Seam, c.Path)
	}
	if err != nil {
		t.Fatalf("sweep beside a competing writer at %s on the %s path: %v", c.Seam, c.Path, err)
	}
	switch expect {
	case sweepContentionWriterWaits:
		if sqlite.ErrCode(probeErr).ToPrimary() != sqlite.ResultBusy {
			t.Fatalf("competing writer at %s = %v, want BUSY: the sweep must hold the writer before it reads what it deletes", c.Seam, probeErr)
		}
		if err := competingWriteBegin(writer, 1); err != nil {
			t.Fatalf("competing writer after the sweep committed: %v", err)
		}
		if err := sqlitex.ExecuteTransient(writer, "COMMIT", nil); err != nil {
			t.Fatalf("commit the competing write after the sweep: %v", err)
		}
	case sweepContentionWriterCommits:
		if probeErr != nil {
			t.Fatalf("competing writer at %s = %v, want its commit to land between sweep transactions", c.Seam, probeErr)
		}
	case sweepContentionSweepWaits:
		if !returnedAfterRelease {
			t.Fatal("the sweep returned while the competitor still held the writer; it must wait for the release")
		}
		if err := testwait.Receive(t, released, "the competing writer's commit"); err != nil {
			t.Fatalf("commit the held competing write: %v", err)
		}
	}
	if got.RowsDeleted != 7 || got.BodiesDeleted != 3 {
		t.Fatalf("contended sweep = %+v, want 7 superseded rows and 3 bodies", got)
	}
	if readSweepFlag(t, s, sid) {
		t.Fatal("contended sweep left the flag set")
	}
	if rows := competingProbeRows(t, s); rows != 1 {
		t.Fatalf("competing writer rows = %d, want its one write durable beside the sweep", rows)
	}
	if visible := visibleGeneration(t, s, sid); visible != "gc_contend_active" {
		t.Fatalf("visible generation = %q after the contended sweep, want gc_contend_active", visible)
	}
}
