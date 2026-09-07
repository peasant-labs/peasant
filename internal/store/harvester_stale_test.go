package store_test

import (
	"context"
	_ "embed"
	"slices"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/harvester_stale.yaml
var harvesterStaleYAML []byte

type harvesterStaleFixtures struct {
	Cases []struct {
		Name     string                 `yaml:"name"`
		Targets  map[ingest.Harness]int `yaml:"targets"`
		Expected []string               `yaml:"expected"`
	} `yaml:"cases"`
	Sessions []struct {
		Name    string           `yaml:"name"`
		ID      ingest.SessionID `yaml:"id"`
		Harness ingest.Harness   `yaml:"harness"`
		Version int              `yaml:"version"`
	} `yaml:"sessions"`
	Writes []struct {
		Name     string `yaml:"name"`
		Previous int    `yaml:"previous"`
		Next     int    `yaml:"next"`
		Error    bool   `yaml:"error"`
	} `yaml:"writes"`
}

func loadHarvesterStaleFixtures(t *testing.T) harvesterStaleFixtures {
	t.Helper()
	var fixtures harvesterStaleFixtures
	if err := yaml.Unmarshal(harvesterStaleYAML, &fixtures); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, fixture := range fixtures.Cases {
		names[fixture.Name] = true
	}
	for _, fixture := range fixtures.Writes {
		names[fixture.Name] = true
	}
	for _, name := range []string{"bump_only_opencode", "baseline_preserves_current", "explicit_claude_scope", "no_registered_indexers", "refuse_older_parser", "preserve_equal_parser", "advance_current_parser"} {
		if !names[name] {
			t.Fatalf("missing required harvester store fixture %q", name)
		}
	}
	return fixtures
}

func TestStore_HarvesterStaleSelection(t *testing.T) {
	t.Parallel()
	fixtures := loadHarvesterStaleFixtures(t)
	for _, fixture := range fixtures.Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			t.Parallel()
			s := openTestStore(t)
			ctx := context.Background()
			byID := make(map[ingest.SessionID]string)
			for _, session := range fixtures.Sessions {
				entry := makeStoreEntry(t, string(session.ID), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "fixture-project", session.Harness, 1700000000000, 0, 0)
				if err := s.InsertSessions(ctx, []ingest.StoreEntry{entry}); err != nil {
					t.Fatal(err)
				}
				if err := s.UpdateIndexState(ctx, session.ID, session.Version, 1700000001000); err != nil {
					t.Fatal(err)
				}
				byID[session.ID] = session.Name
			}
			targets := make(map[ingest.Harness]ingest.HarvesterVersions)
			for harness, version := range fixture.Targets {
				targets[harness] = ingest.HarvesterVersions{AdapterVersion: 1, IndexerVersion: version, IndexVersion: 1}
			}
			ids, err := s.ListStaleIndexSessions(ctx, targets)
			if err != nil {
				t.Fatal(err)
			}
			got := make([]string, 0, len(ids))
			for _, id := range ids {
				got = append(got, byID[id])
			}
			slices.Sort(got)
			if !slices.Equal(got, fixture.Expected) {
				t.Fatalf("stale sessions = %v, want %v", got, fixture.Expected)
			}
		})
	}
}

func TestStore_HarvesterWriterPreservesFutureParser(t *testing.T) {
	t.Parallel()
	for _, fixture := range loadHarvesterStaleFixtures(t).Writes {
		t.Run(fixture.Name, func(t *testing.T) {
			t.Parallel()
			s := openTestStore(t)
			ctx := context.Background()
			sid := ingest.SessionID("99999999-9999-4999-8999-999999999999")
			seedSession(t, s, string(sid))
			before := s.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{SessionID: sid, Result: indexformat.V1{Entries: batchTestEntries(sid, "last-good", 1)}, IndexVersion: 1, IndexerVersion: fixture.Previous, IndexedAtMs: 1700000001000}})
			assertBatchResult(t, before, 0, sid, true)
			hash := sessionEntriesHash(t, s, sid)
			after := s.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{SessionID: sid, Result: indexformat.V1{Entries: batchTestEntries(sid, "replacement", 1)}, IndexVersion: 1, IndexerVersion: fixture.Next, IndexedAtMs: 1700000002000}})
			assertBatchResult(t, after, 0, sid, !fixture.Error)
			if fixture.Error {
				assertIndexState(t, s, sid, fixture.Previous, 1700000001000)
				assertEntryContent(t, s, sid, "last-good-0")
				if sessionEntriesHash(t, s, sid) != hash {
					t.Fatal("refused downgrade changed entry hash")
				}
			} else {
				assertIndexState(t, s, sid, fixture.Next, 1700000002000)
				assertEntryContent(t, s, sid, "replacement-0")
			}
		})
	}
}
