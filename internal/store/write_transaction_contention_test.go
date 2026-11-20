package store

import (
	"context"
	_ "embed"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testkit/testwait"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"gopkg.in/yaml.v3"
)

// Store writes that read before they write, beside a writer on another
// SQLite connection. A DEFERRED transaction holds only a read snapshot until
// its first write, so a commit on another connection between the read and
// the write makes that write fail with "database is locked" at once, without
// waiting on busy_timeout. These cases drive the production entry points
// with a competing connection at a deterministic point (the SQLite authorizer
// sees the path prepare its first write) and prove each path took the writer
// before it read. Nothing here runs parallel: each case owns its store.

//go:embed testdata/write_transaction_contention.yaml
var writeTransactionContentionYAML []byte

//go:embed testdata/write_transaction_contention.manifest.yaml
var writeTransactionContentionManifestYAML []byte

type writeTransactionContentionCase struct {
	Name   string `yaml:"name"`
	Path   string `yaml:"path"`
	Seam   string `yaml:"seam"`
	Expect string `yaml:"expect"`
}

const (
	writeContentionSeamFirstWrite  = "first-write"
	writeContentionSeamHeldWriter  = "held-writer"
	writeContentionExpectBothWrote = "competitor-waits-then-both-written"
	writeContentionExpectRefused   = "refused-with-lock-advice"
)

// writeContentionExpectations pairs each seam with the one outcome it proves.
var writeContentionExpectations = map[string]string{
	writeContentionSeamFirstWrite: writeContentionExpectBothWrote,
	writeContentionSeamHeldWriter: writeContentionExpectRefused,
}

// writeContentionPath is one production store entry point that reads before
// it writes: seed prepares its rows, run calls the entry point, and written
// reports whether the path's own write is durable.
type writeContentionPath struct {
	seed    func(t *testing.T, s *Store)
	run     func(ctx context.Context, s *Store) error
	written func(t *testing.T, s *Store) bool
}

const (
	writeContentionSession = "f1f1f1f1-f1f1-41f1-81f1-f1f1f1f1f1f1"
	writeContentionParent  = "f2f2f2f2-f2f2-42f2-82f2-f2f2f2f2f2f2"
)

func writeContentionCount(t *testing.T, s *Store, query string) bool {
	t.Helper()
	return queryMigrateInt(t, s, query) == 1
}

// writeContentionPaths is the closed set of entry points the fixture names.
var writeContentionPaths = map[string]writeContentionPath{
	"update-origin-state": {
		seed: func(t *testing.T, s *Store) { seedGenerationSession(t, s, writeContentionSession) },
		run: func(ctx context.Context, s *Store) error {
			return s.UpdateOriginState(ctx, writeContentionSession, "agent", 7)
		},
		written: func(t *testing.T, s *Store) bool {
			return writeContentionCount(t, s, "SELECT COUNT(*) FROM sessions WHERE session_id='"+writeContentionSession+"' AND session_origin='agent' AND origin_version=7")
		},
	},
	"update-index-state": {
		seed: func(t *testing.T, s *Store) { seedGenerationSession(t, s, writeContentionSession) },
		run: func(ctx context.Context, s *Store) error {
			return s.UpdateIndexState(ctx, writeContentionSession, 3, 1700000000000)
		},
		written: func(t *testing.T, s *Store) bool {
			return writeContentionCount(t, s, "SELECT COUNT(*) FROM sessions WHERE session_id='"+writeContentionSession+"' AND index_version=3 AND indexed_at=1700000000000")
		},
	},
	"reconcile-parent-cache": {
		seed: func(t *testing.T, s *Store) {
			seedGenerationSession(t, s, writeContentionParent)
			seedGenerationSession(t, s, writeContentionSession)
		},
		run: func(ctx context.Context, s *Store) error {
			return s.ReconcileParentCache(ctx, []ingest.ParentCacheReconcile{{Child: writeContentionSession, Parent: writeContentionParent}})
		},
		written: func(t *testing.T, s *Store) bool {
			return writeContentionCount(t, s, "SELECT COUNT(*) FROM sessions WHERE session_id='"+writeContentionSession+"' AND parent_id='"+writeContentionParent+"'")
		},
	},
	"prune-sessions": {
		seed: func(t *testing.T, s *Store) { seedGenerationSession(t, s, writeContentionSession) },
		run: func(ctx context.Context, s *Store) error {
			_, err := s.PruneSessions(ctx, []ingest.SessionID{writeContentionSession})
			return err
		},
		written: func(t *testing.T, s *Store) bool {
			return writeContentionCount(t, s, "SELECT COUNT(*) = 0 FROM sessions WHERE session_id='"+writeContentionSession+"'")
		},
	},
	"insert-sessions": {
		seed: func(t *testing.T, s *Store) {},
		run: func(ctx context.Context, s *Store) error {
			meta := planLibraryMeta(writeContentionSession, defaults.HarnessClaudeCode, nil)
			return s.InsertSessions(ctx, []ingest.StoreEntry{{Metadata: &meta, PublicationCapture: true, CWDProvenance: ingest.CWDSourceAbsent}})
		},
		written: func(t *testing.T, s *Store) bool {
			return writeContentionCount(t, s, "SELECT COUNT(*) FROM sessions WHERE session_id='"+writeContentionSession+"'")
		},
	},
}

