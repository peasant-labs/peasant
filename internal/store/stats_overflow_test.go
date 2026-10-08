package store

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitemigration"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

type statsOverflowCase struct {
	Name           string           `yaml:"name"`
	StatsJSON      string           `yaml:"statsJson"`
	Want           v62ExpectedStats `yaml:"want"`
	Overflow       *string          `yaml:"overflow"`
	Keys           []string         `yaml:"keys"`
	Damage         string           `yaml:"damage"`
	Expect         string           `yaml:"expect"`
	ReportDriver   bool             `yaml:"reportDriver"`
	Refresh        bool             `yaml:"refresh"`
	RefreshCount   int              `yaml:"refreshCount"`
	LegacySeedOnly bool             `yaml:"legacySeedOnly"`
}

//go:embed testdata/stats_overflow.yaml
var statsOverflowYAML []byte

//go:embed testdata/stats_overflow.manifest.yaml
var statsOverflowManifestYAML []byte

func LoadStatsOverflowFixtures(t *testing.T) []statsOverflowCase {
	t.Helper()
	var fixtures struct {
		Cases []statsOverflowCase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(statsOverflowYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatal(err)
	}
	manifest, err := decodeRecoveryRequiredNames(statsOverflowManifestYAML)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range fixtures.Cases {
		if c.StatsJSON == "" || (c.Expect != "converted" && c.Expect != "rolled-back") {
			t.Fatalf("incomplete stat fixture %q", c.Name)
		}
		if c.Refresh && c.RefreshCount < 1 {
			t.Fatalf("stat refresh fixture %q has no activation", c.Name)
		}
		names = append(names, c.Name)
	}
	if err := validateRecoveryRequiredNames(manifest, names, "stat overflow"); err != nil {
		t.Fatal(err)
	}
	return fixtures.Cases
}

func assertStatsPresence(t *testing.T, got CapturedStats, want v62ExpectedStats) {
	t.Helper()
	expected := CapturedStats{TurnCount: want.TurnCount, InputSubmissionCount: want.InputSubmissionCount,
		ToolCallCount: want.ToolCallCount, SubagentCount: want.SubagentCount, DurationMs: want.DurationMs,
		TokensIn: want.TokensIn, TokensOut: want.TokensOut, ThoughtTokens: want.ThoughtTokens,
		CachedReadTokens: want.CachedReadTokens, CachedWriteTokens: want.CachedWriteTokens}
	got.SessionID, got.Source, got.UpdatedAtMs, got.SeedJSON, got.Overflow = "", "", 0, nil, nil
	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("captured presence changed: got %+v; want %+v", got, expected)
	}
}

