// Copyright (c) 2018 David Crawshaw <david@zentus.com>
// Copyright (c) 2021 Roxy Light <roxy@zombiezen.com>
//
// Permission to use, copy, modify, and distribute this software for any
// purpose with or without fee is hereby granted, provided that the above
// copyright notice and this permission notice appear in all copies.
//
// THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
// WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
// MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
// ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
// WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
// ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
// OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
//
// SPDX-License-Identifier: ISC

package sqlitex_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

const poolSize = 20

// newMemPool returns new sqlitex.Pool attached to new database opened in memory.
//
// the pool is initialized with size=poolSize.
// any error is t.Fatal.
func newMemPool(t *testing.T) *sqlitex.Pool {
	t.Helper()
	dbpool, err := sqlitex.NewPool("file::memory:?mode=memory&cache=shared", sqlitex.PoolOptions{
		Flags:    sqlite.OpenReadWrite | sqlite.OpenCreate | sqlite.OpenURI | sqlite.OpenSharedCache,
		PoolSize: poolSize,
	})
	if err != nil {
		t.Fatal(err)
	}
	return dbpool
}

func TestPool(t *testing.T) {
	dbpool := newMemPool(t)
	defer func() {
		if err := dbpool.Close(); err != nil {
			t.Error(err)
		}
	}()

	c := dbpool.Get(nil)
	c.Prep("DROP TABLE IF EXISTS footable;").Step()
	if hasRow, err := c.Prep("CREATE TABLE footable (col1 integer);").Step(); err != nil {
		t.Fatal(err)
	} else if hasRow {
		t.Errorf("CREATE TABLE reports having a row")
	}
	dbpool.Put(c)
	c = nil

	var wg sync.WaitGroup
	for i := 0; i < poolSize; i++ {
		wg.Add(1)
		go func(i int) {
			for j := 0; j < 10; j++ {
				testInsert(t, fmt.Sprintf("%d-%d", i, j), dbpool)
			}
			wg.Done()
		}(i)
	}
	wg.Wait()

	c = dbpool.Get(nil)
	defer dbpool.Put(c)
	stmt := c.Prep("SELECT COUNT(*) FROM footable;")
	if hasRow, err := stmt.Step(); err != nil {
		t.Fatal(err)
	} else if hasRow {
		count := int(stmt.ColumnInt64(0))
		if want := poolSize * 10 * insertCount; count != want {
			t.Errorf("SELECT COUNT(*) = %d, want %d", count, want)
		}
	} else {
		t.Errorf("SELECT COUNT(*) reports not having a row")
	}
	stmt.Reset()
}

const insertCount = 120

func testInsert(t *testing.T, id string, dbpool *sqlitex.Pool) {
	c := dbpool.Get(nil)
	defer dbpool.Put(c)

	begin := c.Prep("BEGIN;")
	commit := c.Prep("COMMIT;")
	stmt := c.Prep("INSERT INTO footable (col1) VALUES (?);")

	if _, err := begin.Step(); err != nil {
		t.Errorf("id=%s: BEGIN step: %v", id, err)
	}
	for i := int64(0); i < insertCount; i++ {
		if err := stmt.Reset(); err != nil {
			t.Errorf("id=%s: reset: %v", id, err)
		}
		stmt.BindInt64(1, i)
		if _, err := stmt.Step(); err != nil {
			t.Errorf("id=%s: step: %v", id, err)
		}
	}
	if _, err := commit.Step(); err != nil {
		t.Errorf("id=%s: COMMIT step: %v", id, err)
	}
}

func TestPoolAfterClose(t *testing.T) {
	// verify that Get after close never try to initialize a Conn and segfault
	dbpool := newMemPool(t)

	err := dbpool.Close()
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 10*poolSize; i++ {
		conn := dbpool.Get(nil)
		if conn != nil {
			t.Fatal("dbpool: Get after Close -> !nil conn")
		}
	}
}