func loadWriteTransactionContentionCases(t *testing.T) []writeTransactionContentionCase {
	t.Helper()
	var fixture struct {
		Cases []writeTransactionContentionCase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(writeTransactionContentionYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode write_transaction_contention.yaml: %v", err)
	}
	manifest, err := decodeRecoveryRequiredNames(writeTransactionContentionManifestYAML)
	if err != nil {
		t.Fatalf("decode write_transaction_contention manifest: %v", err)
	}
	names := make([]string, 0, len(fixture.Cases))
	for _, c := range fixture.Cases {
		if _, ok := writeContentionPaths[c.Path]; !ok {
			t.Fatalf("write_transaction_contention.yaml case %q names unknown path %q", c.Name, c.Path)
		}
		if want, ok := writeContentionExpectations[c.Seam]; !ok || c.Expect != want {
			t.Fatalf("write_transaction_contention.yaml case %q pairs seam %q with expectation %q, want %q", c.Name, c.Seam, c.Expect, want)
		}
		names = append(names, c.Name)
	}
	if err := validateRecoveryRequiredNames(manifest, names, "write transaction contention"); err != nil {
		t.Fatal(err)
	}
	return fixture.Cases
}

func TestStoreWritesTakeTheWriterBeforeTheyRead(t *testing.T) {
	for _, c := range loadWriteTransactionContentionCases(t) {
		t.Run(c.Name, func(t *testing.T) {
			path := writeContentionPaths[c.Path]
			s, root := openGenerationStoreWith(t, WithPoolSize(1))
			path.seed(t, s)
			competitor := openCompetingWriter(t, root)
			ctx := testwait.Context(t)
			switch c.Seam {
			case writeContentionSeamFirstWrite:
				runWriteContentionAtFirstWrite(t, ctx, s, competitor, path)
			case writeContentionSeamHeldWriter:
				runWriteContentionHeldWriter(t, ctx, s, competitor, path)
			}
		})
	}
}

// armFirstWriteSeam installs an authorizer on the store's one pooled
// connection that calls onFirstWrite once, when the connection prepares its
// first INSERT, UPDATE or DELETE inside a transaction: after the path's
// reads, before its first write runs. The authorizer changes neither SQL nor
// results. Setting it invalidates the connection's cached statements, so the
// path's statements are prepared, and seen, afresh.
func armFirstWriteSeam(t *testing.T, s *Store, onFirstWrite func()) func() int {
	t.Helper()
	conn := searchTakeConn(t, s)
	defer s.pool.Put(conn)
	var mu sync.Mutex
	fired := 0
	err := conn.SetAuthorizer(sqlite.AuthorizeFunc(func(action sqlite.Action) sqlite.AuthResult {
		switch action.Type() {
		case sqlite.OpInsert, sqlite.OpUpdate, sqlite.OpDelete:
		default:
			return sqlite.AuthResultOK
		}
		if conn.AutocommitEnabled() {
			return sqlite.AuthResultOK
		}
		mu.Lock()
		defer mu.Unlock()
		fired++
		if fired == 1 {
			onFirstWrite()
		}
		return sqlite.AuthResultOK
	}))
	if err != nil {
		t.Fatalf("install the first-write seam: %v", err)
	}
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return fired
	}
}

