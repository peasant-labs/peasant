package store_test

import (
	"context"
	_ "embed"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/session_captured_stats.yaml
var sessionCapturedStatsYAML []byte

//go:embed testdata/session_captured_stats.manifest.yaml
var sessionCapturedStatsManifestYAML []byte

type capturedStatsValues struct {
	TurnCount            *int              `yaml:"turnCount"`
	ToolCallCount        *int              `yaml:"toolCallCount"`
	InputSubmissionCount *int64            `yaml:"inputSubmissionCount"`
	TokensOut            *int              `yaml:"tokensOut"`
	SeedJSON             *string           `yaml:"seedJSON"`
	Overflow             *string           `yaml:"overflow"`
	Source               store.StatsSource `yaml:"source"`
	UpdatedAtMs          int64             `yaml:"updatedAtMs"`
}

func (v capturedStatsValues) row(id schema.SessionID) store.CapturedStats {
	return store.CapturedStats{SessionID: id, TurnCount: v.TurnCount, ToolCallCount: v.ToolCallCount,
		InputSubmissionCount: v.InputSubmissionCount, TokensOut: v.TokensOut, SeedJSON: v.SeedJSON,
		Overflow: v.Overflow, Source: v.Source, UpdatedAtMs: v.UpdatedAtMs}
}

type capturedStatsUpdate struct {
	Values           capturedStatsValues `yaml:"values"`
	WantApplied      bool                `yaml:"wantApplied"`
	MirrorFailureSQL string              `yaml:"mirrorFailureSQL"`
	WantError        string              `yaml:"wantError"`
}

type capturedStatsSeed struct {
	TurnCount     int `yaml:"turnCount"`
	ToolCallCount int `yaml:"toolCallCount"`
}

type capturedStatsCase struct {
	Name         string                `yaml:"name"`
	Action       string                `yaml:"action"`
	Updates      []capturedStatsUpdate `yaml:"updates"`
	Want         capturedStatsValues   `yaml:"want"`
	WantMirror   *int64                `yaml:"wantMirror"`
	WantSeed     *capturedStatsSeed    `yaml:"wantSeed"`
	CheckSeed    bool                  `yaml:"checkSeed"`
	NativeTurns  int                   `yaml:"nativeTurns"`
	NativeTools  int                   `yaml:"nativeTools"`
	NativeOutput int                   `yaml:"nativeOutput"`
	LegacyTurns  int                   `yaml:"legacyTurns"`
	LegacyTools  int                   `yaml:"legacyTools"`
	LegacyOutput int                   `yaml:"legacyOutput"`
	PeakInput    int                   `yaml:"peakInput"`
	DurationMs   int64                 `yaml:"durationMs"`
}

func LoadSessionCapturedStatsFixtures(t *testing.T) []capturedStatsCase {
	t.Helper()
	var fixtures struct {
		Cases []capturedStatsCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(sessionCapturedStatsYAML, &fixtures); err != nil {
		t.Fatal(err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(sessionCapturedStatsManifestYAML, "captured stats")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range fixtures.Cases {
		if c.Action == "" {
			t.Fatalf("captured stats %q has no action", c.Name)
		}
		names = append(names, c.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "captured stats"); err != nil {
		t.Fatal(err)
	}
	return fixtures.Cases
}

func TestSessionCapturedStatsFamily(t *testing.T) {
	for _, c := range LoadSessionCapturedStatsFixtures(t) {
		t.Run(c.Name, func(t *testing.T) {
			if c.Action == "list-detail" {
				runListDetailStatsSource(t, c)
				return
			}
			if c.Action != "upsert" || len(c.Updates) == 0 {
				t.Fatalf("invalid captured stats action %q or empty updates", c.Action)
			}
			s := openTestStore(t)
			ctx := context.Background()
			id := schema.SessionID("ses-stats-fixture")
			gen := "gen-stats-fixture"
			seedStatsSession(t, s, string(id), &gen)
			for i, update := range c.Updates {
				var before store.CapturedStats
				var beforeMirror any
				if update.MirrorFailureSQL != "" {
					if update.WantError == "" || update.WantApplied {
						t.Fatalf("update %d mirror failure must specify an error and unapplied result", i)
					}
					var err error
					before, err = s.ReadCapturedStats(ctx, id)
					if err != nil {
						t.Fatalf("read captured row before failed update %d: %v", i, err)
					}
					beforeMirror = readStatsMirror(t, s, string(id))
					conn, err := s.PoolForTest().Take(ctx)
					if err != nil {
						t.Fatal(err)
					}
					err = sqlitex.ExecuteTransient(conn, update.MirrorFailureSQL, nil)
					s.PoolForTest().Put(conn)
					if err != nil {
						t.Fatalf("install mirror-write refusal for update %d: %v", i, err)
					}
				}
				applied, err := s.UpsertCapturedStats(ctx, update.Values.row(id))
				if update.WantError != "" {
					if update.MirrorFailureSQL == "" {
						t.Fatalf("update %d expects a mirror error without a failure trigger", i)
					}
					if err == nil || !strings.Contains(err.Error(), update.WantError) || applied != update.WantApplied {
						t.Fatalf("update %d = (%v, %v), want (%v, error containing %q)", i, applied, err, update.WantApplied, update.WantError)
					}
					after, err := s.ReadCapturedStats(ctx, id)
					if err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(after, before) {
						t.Fatalf("failed mirror update %d changed captured row: got %+v, prior %+v", i, after, before)
					}
					if afterMirror := readStatsMirror(t, s, string(id)); afterMirror != beforeMirror {
						t.Fatalf("failed mirror update %d changed mirror: got %v, prior %v", i, afterMirror, beforeMirror)
					}
					continue
				}
				if err != nil || applied != update.WantApplied {
					t.Fatalf("update %d = (%v, %v), want (%v, nil)", i, applied, err, update.WantApplied)
				}
			}
			got, err := s.ReadCapturedStats(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if want := c.Want.row(id); !reflect.DeepEqual(got, want) {
				t.Fatalf("captured row = %+v, want %+v", got, want)
			}
			var mirror any
			if c.WantMirror != nil {
				mirror = *c.WantMirror
			}
			if got := readStatsMirror(t, s, string(id)); got != mirror {
				t.Fatalf("mirror = %v, want %v", got, mirror)
			}
			if c.CheckSeed {
				got, err := s.ReadMetricSeed(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				var want *schema.SessionStats
				if c.WantSeed != nil {
					want = &schema.SessionStats{TurnCount: c.WantSeed.TurnCount, ToolCallCount: c.WantSeed.ToolCallCount}
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("seed = %+v, want %+v", got, c.WantSeed)
				}
			}
		})
	}
}