func TestStatsOverflowFamily(t *testing.T) {
	for _, c := range LoadStatsOverflowFixtures(t) {
		t.Run(c.Name, func(t *testing.T) {
			dir := t.TempDir()
			sid, err := schema.NewSessionID("11111111-1111-4111-8111-111111111111")
			if err != nil {
				t.Fatal(err)
			}
			genID := "gen_stats_raw"
			conn, err := sqlite.OpenConn(filepath.Join(dir, "generations.db"), sqlite.OpenReadWrite, sqlite.OpenCreate)
			if err != nil {
				t.Fatal(err)
			}
			predecessor := sqlitemigration.Schema{Migrations: dbSchema.Migrations[:61], MigrationOptions: dbSchema.MigrationOptions[:61]}
			if err := sqlitemigration.Migrate(t.Context(), conn, predecessor); err != nil {
				t.Fatal(err)
			}
			seedV62Predecessor(t, conn, v62Fixtures{StatsCases: []v62StatsCase{{SessionID: string(sid), ActiveGeneration: &genID, Generations: []v62GenerationSeed{{ID: genID, MetadataJSON: `{"stats":` + c.StatsJSON + `}`, InstalledAtMs: 1}}}}})
			if err := conn.Close(); err != nil {
				t.Fatal(err)
			}
			s := openGenerationStoreAt(t, dir)
			t.Cleanup(func() {
				_ = s.Close()
			})
			backfilled, err := s.ReadCapturedStats(t.Context(), sid)
			if err != nil {
				t.Fatal(err)
			}
			assertStatsPresence(t, backfilled, c.Want)
			if backfilled.SeedJSON == nil || *backfilled.SeedJSON != c.StatsJSON {
				t.Fatalf("backfill lost raw stats: %+v", backfilled.SeedJSON)
			}

			// Add the entry/blob evidence needed for the real conversion. The
			// backfilled stats row stays intact, and the same raw stats subtree
			// is restored beside the complete captured metadata document.
			execMigrateSQL(t, s, `DELETE FROM session_projection_generations WHERE session_id='`+string(sid)+`'`)
			seedMigrateProfile(t, s, filepath.Join(dir, "artifacts"), sid, "clean", genID)
			// The general file-backed helper seeds typed measurements too;
			// restore the exact raw backfill row so it cannot invent presence.
			execMigrateSQL(t, s, `DELETE FROM session_captured_stats WHERE session_id='`+string(sid)+`'`)
			if _, err := s.UpsertCapturedStats(t.Context(), backfilled); err != nil {
				t.Fatal(err)
			}
			conn, err = s.pool.Take(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			err = sqlitex.Execute(conn, `UPDATE session_projection_generations SET metadata_json=json_set(metadata_json,'$.stats',json(?)) WHERE session_id=?`, &sqlitex.ExecOptions{Args: []any{c.StatsJSON, string(sid)}})
			s.pool.Put(conn)
			if err != nil {
				t.Fatal(err)
			}
			if c.Refresh {
				if c.LegacySeedOnly {
					conn, err := s.pool.Take(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					err = sqlitex.Execute(conn, `DELETE FROM session_captured_stats WHERE session_id=?`, &sqlitex.ExecOptions{Args: []any{string(sid)}})
					if err == nil {
						err = sqlitex.Execute(conn, `UPDATE sessions SET metric_seed_json=? WHERE session_id=?`, &sqlitex.ExecOptions{Args: []any{c.StatsJSON, string(sid)}})
					}
					s.pool.Put(conn)
					if err != nil {
						t.Fatal(err)
					}
				}
				for ordinal := 1; ordinal <= c.RefreshCount; ordinal++ {
					v2, blobs := buildTestGeneration(t, sid, fmt.Sprintf("gen_stats_refresh_%d", ordinal), fmt.Sprintf("fresh measured content %d", ordinal), "fresh tool input", "fresh tool output")
					v2.Generation.Metadata.ModelHarness = schema.HarnessClaudeCode
					activation := GenerationActivation{Generation: v2, Blobs: blobs, IndexerVersion: 1, IndexedAtMs: int64(1000 + ordinal),
						ContentCapture: ingest.SessionContentCaptureWrite{
							Status: ingest.ContentCaptureComplete, SourceAuthority: ingest.ContentSourceNewIngest,
							TranscriptOrigin: ingest.TranscriptOriginFile, CaptureFormat: ingest.ContentCaptureFormatFull,
						},
					}
					activation.Prepared, err = s.StageGeneration(t.Context(), activation)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := s.ActivateGeneration(t.Context(), activation); err != nil {
						t.Fatal(err)
					}
					assertRefreshedStatsOverflow(t, s, sid, v2.Generation.Metadata.Stats, c)
				}
				return
			}
			if c.Damage != "" {
				if c.Damage != "drop-overflow" {
					t.Fatalf("unknown stat damage %q", c.Damage)
				}
				previous := migrateShadowSeam
				migrateShadowSeam = func(conn *sqlite.Conn, _ string) error {
					return sqlitex.Execute(conn, `UPDATE session_captured_stats SET overflow=NULL WHERE session_id=?`, &sqlitex.ExecOptions{Args: []any{string(sid)}})
				}
				t.Cleanup(func() {
					migrateShadowSeam = previous
				})
			}
			var result MigrateResult
			var outcome MigrateOutcome
			if c.ReportDriver {
				result, err = s.Migrate(t.Context(), MigrateOptions{Limit: 1})
				if result.Converted == 1 {
					outcome = MigrateOutcomeConverted
				}
			} else {
				outcome, err = s.MigrateSession(t.Context(), sid)
			}
			if c.Expect == "rolled-back" {
				var rollback *MigrateDataRollbackError
				if outcome != MigrateOutcomeRolledBack || !errors.As(err, &rollback) || rollback.Dimension != "stats-overflow" {
					t.Fatalf("dropped overflow escaped shadow check: %s, %v", outcome, err)
				}
				return
			}
			if err != nil || outcome != MigrateOutcomeConverted {
				t.Fatalf("conversion: %s, %v", outcome, err)
			}
			got, err := s.ReadCapturedStats(t.Context(), sid)
			if err != nil {
				t.Fatal(err)
			}
			assertStatsPresence(t, got, c.Want)
			if !reflect.DeepEqual(got.Overflow, c.Overflow) || got.SeedJSON == nil || *got.SeedJSON != c.StatsJSON {
				t.Fatalf("raw stats retention: %+v", got)
			}
			var warnings []schema.DiagnosticEntry
			if err := s.WithSessionSnapshot(t.Context(), sid, func(snapshot indexformat.ReadSnapshot) error {
				warnings = snapshot.Metadata.Diagnostics.Warnings
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			keys := make([]string, 0)
			for _, w := range warnings {
				if w.ErrorType == "stats-overflow" {
					keys = append(keys, strings.TrimPrefix(w.Location, "stats."))
					if !strings.Contains(w.Message, "harness claude-code") || w.Remediation == "" {
						t.Fatalf("stat diagnostic lacks origin/remedy: %+v", w)
					}
				}
			}
			if len(c.Keys) == 0 {
				c.Keys = []string{}
			}
			if !reflect.DeepEqual(keys, c.Keys) {
				t.Fatalf("diagnostic keys: got %v, want %v", keys, c.Keys)
			}
			if c.ReportDriver {
				for _, key := range c.Keys {
					if result.StatsOverflowKeys[key] != 1 {
						t.Fatalf("stat %s absent from report: %+v", key, result)
					}
				}
				if len(result.StatsOverflowKeys) != len(c.Keys) {
					t.Fatalf("report key membership differs: %+v", result.StatsOverflowKeys)
				}
			}
			seed, err := s.ReadMetricSeed(t.Context(), sid)
			if err != nil {
				t.Fatal(err)
			}
			var typed ingest.StatsInfo
			if err := json.Unmarshal([]byte(c.StatsJSON), &typed); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(seed, &typed) {
				t.Fatalf("typed metric seed differs: got %+v; want %+v", seed, typed)
			}
		})
	}
}

func assertRefreshedStatsOverflow(t *testing.T, s *Store, sid schema.SessionID, measured schema.SessionStats, c statsOverflowCase) {
	t.Helper()
	got, err := s.ReadCapturedStats(t.Context(), sid)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Overflow, c.Overflow) {
		t.Fatalf("refresh dropped retained raw stat overflow: got %v, want %v", got.Overflow, c.Overflow)
	}
	seed, err := s.ReadMetricSeed(t.Context(), sid)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(measured)
	if err != nil {
		t.Fatal(err)
	}
	var typed ingest.StatsInfo
	if err := json.Unmarshal(raw, &typed); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(seed, &typed) {
		t.Fatalf("refresh seed is not current adapter measurements: got %+v, want %+v", seed, typed)
	}
	var keys []string
	if err := s.WithSessionSnapshot(t.Context(), sid, func(snapshot indexformat.ReadSnapshot) error {
		for _, warning := range snapshot.Metadata.Diagnostics.Warnings {
			if warning.ErrorType == "stats-overflow" {
				keys = append(keys, strings.TrimPrefix(warning.Location, "stats."))
				if !strings.Contains(warning.Message, "harness claude-code") || warning.Remediation == "" {
					t.Errorf("refresh stat diagnostic lacks origin/remedy: %+v", warning)
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(keys, c.Keys) {
		t.Fatalf("refresh diagnostic keys: got %v, want %v", keys, c.Keys)
	}
	var report MigrateResult
	if err := s.accumulateMigrateStatsOverflow(t.Context(), sid, &report); err != nil {
		t.Fatal(err)
	}
	if len(report.StatsOverflowKeys) != len(c.Keys) {
		t.Fatalf("refresh diagnostic count membership differs: %+v", report.StatsOverflowKeys)
	}
	for _, key := range c.Keys {
		if report.StatsOverflowKeys[key] != 1 {
			t.Fatalf("refresh diagnostic %s missing or duplicated: %+v", key, report.StatsOverflowKeys)
		}
	}
}
