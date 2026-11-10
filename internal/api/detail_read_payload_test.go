package api

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
)

const detailReadPayloadSessionID = "4d5e6f70-8192-4a3b-9c4d-5e6f708192a3"

const detailReadPayloadTranscript = `{"sessionId":"` + detailReadPayloadSessionID + `","type":"user","uuid":"u1","timestamp":"2026-10-22T10:00:00Z","cwd":"/workspace","gitBranch":"main","message":{"role":"user","content":"benchmark the transfer handler"}}
{"sessionId":"` + detailReadPayloadSessionID + `","type":"assistant","uuid":"a1","timestamp":"2026-10-22T10:03:00Z","message":{"role":"assistant","model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"the ledger query takes most of the time"}],"usage":{"input_tokens":800,"output_tokens":200}}}
`

func ptrTo[T any](value T) *T { return &value }

// TestDetailReadPayloadCarriesStoredTitleMediansAndRevision reads one stored
// session the way the transcript page does after the full quality topic is
// retired. The title, the personal medians, and the revision all come from the
// local read payload; the durable push payload keeps no such fields.
func TestDetailReadPayloadCarriesStoredTitleMediansAndRevision(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(project, detailReadPayloadSessionID+".jsonl"), []byte(detailReadPayloadTranscript), 0o600); err != nil {
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

	ctx := context.Background()
	const wantTitle = "refactor the ingest pipeline"
	if err := db.SaveMetrics(ctx, &ingest.SessionMetrics{
		SessionID: detailReadPayloadSessionID,
		QualityMetrics: schema.QualityMetrics{
			TitleGenerated:       ptrTo(wantTitle),
			InputTokens:          ptrTo(800),
			OutputTokens:         ptrTo(200),
			RetryTokensWasted:    ptrTo(50),
			SpecQualityScore:     ptrTo(61.0),
			WithinSessionReverts: ptrTo(0),
		},
	}); err != nil {
		t.Fatalf("save metrics: %v", err)
	}

	revision, err := db.Revision(ctx)
	if err != nil {
		t.Fatalf("store revision: %v", err)
	}
	policy := sessionvisibility.New(config.SelectionConfig{Mode: config.SelectionModeAll})
	read, err := NewStoreDataProvider(db, policy, cfg.Output.BasePath).DetailReadPayload(ctx, detailReadPayloadSessionID)
	if err != nil {
		t.Fatalf("detail read: %v", err)
	}
	if read.Title != wantTitle {
		t.Fatalf("read title = %q, want %q", read.Title, wantTitle)
	}
	if read.Revision != revision {
		t.Fatalf("read revision = %q, want the store revision %q", read.Revision, revision)
	}
	if read.Medians == nil {
		t.Fatal("read payload carries no personal medians for a library with a metric sample")
	}
	if read.Medians.RetryShare == nil || *read.Medians.RetryShare != 0.05 {
		t.Fatalf("retryShare = %v, want 0.05", read.Medians.RetryShare)
	}
	if read.Medians.SpecQualityScore == nil || *read.Medians.SpecQualityScore != 61 {
		t.Fatalf("specQualityScore = %v, want 61", read.Medians.SpecQualityScore)
	}
	if read.Medians.WithinSessionReverts == nil || *read.Medians.WithinSessionReverts != 0 {
		t.Fatalf("withinSessionReverts = %v, want 0", read.Medians.WithinSessionReverts)
	}
	// The durable push payload is unchanged: the metadata never rides on it.
	durable := read.SessionDetailPayload
	if durable.ID != detailReadPayloadSessionID {
		t.Fatalf("durable payload id = %q, want %q", durable.ID, detailReadPayloadSessionID)
	}
}

// TestDetailReadPayloadOmitsAnEmptyBaseline pins the empty-library read: no
// metric sample means no medians object rather than a fabricated zero baseline.
func TestDetailReadPayloadOmitsAnEmptyBaseline(t *testing.T) {
	t.Parallel()
	medians := readPayloadMedians(schema.PersonalMedians{})
	if medians != nil {
		t.Fatalf("empty baseline = %+v, want nil", medians)
	}
	score := 61.0
	if got := readPayloadMedians(schema.PersonalMedians{SpecQualityScore: &score}); got == nil || *got.SpecQualityScore != score {
		t.Fatalf("non-empty baseline = %+v, want the score %v", got, score)
	}
}
