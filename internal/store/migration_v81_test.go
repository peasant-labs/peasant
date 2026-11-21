package store

import (
	"bytes"
	"context"
	_ "embed"
	regexp "github.com/wasilibs/go-re2"
	"maps"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/source_unavailability_lanes_migration.yaml
var sourceUnavailabilityLanesMigrationYAML []byte

type sourceLanesMigrationFixture struct {
	RequiredNames []string `yaml:"requiredNames"`
	Plans         []struct {
		Name     string `yaml:"name"`
		SeedRows bool   `yaml:"seedRows"`
		WantLong bool   `yaml:"wantLong"`
	} `yaml:"plans"`
	LegacyRows []struct {
		Name         string `yaml:"name"`
		SessionID    string `yaml:"sessionID"`
		Reason       string `yaml:"reason"`
		ExpectedLane string `yaml:"expectedLane"`
	} `yaml:"legacyRows"`
	Checks []struct {
		Name   string `yaml:"name"`
		Lane   string `yaml:"lane"`
		Reason string `yaml:"reason"`
		Valid  bool   `yaml:"valid"`
	} `yaml:"checks"`
	Concurrent []struct {
		Name       string   `yaml:"name"`
		SessionID  string   `yaml:"sessionID"`
		Lane       string   `yaml:"lane"`
		Reasons    []string `yaml:"reasons"`
		WantStored string   `yaml:"wantStored"`
	} `yaml:"concurrent"`
}

func loadSourceLanesMigrationFixture(t *testing.T) sourceLanesMigrationFixture {
	t.Helper()
	var fixture sourceLanesMigrationFixture
	decoder := yaml.NewDecoder(bytes.NewReader(sourceUnavailabilityLanesMigrationYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode source diagnostic lane migration fixture: %v", err)
	}
	names := make(map[string]bool)
	add := func(name string) {
		if name == "" || names[name] {
			t.Fatalf("source diagnostic lane fixture has an empty or duplicate name %q", name)
		}
		names[name] = true
	}
	for _, plan := range fixture.Plans {
		add(plan.Name)
	}
	for _, row := range fixture.LegacyRows {
		add(row.Name)
	}
	for _, check := range fixture.Checks {
		add(check.Name)
	}
	for _, concurrent := range fixture.Concurrent {
		add(concurrent.Name)
	}
	if len(fixture.RequiredNames) == 0 {
		t.Fatal("source diagnostic lane fixture needs a required-name manifest")
	}
	for _, name := range fixture.RequiredNames {
		if !names[name] {
			t.Fatalf("source diagnostic lane fixture is missing %q", name)
		}
		delete(names, name)
	}
	if len(names) != 0 {
		t.Fatalf("source diagnostic lane fixture has cases absent from its manifest: %v", names)
	}
	return fixture
}

func TestMigrationV81SeparatesSourceDiagnosticLanes(t *testing.T) {
	fixture := loadSourceLanesMigrationFixture(t)
	for _, planCase := range fixture.Plans {
		t.Run(planCase.Name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "pre-lane.db")
			buildBehindStore(t, path, 80, func(conn *sqlite.Conn) {
				if !planCase.SeedRows {
					return
				}
				if err := sqlitex.ExecuteScript(conn, `
INSERT INTO host_slugs(opaque_id, host_slug) VALUES('source-lanes-host','source-lanes-host');
INSERT INTO projects(project_hash) VALUES('source-lanes-project');`, nil); err != nil {
					t.Fatal(err)
				}
				for _, row := range fixture.LegacyRows {
					if err := sqlitex.ExecuteTransient(conn, `INSERT INTO sessions
(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format)
VALUES(?,'claude-code','fixture-model','source-lanes-host','source-lanes-project',1,2,3,'/synthetic/source.jsonl','jsonl')`,
						&sqlitex.ExecOptions{Args: []any{row.SessionID}}); err != nil {
						t.Fatalf("%s: seed session: %v", row.Name, err)
					}
					if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_source_unavailability(session_id, reason) VALUES(?,?)`,
						&sqlitex.ExecOptions{Args: []any{row.SessionID, row.Reason}}); err != nil {
						t.Fatalf("%s: seed legacy cause: %v", row.Name, err)
					}
				}
			})
			if opened, err := Open(path); err == nil {
				_ = opened.Close()
				t.Fatal("opening a schema-80 store silently ran its upgrade")
			} else if !IsSchemaBehind(err) {
				t.Fatalf("opening the predecessor returned %v, want a schema-behind refusal", err)
			}
			plan := mustPlan(t, path)
			stepsAfter80 := len(plan.Steps) == 2 && plan.Steps[0].Version == 81 && plan.Steps[1] == schemaStep(mappingSetSchemaVersion)
			if plan.From != 80 || plan.To != 82 || !stepsAfter80 {
				t.Fatalf("upgrade plan = %+v; want step 81 then the quick mapping-set step, from 80 to 82", plan)
			}
			if plan.HasLongStep() != planCase.WantLong {
				t.Fatalf("upgrade plan = %+v; want step 81 long=%t", plan, planCase.WantLong)
			}
			if _, err := Upgrade(t.Context(), path, UpgradeOptions{}); err != nil {
				t.Fatalf("upgrade source diagnostic lanes: %v", err)
			}
			db, err := Open(path, WithPoolSize(4))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if !planCase.SeedRows {
				return
			}
			for _, row := range fixture.LegacyRows {
				sid, err := ingest.NewSessionID(row.SessionID)
				if err != nil {
					t.Fatal(err)
				}
				lane, err := ingest.NewSourceUnavailabilityLane(row.ExpectedLane)
				if err != nil {
					t.Fatal(err)
				}
				stored, err := db.ReadSourceUnavailabilityForLane(t.Context(), sid, lane)
				if err != nil || stored == nil || string(*stored) != row.Reason {
					t.Fatalf("%s: migrated lane %s cause=%v err=%v, want %s", row.Name, lane, stored, err, row.Reason)
				}
				for _, other := range ingest.AllSourceUnavailabilityLanes() {
					if other == lane {
						continue
					}
					extra, err := db.ReadSourceUnavailabilityForLane(t.Context(), sid, other)
					if err != nil || extra != nil {
						t.Fatalf("%s: migration invented lane %s with cause %v, err=%v", row.Name, other, extra, err)
					}
				}
				detail, err := db.ReadSourceUnavailabilityDetail(t.Context(), sid)
				if err != nil || detail == nil {
					t.Fatalf("%s: aggregate=%+v err=%v", row.Name, detail, err)
				}
				if detail.Lane != lane || string(detail.Reason) != row.Reason {
					t.Fatalf("%s: aggregate=%+v err=%v", row.Name, detail, err)
				}
			}
			checkSourceLanesConstraints(t, db, fixture)
			for _, check := range fixture.Checks {
				if check.Valid {
					continue
				}
				sid := ingest.SessionID(fixture.LegacyRows[0].SessionID)
				lane := ingest.SourceUnavailabilityLane(check.Lane)
				reason := ingest.SourceUnavailabilityReason(check.Reason)
				if _, err := db.RecordSourceUnavailable(t.Context(), sid, lane, reason); err == nil {
					t.Fatalf("%s: record accepted invalid lane %q or cause %q", check.Name, lane, reason)
				}
				if _, laneErr := ingest.NewSourceUnavailabilityLane(check.Lane); laneErr != nil {
					if _, err := db.ReadSourceUnavailabilityForLane(t.Context(), sid, lane); err == nil {
						t.Fatalf("%s: lane reader accepted invalid lane %q", check.Name, lane)
					}
				}
			}
			for _, concurrent := range fixture.Concurrent {
				t.Run(concurrent.Name+"-race-check", func(t *testing.T) {
					sid, err := ingest.NewSessionID(concurrent.SessionID)
					if err != nil {
						t.Fatal(err)
					}
					lane, err := ingest.NewSourceUnavailabilityLane(concurrent.Lane)
					if err != nil {
						t.Fatal(err)
					}
					ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
					defer cancel()
					start := make(chan struct{})
					results := make(chan error, len(concurrent.Reasons))
					for _, raw := range concurrent.Reasons {
						reason, err := ingest.NewSourceUnavailabilityReason(raw)
						if err != nil {
							t.Fatal(err)
						}
						go func() {
							<-start
							_, err := db.RecordSourceUnavailable(ctx, sid, lane, reason)
							results <- err
						}()
					}
					// This exercises concurrent writes; the ordered
					// source_unavailability_upsert.yaml fixture proves a worse
					// reason cannot overwrite a better one.
					close(start)
					for range concurrent.Reasons {
						select {
						case err := <-results:
							if err != nil {
								t.Fatalf("concurrent source report: %v", err)
							}
						case <-ctx.Done():
							t.Fatalf("concurrent source reports did not complete: %v", ctx.Err())
						}
					}
					stored, err := db.ReadSourceUnavailabilityForLane(ctx, sid, lane)
					if err != nil || stored == nil || string(*stored) != concurrent.WantStored {
						t.Fatalf("concurrent stored cause=%v err=%v, want %s", stored, err, concurrent.WantStored)
					}
				})
			}
		})
	}
}

func checkSourceLanesConstraints(t *testing.T, db *Store, fixture sourceLanesMigrationFixture) {
	t.Helper()
	conn, err := db.pool.Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer db.pool.Put(conn)
	if err := sqlitex.ExecuteTransient(conn, `PRAGMA foreign_key_check`, &sqlitex.ExecOptions{
		ResultFunc: func(*sqlite.Stmt) error { t.Error("migration left a foreign-key violation"); return nil },
	}); err != nil {
		t.Fatal(err)
	}
	var createSQL string
	if err := sqlitex.ExecuteTransient(conn, `SELECT sql FROM sqlite_master WHERE type='table' AND name='session_source_unavailability'`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error { createSQL = stmt.ColumnText(0); return nil },
	}); err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`lane IN \(([^)]*)\)`).FindStringSubmatch(createSQL)
	if len(match) != 2 {
		t.Fatalf("source diagnostic table has no lane CHECK: %s", createSQL)
	}
	sqlLanes := make(map[string]bool)
	for _, value := range strings.Split(match[1], ",") {
		sqlLanes[strings.Trim(strings.TrimSpace(value), "'")] = true
	}
	goLanes := make(map[string]bool)
	for _, lane := range ingest.AllSourceUnavailabilityLanes() {
		goLanes[string(lane)] = true
	}
	if !maps.Equal(sqlLanes, goLanes) {
		t.Fatalf("database lane set %v differs from Go lane set %v", sqlLanes, goLanes)
	}
	reasonMatch := regexp.MustCompile(`reason IN \(([^)]*)\)`).FindStringSubmatch(createSQL)
	if len(reasonMatch) != 2 {
		t.Fatalf("source diagnostic table has no reason CHECK: %s", createSQL)
	}
	sqlReasons := make(map[string]bool)
	for _, value := range strings.Split(reasonMatch[1], ",") {
		sqlReasons[strings.Trim(strings.TrimSpace(value), "'")] = true
	}
	fixtureReasons := make(map[string]bool)
	for _, check := range fixture.Checks {
		if check.Valid {
			fixtureReasons[check.Reason] = true
		}
	}
	if !maps.Equal(sqlReasons, fixtureReasons) {
		t.Fatalf("database reason set %v differs from fixture reason set %v", sqlReasons, fixtureReasons)
	}
	// The checks use a separate session so existing migrated rows remain
	// untouched even when the lane under test is stored_content.
	checkID := "11111111-1111-4111-8111-111111111199"
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO sessions
(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format)
VALUES(?,'claude-code','fixture-model','source-lanes-host','source-lanes-project',1,2,3,'/synthetic/source.jsonl','jsonl')`,
		&sqlitex.ExecOptions{Args: []any{checkID}}); err != nil {
		t.Fatal(err)
	}
	for _, check := range fixture.Checks {
		t.Run(check.Name, func(t *testing.T) {
			_, laneErr := ingest.NewSourceUnavailabilityLane(check.Lane)
			_, reasonErr := ingest.NewSourceUnavailabilityReason(check.Reason)
			if valid := laneErr == nil && reasonErr == nil; valid != check.Valid {
				t.Fatalf("lane %q reason %q: constructors returned %v and %v, valid=%t", check.Lane, check.Reason, laneErr, reasonErr, check.Valid)
			}
			err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_source_unavailability(session_id, lane, reason) VALUES(?,?,?)`,
				&sqlitex.ExecOptions{Args: []any{checkID, check.Lane, check.Reason}})
			if (err == nil) != check.Valid {
				t.Fatalf("lane %q reason %q: database error=%v, valid=%t", check.Lane, check.Reason, err, check.Valid)
			}
			if err := sqlitex.ExecuteTransient(conn, `DELETE FROM session_source_unavailability WHERE session_id=?`,
				&sqlitex.ExecOptions{Args: []any{checkID}}); err != nil {
				t.Fatal(err)
			}
		})
	}
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_source_unavailability(session_id, lane, reason) VALUES(?,?,?)`,
		&sqlitex.ExecOptions{Args: []any{checkID, string(ingest.SourceLanePairRepair), string(ingest.SourceUnavailableNoSavedCopy)}}); err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_source_unavailability(session_id, lane, reason) VALUES(?,?,?)`,
		&sqlitex.ExecOptions{Args: []any{checkID, string(ingest.SourceLanePublicationCapture), string(ingest.SourceUnavailableRecordUnusable)}}); err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteTransient(conn, `DELETE FROM sessions WHERE session_id=?`, &sqlitex.ExecOptions{Args: []any{checkID}}); err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteTransient(conn, `SELECT lane FROM session_source_unavailability WHERE session_id=?`, &sqlitex.ExecOptions{
		Args: []any{checkID}, ResultFunc: func(stmt *sqlite.Stmt) error {
			t.Errorf("deleting the session left the %s lane behind", stmt.ColumnText(0))
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
}
