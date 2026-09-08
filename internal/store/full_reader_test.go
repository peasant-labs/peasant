package store_test

import (
	"context"
	_ "embed"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/full_read_budget_policy.yaml
var fullReadBudgetYAML []byte

//go:embed testdata/full_read_consistency.yaml
var fullReadConsistencyYAML []byte

type fullReadFixture struct {
	RequiredNames []string `yaml:"requiredNames"`
	Cases         []struct {
		Name, Action   string
		Entries, Bytes int
		Budget         int64
		Pages          []int
	}
}

func loadFullReadFixtures(t *testing.T, data []byte) fullReadFixture {
	t.Helper()
	var f fullReadFixture
	if err := yaml.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, c := range f.Cases {
		if c.Name == "" || names[c.Name] {
			t.Fatal("empty or duplicate fixture name")
		}
		names[c.Name] = true
	}
	for _, name := range f.RequiredNames {
		if !names[name] {
			t.Fatalf("missing fixture %s", name)
		}
	}
	return f
}

func seedFullRead(t *testing.T, n, bytes int) (*store.Store, ingest.SessionID, []schema.SessionEntry, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "snapshot.db")
	s, err := store.Open(path, store.WithPoolSize(1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	id := ingest.SessionID("aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa")
	seedSession(t, s, string(id))
	text := strings.Repeat("x", bytes)
	entries := make([]schema.SessionEntry, n)
	for i := range entries {
		entries[i] = schema.SessionEntry{SessionID: id, EntryIndex: i, Role: schema.RoleAssistant, Harness: schema.HarnessClaudeCode, EntryType: schema.EntryTypeText, ContentPreview: &text}
	}
	writeFull(t, s, id, entries, ingest.SessionEntryWriteReplaceAll)
	return s, id, entries, path
}

// The SQLite authorizer observes actual production SELECT compilation, without
// replacing the reader. entry_type identifies projection/page preparations
// (including SQLite range-query reauthorization); chunk data identifies each
// hydrated entry query.
func observeFullReads(t *testing.T, s *store.Store, observe func(sqlite.Action)) {
	t.Helper()
	conn := takeConn(t, s.PoolForTest())
	if err := conn.SetAuthorizer(sqlite.AuthorizeFunc(func(a sqlite.Action) sqlite.AuthResult {
		observe(a)
		return sqlite.AuthResultOK
	})); err != nil {
		t.Fatal(err)
	}
	s.PoolForTest().Put(conn)
	t.Cleanup(func() {
		conn := takeConn(t, s.PoolForTest())
		_ = conn.SetAuthorizer(nil)
		s.PoolForTest().Put(conn)
	})
}

func TestFullReaderBudgetAndLinearVerification(t *testing.T) {
	for _, f := range loadFullReadFixtures(t, fullReadBudgetYAML).Cases {
		t.Run(f.Name, func(t *testing.T) {
			s, id, want, _ := seedFullRead(t, f.Entries, f.Bytes)
			projections, chunks := 0, 0
			observeFullReads(t, s, func(a sqlite.Action) {
				if a.Type() == sqlite.OpRead && a.Table() == "session_entries" && a.Column() == "entry_type" {
					projections++
				}
				if a.Type() == sqlite.OpRead && a.Table() == "session_entry_full_content_chunks" && a.Column() == "data" {
					chunks++
				}
			})
			got, capture, err := s.LoadFullSessionEntries(t.Context(), id, f.Budget)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) || capture.EntryCount != len(want) {
				t.Fatal("full entries changed")
			}
			// SQLite reauthorizes parameterized range queries when stepping after
			// binding (STAT4 planning). The unbounded verification compiles once.
			if projections != 2*len(f.Pages)+1 {
				t.Fatalf("projection/page authorizations = %d, want %d (one verification plus range preparations)", projections, 2*len(f.Pages)+1)
			}
			if chunks != len(want) {
				t.Fatalf("chunk hydrations = %d, want %d", chunks, len(want))
			}
			projections, chunks = 0, 0
			from := 0
			var sizes []int
			for {
				page, err := s.ReadSessionEntries(t.Context(), id, ingest.SessionEntryReadOptions{Mode: ingest.SessionEntryReadFullContent, FromIndex: from, SoftMaxBytes: f.Budget})
				if err != nil {
					t.Fatal(err)
				}
				sizes = append(sizes, len(page.Entries))
				if page.NextIndex == nil {
					break
				}
				if *page.NextIndex <= from {
					t.Fatal("cursor failed to advance")
				}
				from = *page.NextIndex
			}
			if !reflect.DeepEqual(sizes, f.Pages) {
				t.Fatalf("page sizes %v, want %v", sizes, f.Pages)
			}
			if projections != 3*len(f.Pages) {
				t.Fatalf("standalone pages must each independently verify: SELECTs %d", projections)
			}
		})
	}
}

func TestFullReaderSnapshotCancellationAndRelease(t *testing.T) {
	for _, f := range loadFullReadFixtures(t, fullReadConsistencyYAML).Cases {
		t.Run(f.Name, func(t *testing.T) {
			s, id, want, path := seedFullRead(t, 3, 270000)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var writer *sqlite.Conn
			if f.Action == "snapshot" {
				var err error
				writer, err = sqlite.OpenConn(path, sqlite.OpenReadWrite)
				if err != nil {
					t.Fatal(err)
				}
				defer writer.Close()
			}
			if f.Action == "corrupt_chunk" {
				execContentSQL(t, s, `UPDATE session_entry_full_content_chunks SET chunk_sha256='0000000000000000000000000000000000000000000000000000000000000000' WHERE entry_index=2 AND chunk_index=1`)
			}
			triggered := false
			chunkQueries := 0
			observeFullReads(t, s, func(a sqlite.Action) {
				if a.Type() != sqlite.OpRead {
					return
				}
				if a.Table() == "session_entries" && a.Column() == "entry_type" && !triggered {
					// Capture SELECT has already executed, establishing the snapshot.
					if f.Action == "snapshot" {
						triggered = true
						if err := sqlitex.ExecuteTransient(writer, `UPDATE session_entries SET tool_output='changed' WHERE session_id=?`, &sqlitex.ExecOptions{Args: []any{string(id)}}); err != nil {
							t.Error(err)
						}
					}
				}
				// Manifest preparation occurs inside the projection row callback,
				// after the semantic SELECT has begun stepping.
				if a.Table() == "session_entry_full_content" && f.Action == "cancel_projection" && !triggered {
					triggered = true
					cancel()
				}
				if a.Table() == "session_entry_full_content_chunks" && a.Column() == "data" {
					chunkQueries++
					if f.Action == "cancel_chunk" && chunkQueries == 2 {
						triggered = true
						cancel()
					}
				}
			})
			budget := int64(1)
			if f.Action == "negative" {
				budget = -1
			}
			got, _, err := s.LoadFullSessionEntries(ctx, id, budget)
			if f.Action == "snapshot" {
				if !triggered || err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("snapshot mixed concurrent state: triggered=%v err=%v", triggered, err)
				}
				if _, _, err := s.LoadFullSessionEntries(t.Context(), id, 0); err == nil {
					t.Fatal("later read missed changed semantic projection")
				}
			} else {
				if err == nil || got != nil {
					t.Fatal("failure returned usable partial entries")
				}
				if strings.HasPrefix(f.Action, "cancel_") && (!triggered || !errors.Is(err, context.Canceled)) {
					t.Fatalf("expected cancellation: %v", err)
				}
			}
			// A bounded follow-up on a one-connection pool proves transaction cleanup.
			followup, stop := context.WithTimeout(t.Context(), time.Second)
			defer stop()
			if _, _, err := s.GetSessionContentCapture(followup, id); err != nil {
				t.Fatalf("connection was not released: %v", err)
			}
		})
	}
}

func TestFullReaderRejectsCorruptionAndReleasesPool(t *testing.T) {
	for _, f := range loadContentFixtures(t).Corruptions {
		t.Run(f.Name, func(t *testing.T) {
			s, id, _, _ := seedFullRead(t, 3, 270000)
			execContentSQL(t, s, f.SQL)
			entries, _, err := s.LoadFullSessionEntries(t.Context(), id, 1)
			if err == nil || entries != nil {
				t.Fatal("corrupt capture returned usable full entries")
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if _, _, err := s.GetSessionContentCapture(ctx, id); err != nil {
				t.Fatalf("connection not released after corruption: %v", err)
			}
		})
	}
}
