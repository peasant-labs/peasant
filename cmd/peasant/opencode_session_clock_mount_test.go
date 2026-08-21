package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// TestOpenCodeSessionClockFixturesMountedHarvest proves both clock fixtures flow
// through the mounted harvest command and re-ingest when their freshness moves.
// The absent-clock database re-ingests when its content mtime floor moves,
// because it has no session clock. The lagging-clock database re-ingests when a
// row time passes the recorded ingest time, because the changed time tracks the
// newest row time rather than the lagging clock.
func TestOpenCodeSessionClockFixturesMountedHarvest(t *testing.T) {
	oldModTime := time.Unix(1_700_001_000, 0)
	newerModTime := time.Unix(1_700_002_000, 0)
	ingestedBetweenMS := int64(1_700_001_500_000)
	newRowMS := int64(1_700_002_000_000)

	cases := []struct {
		name    string
		fixture string
		// mutate moves the fixture's freshness forward before the second harvest.
		mutate func(t *testing.T, databasePath string)
	}{
		{
			name:    "absent-clock-re-ingests-when-the-mtime-floor-moves",
			fixture: "session-clock-absent-floor",
			mutate: func(t *testing.T, databasePath string) {
				setSyntheticSQLiteContentModTime(t, databasePath, newerModTime)
			},
		},
		{
			name:    "lagging-clock-re-ingests-when-a-row-time-moves",
			fixture: "session-clock-lagging",
			mutate: func(t *testing.T, databasePath string) {
				updateSourceRowTime(t, databasePath, "message", "msg_lag_b", newRowMS)
				// Keep the floor old so only the newest row time drives the change.
				setSyntheticSQLiteContentModTime(t, databasePath, oldModTime)
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			materialized := testfixture.MaterializeByName(t, testCase.fixture)
			setSyntheticSQLiteContentModTime(t, materialized.Path, oldModTime)
			commandRoot := t.TempDir()
			outputRoot := filepath.Join(commandRoot, "managed")
			args := []string{"--source-provider=" + defaults.HarnessOpenCode.String(), "--source-path=" + filepath.Dir(materialized.Path), "--output=" + outputRoot}

			output, err := executeHarvestCmd(t, commandRoot, args)
			if err != nil {
				t.Fatalf("first mounted harvest: %v\n%s", err, output)
			}
			if !strings.Contains(output, "1 new") {
				t.Fatalf("first harvest did not ingest the session:\n%s", output)
			}

			storePath := defaults.ResolveDBFilePathWith(commandRoot).String()
			testCase.mutate(t, materialized.Path)
			setLocalIngestedTimestamp(t, storePath, ingestedBetweenMS)

			output, err = executeHarvestCmd(t, commandRoot, args)
			if err != nil {
				t.Fatalf("second mounted harvest: %v\n%s", err, output)
			}
			if !strings.Contains(output, "1 updated") {
				t.Fatalf("moved freshness did not re-ingest the session:\n%s", output)
			}
		})
	}
}

func updateSourceRowTime(t testing.TB, databasePath, table, id string, updated int64) {
	t.Helper()
	connection, err := sqlite.OpenConn(databasePath, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatalf("open synthetic source for row time update: %v", err)
	}
	updateErr := sqlitex.ExecuteTransient(connection, "UPDATE "+table+" SET time_updated = ?1 WHERE id = ?2", &sqlitex.ExecOptions{Args: []any{updated, id}})
	closeErr := connection.Close()
	if updateErr != nil || closeErr != nil {
		t.Fatalf("update synthetic source row time: %v", errors.Join(updateErr, closeErr))
	}
}
