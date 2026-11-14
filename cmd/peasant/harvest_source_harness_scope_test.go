package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
)

// TestHarvestSourceHarnessScopesStoredMaintenance pins that a native harvest
// scoped with --source-harness and --source-path maintains only the named
// harness's stored sessions. Every fixture harness carries a session at an older
// indexer revision, so an unscoped upkeep pass would re-index all of them; the
// scoped run must refresh the named harness's stale rows, report no session of
// another harness, and leave every other harness's stored rows unchanged.
func TestHarvestSourceHarnessScopesStoredMaintenance(t *testing.T) {
	t.Parallel()
	sessions, _ := LoadHarvestIndexSelectionFixtures(t)
	effectiveTargets := ingest.GenerationTargets(ingest.HarvesterVersionRegistry)
	var harnesses []ingest.Harness
	for _, session := range sessions {
		if !slices.Contains(harnesses, session.Harness) {
			harnesses = append(harnesses, session.Harness)
		}
	}
	if len(harnesses) < 2 {
		t.Fatalf("fixture sessions cover harnesses %v; need at least two for a cross-harness scope check", harnesses)
	}
	for _, named := range harnesses {
		t.Run(string(named), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			output := filepath.Join(dir, "managed")
			dbPath := defaults.ResolveDBFilePathWith(dir).String()
			if err := os.MkdirAll(filepath.Dir(dbPath), 0700); err != nil {
				t.Fatal(err)
			}
			db, err := openPreparedStore(t, dbPath, store.WithIndexFormats(store.V2IndexFormat()))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			before := make(map[ingest.SessionID]harvestIndexSnapshot)
			files := make(map[string][]byte)
			staleNamed := 0
			for _, session := range sessions {
				seedHarvestIndexSession(t, db, output, session, files)
				before[session.ID] = readHarvestIndexSnapshot(t, db, session.ID)
				if session.Harness == named && session.VersionDelta < 0 {
					staleNamed++
				}
			}
			if staleNamed == 0 {
				t.Fatalf("no stale %s fixture session; the scoped refresh would be vacuous", named)
			}
			configPath := writeTestConfigFile(t, dir)
			root := buildRootCommand()
			var buf, diagnostics bytes.Buffer
			root.SetOut(&buf)
			root.SetErr(&diagnostics)
			root.SetArgs([]string{
				"--config", configPath, "--data-dir", dir, "--config-dir", dir, "--state-dir", dir,
				"harvest", "--source-harness", string(named), "--source-path", t.TempDir(),
				"--output", output, "--json",
			})
			if err := root.Execute(); err != nil {
				t.Fatalf("harvest: %v; output: %s; stderr: %s", err, &buf, &diagnostics)
			}
			var result struct {
				Sessions []struct {
					SessionID ingest.SessionID `json:"sessionId"`
					Provider  defaults.Harness `json:"provider"`
				} `json:"sessions"`
			}
			if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
				t.Fatalf("decode result: %v; output: %s", err, &buf)
			}
			for _, reported := range result.Sessions {
				if reported.Provider != named {
					t.Errorf("--source-harness %s reported %s session %s; output: %s", named, reported.Provider, reported.SessionID, &buf)
				}
			}
			for _, session := range sessions {
				after := readHarvestIndexSnapshot(t, db, session.ID)
				switch {
				case session.Harness != named:
					if !reflect.DeepEqual(after, before[session.ID]) {
						t.Errorf("--source-harness %s changed %s session %s: before=%+v after=%+v", named, session.Harness, session.Name, before[session.ID], after)
					}
				case session.VersionDelta < 0:
					if after.IndexerVersion != effectiveTargets[session.Harness].IndexerVersion || after.IndexedAt == before[session.ID].IndexedAt {
						t.Errorf("--source-harness %s did not refresh its stale session %s: %+v", named, session.Name, after)
					}
				}
			}
		})
	}
}
