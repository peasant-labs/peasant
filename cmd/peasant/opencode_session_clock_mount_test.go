package main

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// openCodeSessionClockMutation names the freshness change a case applies before
// the second harvest.
type openCodeSessionClockMutation string

const (
	openCodeSessionClockMutationMTimeFloor   openCodeSessionClockMutation = "mtime-floor"
	openCodeSessionClockMutationSessionClock openCodeSessionClockMutation = "session-clock"
)

type openCodeSessionClockMountDocument struct {
	RequiredCases []string                   `yaml:"required_cases"`
	Cases         []openCodeSessionClockCase `yaml:"cases"`
}

type openCodeSessionClockCase struct {
	Name      string                       `yaml:"name"`
	Fixture   string                       `yaml:"fixture"`
	Mutation  openCodeSessionClockMutation `yaml:"mutation"`
	SessionID string                       `yaml:"sessionID"`
}

//go:embed testdata/opencode_session_clock_mount.yaml
var openCodeSessionClockMountData []byte

func loadOpenCodeSessionClockMountCases(data []byte) ([]openCodeSessionClockCase, error) {
	const fixturePath = "cmd/peasant/testdata/opencode_session_clock_mount.yaml"
	var document openCodeSessionClockMountDocument
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode %s: %w", fixturePath, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s must contain exactly one YAML document", fixturePath)
	}
	if len(document.RequiredCases) == 0 {
		return nil, fmt.Errorf("%s declares no required cases", fixturePath)
	}
	seen := make(map[string]struct{}, len(document.Cases))
	for _, testCase := range document.Cases {
		if testCase.Name == "" || testCase.Fixture == "" {
			return nil, fmt.Errorf("%s has a case missing its name or fixture: %+v", fixturePath, testCase)
		}
		if _, duplicate := seen[testCase.Name]; duplicate {
			return nil, fmt.Errorf("%s has a duplicate case name %q", fixturePath, testCase.Name)
		}
		seen[testCase.Name] = struct{}{}
		switch testCase.Mutation {
		case openCodeSessionClockMutationMTimeFloor:
		case openCodeSessionClockMutationSessionClock:
			if testCase.SessionID == "" {
				return nil, fmt.Errorf("%s session-clock case %q must name sessionID", fixturePath, testCase.Name)
			}
		default:
			return nil, fmt.Errorf("%s case %q has unknown mutation %q", fixturePath, testCase.Name, testCase.Mutation)
		}
	}
	for _, name := range document.RequiredCases {
		if _, ok := seen[name]; !ok {
			return nil, fmt.Errorf("%s is missing required case %q", fixturePath, name)
		}
	}
	return document.Cases, nil
}

// TestOpenCodeSessionClockFixturesMountedHarvest proves both clock fixtures flow
// through the mounted harvest command and re-ingest when their freshness moves.
// The absent-clock database re-ingests when its content mtime floor moves,
// because it has no session clock. The clock-bearing database re-ingests when
// its session clock moves past the recorded ingest time, because freshness is
// clock-first for a session that has a clock and reads no row aggregate.
func TestOpenCodeSessionClockFixturesMountedHarvest(t *testing.T) {
	oldModTime := time.Unix(1_700_001_000, 0)
	newerModTime := time.Unix(1_700_002_000, 0)
	ingestedBetweenMS := int64(1_700_001_500_000)
	newRowMS := int64(1_700_002_000_000)

	cases, err := loadOpenCodeSessionClockMountCases(openCodeSessionClockMountData)
	if err != nil {
		t.Fatalf("load OpenCode session-clock mount cases: %v", err)
	}

	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.Name, func(t *testing.T) {
			materialized := testfixture.MaterializeByName(t, testCase.Fixture)
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
			switch testCase.Mutation {
			case openCodeSessionClockMutationMTimeFloor:
				setSyntheticSQLiteContentModTime(t, materialized.Path, newerModTime)
			case openCodeSessionClockMutationSessionClock:
				updateSourceRowTime(t, materialized.Path, "session", testCase.SessionID, newRowMS)
				// Keep the floor old so only the moved session clock drives the
				// change, proving freshness is clock-first for a clock-bearing
				// session.
				setSyntheticSQLiteContentModTime(t, materialized.Path, oldModTime)
			}
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
