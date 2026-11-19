package store

import (
	"errors"
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

// runConcurrencySweepVsWriter proves the sweep and a competing writer on
// another connection serialize on the SQLite writer, at two stages:
//
//   - first-sweep-write: the competitor tries to commit after the sweep has
//     read its work and before its first delete. A deferred transaction would
//     let that commit land and then fail its delete with BUSY (a stale
//     snapshot cannot upgrade), so the competitor must instead find the
//     writer held, and it writes once the sweep has committed.
//   - after-commit-before-sweep: the competitor holds the writer when the
//     sweep starts. A deferred transaction would read and then fail its
//     delete with BUSY at once, without waiting on busy_timeout, so the sweep
//     must instead wait until the competitor commits, then sweep.
//
// In both, the sweep deletes everything it should and clears its flag.
func runConcurrencySweepVsWriter(t *testing.T, c contentConcurrencyCase) {
	s, root := openGenerationStore(t)
	writer := openCompetingWriter(t, root)
	ctx := testwait.Context(t)
	sid := gcSession(t, s, "e3e3e3e3-e3e3-43e3-83e3-e3e3e3e3e3e3")
	gcActivate(t, s, sid, "gc_contend_old", []string{"gccontendo0", "gccontendo1", "gccontendo2"}, nil)
	gcActivate(t, s, sid, "gc_contend_active", []string{"gccontendn0", "gccontendn1", "gccontendn2"}, nil)
	if !readSweepFlag(t, s, sid) {
		t.Fatal("the superseding activation did not flag the session for the sweep")
	}
	t.Cleanup(func() { contentSweepSeam = nil })

	var fired atomic.Int64
	var probeErr error
	released := make(chan error, 1)
	// The releaser owns the competing connection until it commits; this
	// cleanup runs before the connection's own, so a failed sweep never
	// closes the connection under it.
	var releaser sync.WaitGroup
	t.Cleanup(releaser.Wait)
	wantExpect := map[string]string{
		contentSweepSeamFirstWrite:  "writer-waits-then-swept",
		contentSweepSeamAfterCommit: "sweep-waits-then-swept",
	}
	if want, ok := wantExpect[c.Seam]; !ok || c.Expect != want {
		t.Fatalf("sweep contention case %q pairs seam %q with expectation %q, want %q", c.Name, c.Seam, c.Expect, want)
	}
	switch c.Seam {
	case contentSweepSeamFirstWrite:
		contentSweepSeam = func(stage string) error {
			if stage != contentSweepSeamFirstWrite || fired.Add(1) != 1 {
				return nil
			}
			probeErr = competingWriteBegin(writer, 1)
			if probeErr == nil {
				// The writer was free: commit, as the next activation would.
				probeErr = sqlitex.ExecuteTransient(writer, "COMMIT", nil)
				if probeErr == nil {
					probeErr = errors.New("the competing writer committed between the sweep's read and its first delete")
				}
			}
			return nil
		}
	case contentSweepSeamAfterCommit:
		contentSweepSeam = func(stage string) error {
			if stage != contentSweepSeamAfterCommit || fired.Add(1) != 1 {
				return nil
			}
			if err := competingWriteBegin(writer, 1); err != nil {
				return err
			}
			releaser.Add(1)
			go func() {
				defer releaser.Done()
				// The timer is the wake source for the bounded hold. It gives
				// the sweep the chance to meet the held writer; it stays well
				// under busy_timeout so a waiting sweep is never timed out.
				hold := time.NewTimer(250 * time.Millisecond)
				defer hold.Stop()
				<-hold.C
				released <- sqlitex.ExecuteTransient(writer, "COMMIT", nil)
			}()
			return nil
		}
	default:
		t.Fatalf("unknown sweep contention seam %q", c.Seam)
	}

	got, err := s.SweepSession(ctx, sid)
	if fired.Load() != 1 {
		t.Fatalf("the %s seam fired %d times, want once", c.Seam, fired.Load())
	}
	if err != nil {
		t.Fatalf("sweep beside a competing writer at %s: %v", c.Seam, err)
	}
	switch c.Seam {
	case contentSweepSeamFirstWrite:
		if sqlite.ErrCode(probeErr).ToPrimary() != sqlite.ResultBusy {
			t.Fatalf("competing writer at the sweep's first delete = %v, want BUSY: the sweep must hold the writer before it reads its work", probeErr)
		}
		if err := competingWriteBegin(writer, 1); err != nil {
			t.Fatalf("competing writer after the sweep committed: %v", err)
		}
		if err := sqlitex.ExecuteTransient(writer, "COMMIT", nil); err != nil {
			t.Fatalf("commit the competing write after the sweep: %v", err)
		}
	case contentSweepSeamAfterCommit:
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
