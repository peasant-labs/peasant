package api

import (
	"context"
	_ "embed"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/detail_totals.yaml
var detailTotalsYAML []byte

type detailTotals struct {
	TotalTokens  int     `yaml:"totalTokens"`
	TokensIn     int     `yaml:"tokensIn"`
	TokensOut    int     `yaml:"tokensOut"`
	DurationMins float64 `yaml:"durationMins"`
}

type detailTotalsFixture struct {
	Required []string `yaml:"requiredNames"`
	Sessions []struct {
		Name       string `yaml:"name"`
		Transcript string `yaml:"transcript"`
	} `yaml:"sessions"`
	Overlay []struct {
		Name   string        `yaml:"name"`
		Detail detailTotals  `yaml:"detail"`
		Row    *detailTotals `yaml:"row"`
		Want   detailTotals  `yaml:"want"`
	} `yaml:"overlay"`
}

func loadDetailTotalsFixture(t *testing.T) detailTotalsFixture {
	t.Helper()
	var fixture detailTotalsFixture
	if err := testutil.DecodeFixtureYAML(detailTotalsYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	present := map[string]bool{}
	for _, c := range fixture.Sessions {
		present[c.Name] = true
	}
	for _, c := range fixture.Overlay {
		present[c.Name] = true
	}
	if err := testutil.RequireFixtureNames("testdata/detail_totals.yaml", "detail totals", fixture.Required, present); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (d detailTotals) of(detail *schema.SessionDetailPayload) detailTotals {
	return detailTotals{TotalTokens: detail.TotalTokens, TokensIn: detail.TokensIn, TokensOut: detail.TokensOut, DurationMins: detail.DurationMins}
}

func TestWithStoredTotals(t *testing.T) {
	t.Parallel()
	for _, c := range loadDetailTotalsFixture(t).Overlay {
		t.Run(c.Name, func(t *testing.T) {
			detail := &schema.SessionDetailPayload{TotalTokens: c.Detail.TotalTokens, TokensIn: c.Detail.TokensIn, TokensOut: c.Detail.TokensOut, DurationMins: c.Detail.DurationMins}
			var row *store.SessionRow
			if c.Row != nil {
				row = &store.SessionRow{TokensTotal: c.Row.TotalTokens, InputTokens: c.Row.TokensIn, OutputTokens: c.Row.TokensOut, DurationMinutes: c.Row.DurationMins}
			}
			withStoredTotals(detail, row)
			if got := (detailTotals{}).of(detail); got != c.Want {
				t.Fatalf("totals = %+v, want %+v", got, c.Want)
			}
		})
	}
}

// TestManagedDetailReadCarriesStoredTotals indexes a session the way a harvest
// does (a managed generation) and reads it the way the transcript page does.
// The read carries the stored totals; the publication input's detail keeps
// today's values, so no published content hash moves.
func TestManagedDetailReadCarriesStoredTotals(t *testing.T) {
	t.Parallel()
	const sessionID = "4d5e6f70-8192-4a3b-9c4d-5e6f708192a3"
	for _, c := range loadDetailTotalsFixture(t).Sessions {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			hs := newTestXDGHomes(t)
			root := t.TempDir()
			cfg := config.BaseConfig()
			cfg.Output.BasePath = filepath.Join(root, "managed")
			for harness := range ingest.DefaultAdapterRegistry {
				if provider, ok := cfg.Sources.Provider(harness); ok {
					provider.Enabled = harness == ingest.HarnessClaudeCode
				}
			}
			source, _ := cfg.Sources.Provider(ingest.HarnessClaudeCode)
			source.Paths = []string{filepath.Join(root, "recordings")}
			project := filepath.Join(root, "recordings", "-workspace")
			if err := os.MkdirAll(project, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(project, sessionID+".jsonl"), []byte(strings.ReplaceAll(c.Transcript, "SESSION_ID", sessionID)), 0o600); err != nil {
				t.Fatal(err)
			}
			harvestLikeTheCLI(t, hs.dbPath(), cfg)
			storeOptions, err := store.ManagedGenerationOptions(cfg.Output.BasePath)
			if err != nil {
				t.Fatal(err)
			}
			db, err := store.Open(hs.dbPath(), append(storeOptions, store.WithSkipMigrations())...)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			policy, err := sessionvisibility.New(config.SelectionConfig{Mode: config.SelectionModeAll})
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			row, err := db.SessionByID(ctx, sessionID)
			if err != nil || row == nil {
				t.Fatalf("stored row = %v, %v", row, err)
			}
			stored := detailTotals{TotalTokens: row.TokensTotal, TokensIn: row.InputTokens, TokensOut: row.OutputTokens, DurationMins: row.DurationMinutes}
			if stored.TotalTokens == 0 || stored.DurationMins == 0 {
				t.Fatalf("the stored row holds no totals to show: %+v", stored)
			}
			read, err := NewStoreDataProvider(db, policy, cfg.Output.BasePath).DetailReadPayload(ctx, sessionID)
			if err != nil {
				t.Fatal(err)
			}
			if got := (detailTotals{}).of(&read.SessionDetailPayload); got != stored {
				t.Fatalf("detail read totals = %+v, want the stored %+v", got, stored)
			}
			_, published, err := push.LoadPublicationInput(ctx, db, sessionID)
			if err != nil {
				t.Fatal(err)
			}
			if got := (detailTotals{}).of(published); got != (detailTotals{}) {
				t.Fatalf("publication detail totals = %+v; the packaging path must not change until its follow-up", got)
			}
		})
	}
}
