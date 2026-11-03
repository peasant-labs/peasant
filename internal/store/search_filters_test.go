package store

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

func TestSearchFiltersBeforePagination(t *testing.T) {
	t.Parallel()
	family := loadSearchFilterFixtures(t)
	for _, c := range family.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			s, _ := openGenerationStore(t)
			ctx := context.Background()
			if any, err := s.HasAnySessions(ctx); err != nil || any {
				t.Fatalf("empty store has sessions = %v, err = %v", any, err)
			}
			for _, p := range family.Projects {
				if _, err := schema.NewProjectHash(p.Hash); err != nil {
					t.Fatal(err)
				}
				recallExec(t, s, `INSERT INTO projects(project_hash, canonical_cwd, canonical_remote) VALUES(?, ?, ?)`, p.Hash, p.Cwd, p.Remote)
			}
			recallExec(t, s, `INSERT INTO host_slugs(opaque_id, host_slug) VALUES('search-filter-host', 'search-filter-host')`)
			for _, session := range family.Sessions {
				sid, err := schema.NewSessionID(session.ID)
				if err != nil {
					t.Fatal(err)
				}
				recallExec(t, s, `INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format, schema_version) VALUES(?, ?, 'fixture-model', 'search-filter-host', ?, ?, 300, 400, '/fixture/source.jsonl', 'jsonl', 11)`, session.ID, session.Harness, session.Project, session.Start)
				entries := make([]schema.SessionEntry, 0, len(session.Entries))
				for index, text := range session.Entries {
					entries = append(entries, withPreview(recallEntry(sid, index, recallRef(session.ID, index)), text))
				}
				if session.Body {
					activateRecallEntries(t, s, sid, "search-filter-generation", entries, nil)
				} else {
					id, err := ingest.NewSessionID(session.ID)
					if err != nil {
						t.Fatal(err)
					}
					if err := s.IndexSessionEntries(ctx, id, entries); err != nil {
						t.Fatal(err)
					}
				}
				tags, err := json.Marshal(session.Tags)
				if err != nil {
					t.Fatal(err)
				}
				recallExec(t, s, `UPDATE sessions SET start_ms = ?, model_harness = ?, tags = ? WHERE session_id = ?`, session.Start, session.Harness, string(tags), session.ID)
			}
			if c.ClearUnified {
				// Model the transition before unified postings have been filled.
				// Both tables remain present, so only the legacy probe can find it.
				recallExec(t, s, `INSERT INTO session_search_fts(session_search_fts) VALUES('delete-all')`)
			}
			if any, err := s.HasAnySessions(ctx); err != nil || !any {
				t.Fatalf("seeded store has sessions = %v, err = %v", any, err)
			}
			conn, err := s.Pool().Take(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Pool().Put(conn)
			if !LegacySearchIndexExists(conn) || !UnifiedSearchIndexExists(conn) {
				t.Fatal("search filter fixtures require both transition indexes")
			}
			f := c.Filters.filter()
			hits, err := SearchMergedOnConn(conn, c.Query, c.Limit, c.Offset, f)
			if err != nil {
				t.Fatal(err)
			}
			got := make([]string, 0, len(hits))
			for _, hit := range hits {
				got = append(got, fmt.Sprintf("%s:%d", hit.SessionID, hit.EntryIndex))
			}
			if !reflect.DeepEqual(got, c.Want) {
				t.Fatalf("page = %v, want %v", got, c.Want)
			}
			any, err := SearchMatchesAnyOnConn(conn, c.Query, f)
			if err != nil {
				t.Fatal(err)
			}
			full, err := SearchMergedOnConn(conn, c.Query, 50, 0, f)
			if err != nil {
				t.Fatal(err)
			}
			if any != (len(full) > 0) {
				t.Fatalf("existence = %v, full filtered stream has %d hits", any, len(full))
			}
		})
	}
}
