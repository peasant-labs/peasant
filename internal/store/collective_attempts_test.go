package store

import (
	"context"
	_ "embed"
	"errors"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitemigration"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/collective_attempts.yaml
var collectiveAttemptFixtures []byte

type collectiveAttemptFixture struct {
	Name           string                     `yaml:"name"`
	Mode           string                     `yaml:"mode"`
	Before         []schema.VillageShareEvent `yaml:"before"`
	Incoming       []schema.VillageShareEvent `yaml:"incoming"`
	Newer          []schema.VillageShareEvent `yaml:"newer"`
	Expected       []schema.VillageShareEvent `yaml:"expected"`
	AbsentOrdinals []int32                    `yaml:"absentOrdinals"`
	WantError      string                     `yaml:"wantError"`
	WantChanged    bool                       `yaml:"wantChanged"`
	WantGeneration int64                      `yaml:"wantGeneration"`
}

type collectiveAttemptFixtureSet struct {
	RequiredNames []string                   `yaml:"requiredNames"`
	Cases         []collectiveAttemptFixture `yaml:"cases"`
	Checks        []struct {
		Name   string `yaml:"name"`
		Column string `yaml:"column"`
	} `yaml:"checks"`
}

func loadCollectiveAttemptFixtures(t *testing.T) collectiveAttemptFixtureSet {
	t.Helper()
	var fixtures collectiveAttemptFixtureSet
	decoder := yaml.NewDecoder(strings.NewReader(string(collectiveAttemptFixtures)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, fixture := range fixtures.Cases {
		if fixture.Name == "" || names[fixture.Name] {
			t.Fatal("missing/duplicate collective attempt fixture")
		}
		names[fixture.Name] = true
		if fixture.Before == nil || fixture.Incoming == nil || fixture.Expected == nil {
			t.Fatalf("fixture %s needs explicit complete arrays", fixture.Name)
		}
	}
	for _, name := range fixtures.RequiredNames {
		if !names[name] {
			t.Fatalf("required collective attempt fixture %s missing", name)
		}
	}
	checks := map[string]bool{}
	for _, check := range fixtures.Checks {
		if check.Name == "" || checks[check.Name] {
			t.Fatal("missing/duplicate SQL menu fixture")
		}
		checks[check.Name] = true
	}
	for _, name := range strings.Fields("attempt-status-check-exact-schema-membership attempt-actor-check-exact-schema-membership") {
		if !checks[name] {
			t.Fatalf("required SQL menu fixture %s missing", name)
		}
	}
	return fixtures
}

func TestCollectiveAttemptSourceTransactions(t *testing.T) {
	for _, fixture := range loadCollectiveAttemptFixtures(t).Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			ctx := t.Context()
			s, err := Open(filepath.Join(t.TempDir(), "observations.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			home := t.TempDir()
			creds := auth.Credentials{APIKey: "fixture-key", KeyID: "fixture-key-id", UserID: "11111111-1111-4111-8111-111111111111", Username: "fixture", VillageURL: "https://village.example/a"}
			save := func(value auth.Credentials) auth.CredentialSnapshot {
				t.Helper()
				if err := auth.SaveCredentialsFrom(&value, home); err != nil {
					t.Fatal(err)
				}
				snapshot, err := auth.CaptureCredentials(ctx, home)
				if err != nil {
					t.Fatal(err)
				}
				return snapshot
			}
			snapshot := save(creds)
			collective := schema.VillageUUID("33333333-3333-4333-8333-333333333333")
			transcript := schema.TranscriptID("22222222-2222-4222-8222-222222222222")
			firstSeen := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
			secondSeen := firstSeen.Add(24 * time.Hour)
			begin := func(snapshot auth.CredentialSnapshot) CollectiveObservationInvocation {
				t.Helper()
				invocation, err := s.BeginCollectiveObservation(ctx, snapshot)
				if err != nil {
					t.Fatal(err)
				}
				return invocation
			}
			install := func(invocation CollectiveObservationInvocation, id schema.TranscriptID, events []schema.VillageShareEvent, at time.Time) (ContributionAttemptInstall, error) {
				return s.InstallContributionAttemptSources(ctx, invocation, []ContributionAttemptSource{{CollectiveID: collective, TranscriptID: id, Events: events, ObservedAt: at}})
			}
			if _, err := install(begin(snapshot), transcript, fixture.Before, firstSeen); err != nil {
				t.Fatal(err)
			}
			older := begin(snapshot)
			switch fixture.Mode {
			case "newer-overlap":
				if _, err := install(begin(snapshot), transcript, fixture.Newer, secondSeen); err != nil {
					t.Fatal(err)
				}
			case "newer-disjoint":
				if _, err := install(begin(snapshot), schema.TranscriptID("44444444-4444-4444-8444-444444444444"), fixture.Newer, secondSeen); err != nil {
					t.Fatal(err)
				}
			case "switch-back":
				other := creds
				other.UserID = "55555555-5555-4555-8555-555555555555"
				save(other)
				snapshot = save(creds)
			case "other-principal":
				other := creds
				other.UserID = "55555555-5555-4555-8555-555555555555"
				snapshot = save(other)
				older = begin(snapshot)
			case "other-base-path":
				other := creds
				other.VillageURL = "https://village.example/b"
				snapshot = save(other)
				older = begin(snapshot)
			case "":
			default:
				t.Fatalf("unsupported fixture mode %q", fixture.Mode)
			}
			result, err := install(older, transcript, fixture.Incoming, secondSeen)
			switch fixture.WantError {
			case "":
				if err != nil {
					t.Fatal(err)
				}
				if result.Changed != fixture.WantChanged || result.Generation != fixture.WantGeneration {
					t.Fatalf("install effect %#v differs from expected changed=%v generation=%d", result, fixture.WantChanged, fixture.WantGeneration)
				}
			case "superseded":
				if !errors.Is(err, ErrObservationSuperseded) {
					t.Fatalf("expected source CAS refusal: %v", err)
				}
			case "credentials":
				if !errors.Is(err, auth.ErrCredentialsChanged) {
					t.Fatalf("expected credential generation refusal: %v", err)
				}
			case "contract":
				if err == nil || !strings.Contains(err.Error(), "attempt validation") {
					t.Fatalf("expected canonical conflict refusal: %v", err)
				}
			default:
				t.Fatalf("unsupported expected error %q", fixture.WantError)
			}
			for _, expected := range fixture.Expected {
				got, err := s.ReadContributionAttempt(ctx, snapshot, collective, transcript, expected.EventNum)
				if err != nil {
					t.Fatal(err)
				}
				if got == nil || !reflect.DeepEqual(got.Attempt, expected) {
					t.Fatalf("retained attempt %#v differs from expected %#v", got, expected)
				}
				wantFirstSeen := secondSeen
				for _, prior := range fixture.Before {
					if prior.EventNum == expected.EventNum {
						wantFirstSeen = firstSeen
					}
				}
				if !got.FirstSeenAt.Equal(wantFirstSeen) {
					t.Fatalf("first observation time changed: %v want %v", got.FirstSeenAt, wantFirstSeen)
				}
			}
			for _, ordinal := range fixture.AbsentOrdinals {
				got, err := s.ReadContributionAttempt(ctx, snapshot, collective, transcript, ordinal)
				if err != nil || got != nil {
					t.Fatalf("failed transaction leaked ordinal %d: %#v %v", ordinal, got, err)
				}
			}
			if fixture.Mode == "other-principal" || fixture.Mode == "other-base-path" {
				got, err := s.ReadContributionAttempt(ctx, snapshot, collective, transcript, 1)
				if err != nil || got != nil {
					t.Fatalf("private evidence crossed scope: %#v %v", got, err)
				}
			}
			// Inspect generation through a new complete equivalent source install,
			// never a private helper or a synthetic token.
			probe, err := install(begin(snapshot), transcript, fixture.Expected, secondSeen.Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			if probe.Changed || probe.Generation != fixture.WantGeneration {
				t.Fatalf("stable payload/generation changed on equivalent validation: %#v", probe)
			}
		})
	}
}

func TestCollectiveAttemptSQLMenus(t *testing.T) {
	conn, err := sqlite.OpenConn(":memory:", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := sqlitex.ExecuteScript(conn, migrationV68, nil); err != nil {
		t.Fatal(err)
	}
	var ddl string
	if err := sqlitex.ExecuteTransient(conn, `SELECT sql FROM sqlite_master WHERE name='remote_contribution_attempts'`, &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error { ddl = stmt.ColumnText(0); return nil }}); err != nil {
		t.Fatal(err)
	}
	for _, check := range loadCollectiveAttemptFixtures(t).Checks {
		t.Run(check.Name, func(t *testing.T) {
			want := map[string]bool{}
			switch check.Column {
			case "status":
				for _, value := range schema.AllVillageShareStatuses {
					want[string(value)] = true
				}
			case "decided_by_actor":
				for _, value := range schema.AllVillageShareEventActors {
					want[string(value)] = true
				}
			default:
				t.Fatalf("unknown schema menu column %q", check.Column)
			}
			pattern := regexp.MustCompile(regexp.QuoteMeta(check.Column) + ` IN \(([^)]*)\)`)
			match := pattern.FindStringSubmatch(ddl)
			if match == nil {
				t.Fatalf("missing closed SQL CHECK for %s", check.Column)
			}
			got := map[string]bool{}
			for _, token := range strings.Split(match[1], ",") {
				token = strings.TrimSpace(token)
				if !strings.HasPrefix(token, "'") || !strings.HasSuffix(token, "'") {
					t.Fatalf("nonliteral SQL menu token %q", token)
				}
				got[strings.Trim(token, "'")] = true
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("SQL %s membership %v differs from canonical schema %v", check.Column, got, want)
			}
		})
	}
}

func TestMigrationV68UpgradeAndBaseline(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "upgrade.db")
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite, sqlite.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	predecessor := sqlitemigration.Schema{Migrations: dbSchema.Migrations[:67], MigrationOptions: dbSchema.MigrationOptions[:67]}
	if err := sqlitemigration.Migrate(ctx, conn, predecessor); err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteScript(conn, `CREATE TABLE prior_observation_upgrade_sentinel(value TEXT); INSERT INTO prior_observation_upgrade_sentinel(value) VALUES('retained');`, nil); err != nil {
		t.Fatal(err)
	}
	if err := sqlitemigration.Migrate(ctx, conn, dbSchema); err != nil {
		t.Fatal(err)
	}
	var sentinel string
	if err := sqlitex.ExecuteTransient(conn, `SELECT value FROM prior_observation_upgrade_sentinel`, &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error { sentinel = stmt.ColumnText(0); return nil }}); err != nil || sentinel != "retained" {
		t.Fatalf("upgrade lost prior row: %q %v", sentinel, err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	assertFreshEqualsMigrated(t, "collective-observation-baseline-upgrade-parity")
}