// runWriteContentionAtFirstWrite has the competitor try to write and commit
// between the path's reads and its first write. The path must already hold
// the writer, so the competitor is refused with BUSY, the path commits, and
// the competitor's write then lands beside it.
func runWriteContentionAtFirstWrite(t *testing.T, ctx context.Context, s *Store, competitor *sqlite.Conn, path writeContentionPath) {
	var probeErr error
	fired := armFirstWriteSeam(t, s, func() {
		probeErr = competingWriteBegin(competitor, 1)
		if probeErr == nil {
			probeErr = sqlitex.ExecuteTransient(competitor, "COMMIT", nil)
			if probeErr == nil {
				probeErr = fmt.Errorf("the competing writer committed between the path's read and its first write")
			}
		}
	})
	if err := path.run(ctx, s); err != nil {
		t.Fatalf("store write beside a competing writer that commits after the write's read: %v", err)
	}
	if fired() == 0 {
		t.Fatal("the path never prepared a write inside a transaction; the seam did not run")
	}
	if sqlite.ErrCode(probeErr).ToPrimary() != sqlite.ResultBusy {
		t.Fatalf("competing writer at the path's first write = %v, want BUSY: the path must hold the SQLite writer before it reads", probeErr)
	}
	if !path.written(t, s) {
		t.Fatal("the path reported success but its write is not durable")
	}
	if err := competingWriteBegin(competitor, 2); err != nil {
		t.Fatalf("competing writer after the path committed: %v", err)
	}
	if err := sqlitex.ExecuteTransient(competitor, "COMMIT", nil); err != nil {
		t.Fatalf("commit the competing write after the path: %v", err)
	}
	if rows := queryMigrateInt(t, s, "SELECT COUNT(*) FROM "+competingWriterProbeTable); rows != 1 {
		t.Fatalf("competing writer rows = %d, want its one write durable beside the path", rows)
	}
}

// runWriteContentionHeldWriter has the competitor hold the writer, and the
// path's connection wait on it for no time at all, standing in for a writer
// that holds longer than busy_timeout. The path must refuse, write nothing,
// and name lock contention, never disk space, as the cause.
func runWriteContentionHeldWriter(t *testing.T, ctx context.Context, s *Store, competitor *sqlite.Conn, path writeContentionPath) {
	conn := searchTakeConn(t, s)
	conn.SetBusyTimeout(0)
	s.pool.Put(conn)
	if err := competingWriteBegin(competitor, 1); err != nil {
		t.Fatalf("competing writer takes the writer: %v", err)
	}
	err := path.run(ctx, s)
	if rollbackErr := sqlitex.ExecuteTransient(competitor, "ROLLBACK", nil); rollbackErr != nil {
		t.Fatalf("release the held writer: %v", rollbackErr)
	}
	if err == nil {
		t.Fatal("store write beside a held writer succeeded; want a refusal")
	}
	if sqlite.ErrCode(err).ToPrimary() != sqlite.ResultBusy {
		t.Fatalf("store write beside a held writer = %v, want BUSY", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "held the store's SQLite writer for longer than the "+defaults.SQLiteBusyTimeout.String()+" busy timeout") {
		t.Fatalf("store write beside a held writer = %q, want it to name lock contention", msg)
	}
	if !strings.Contains(msg, "nothing was written") || strings.Contains(msg, "free disk space") {
		t.Fatalf("store write beside a held writer = %q, want it to say nothing was written and not to advise freeing disk space", msg)
	}
	if path.written(t, s) {
		t.Fatal("a refused write left its change behind")
	}
}