func TestSharedCacheLock(t *testing.T) {
	c0, ctx, _, done, result := startSharedCacheInsert(t)
	if err := sqlitex.Execute(c0, "COMMIT;", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
		if err := <-result; err != nil {
			t.Fatalf("insert after releasing read transaction: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("insert did not finish after releasing read transaction:", ctx.Err())
	}
	if err := sqlitex.Execute(c0, "SELECT c FROM t ORDER BY c;", &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			if got := stmt.ColumnInt(0); got != 1 {
				return fmt.Errorf("inserted c = %d, want 1", got)
			}
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	count, err := sqlitex.ResultInt(c0.Prep("SELECT count(*) FROM t;"))
	if err != nil || count != 1 {
		t.Fatalf("inserted row count = %d, %v; want 1, nil", count, err)
	}
}

func TestSharedCacheLockCancel(t *testing.T) {
	c0, ctx, cancel, done, result := startSharedCacheInsert(t)
	cancel()
	if c0.AutocommitEnabled() {
		t.Fatal("read transaction was not held when canceling the insert")
	}
	select {
	case <-done:
		if err := <-result; sqlite.ErrCode(err) != sqlite.ResultInterrupt {
			t.Fatalf("insert canceled while read transaction held: %v; want SQLITE_INTERRUPT", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled insert did not join while read transaction held:", ctx.Err())
	}
	if err := sqlitex.Execute(c0, "ROLLBACK;", nil); err != nil {
		t.Fatal(err)
	}
	count, err := sqlitex.ResultInt(c0.Prep("SELECT count(*) FROM t;"))
	if err != nil || count != 0 {
		t.Fatalf("rows after canceled insert = %d, %v; want 0, nil", count, err)
	}
}

// startSharedCacheInsert returns only after Step is waiting for c0's actual
// shared-cache table lock. SQLite exposes no callback for unlock-notify wait
// registration. Observe the worker's parked stack rather than guessing a delay
// or adding a production hook. This also avoids canceling before registration.
func startSharedCacheInsert(t *testing.T) (*sqlite.Conn, context.Context, context.CancelFunc, <-chan struct{}, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	dbFile := filepath.Join(t.TempDir(), "shared.db")
	flags := sqlite.OpenReadWrite | sqlite.OpenCreate | sqlite.OpenURI | sqlite.OpenSharedCache
	c0, err := sqlite.OpenConn(dbFile, flags)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c0.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := sqlitex.Execute(c0, "CREATE TABLE t (c);", nil); err != nil {
		t.Fatal(err)
	}
	c1, err := sqlite.OpenConn(dbFile, flags)
	if err != nil {
		t.Fatal(err)
	}
	var done chan struct{}
	t.Cleanup(func() {
		cancel()
		// Always release the dependency before joining, including Fatal/panic
		// paths. Close only after Step and its statement finalization return.
		if !c0.AutocommitEnabled() {
			if err := sqlitex.Execute(c0, "ROLLBACK;", nil); err != nil {
				t.Error("release read transaction during cleanup:", err)
			}
		}
		if done != nil {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				// Never free a live connection, even on a broken-driver path.
				t.Error("insert worker did not join after cancel and rollback; leaving its connection open")
				return
			}
		}
		if err := c1.Close(); err != nil {
			t.Error(err)
		}
	})
	c1.SetInterrupt(ctx.Done())
	if err := sqlitex.Execute(c0, "BEGIN;", nil); err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.Execute(c0, "SELECT c FROM t;", nil); err != nil {
		t.Fatal(err)
	}
	stmt, _, err := c1.PrepareTransient("INSERT INTO t VALUES (1);")
	if err != nil {
		t.Fatal(err)
	}
	done = make(chan struct{})
	result := make(chan error, 1)
	go func() {
		defer close(done)
		_, err := stmt.Step()
		result <- errors.Join(err, stmt.Finalize())
	}()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	stack := make([]byte, 1<<20)
	for {
		n := runtime.Stack(stack, true)
		for _, goroutine := range strings.Split(string(stack[:n]), "\n\n") {
			if strings.Contains(goroutine, "sqlitex_test.startSharedCacheInsert.func") &&
				strings.Contains(goroutine, "sqlite.waitForUnlockNotify(") &&
				strings.Contains(goroutine, "[sync.Mutex.Lock]") {
				return c0, ctx, cancel, done, result
			}
		}
		select {
		case <-done:
			t.Fatal("insert finished while read transaction held:", <-result)
		case <-ctx.Done():
			t.Fatal("insert did not park waiting for shared-cache unlock:", ctx.Err())
		case <-ticker.C:
		}
	}
}

func TestPoolPutMatch(t *testing.T) {
	dbpool0 := newMemPool(t)
	dbpool1 := newMemPool(t)
	defer func() {
		if err := dbpool0.Close(); err != nil {
			t.Error(err)
		}
		if err := dbpool1.Close(); err != nil {
			t.Error(err)
		}
	}()

	func() {
		c := dbpool0.Get(nil)
		defer func() {
			if r := recover(); r == nil {
				t.Error("expect put mismatch panic, got none")
			}
			dbpool0.Put(c)
		}()

		dbpool1.Put(c)
	}()
}

// See https://github.com/crawshaw/sqlite/issues/119 and
// https://github.com/zombiezen/go-sqlite/issues/14
func TestPoolWALClose(t *testing.T) {
	dbName := filepath.Join(t.TempDir(), "wal-close.db")
	pool, err := sqlitex.NewPool(dbName, sqlitex.PoolOptions{
		Flags:    sqlite.OpenReadWrite | sqlite.OpenCreate | sqlite.OpenWAL,
		PoolSize: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	conn := pool.Get(context.Background())
	if _, err := os.Stat(dbName + "-wal"); err != nil {
		t.Error(err)
	}
	err = sqlitex.ExecTransient(conn, `CREATE TABLE foo (id integer primary key);`, nil)
	if err != nil {
		t.Error(err)
	}
	pool.Put(conn)
	if err := pool.Close(); err != nil {
		t.Error(err)
	}
	if _, err := os.Stat(dbName + "-wal"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("After close: %v; want %v", err, os.ErrNotExist)
	}
}

func TestPoolPrepareConn(t *testing.T) {
	dbName := filepath.Join(t.TempDir(), "foo.db")
	pool, err := sqlitex.NewPool(dbName, sqlitex.PoolOptions{
		PrepareConn: func(conn *sqlite.Conn) error {
			err := sqlitex.ExecuteTransient(conn, `PRAGMA foreign_keys = on;`, nil)
			if err != nil {
				t.Error("Prepare internal error:", err)
			}
			return err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := pool.Close(); err != nil {
			t.Error("Close:", err)
		}
	}()

	conn := pool.Get(context.Background())
	if conn == nil {
		t.Fatal("conn == nil")
	}
	defer pool.Put(conn)

	fkEnabled := false
	err = sqlitex.ExecuteTransient(conn, `PRAGMA foreign_keys;`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			fkEnabled = stmt.ColumnBool(0)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !fkEnabled {
		t.Error("foreign_keys not enabled")
	}
}
