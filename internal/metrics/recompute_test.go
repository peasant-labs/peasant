package metrics_test

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/metrics"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/recompute.yaml
var recomputeYAML []byte

type recomputeSaveStore struct {
	*store.Store
	fail bool
}

var _ ingest.MetricsStore = (*recomputeSaveStore)(nil)

func (s *recomputeSaveStore) SaveMetrics(ctx context.Context, result *ingest.SessionMetrics) error {
	if s.fail {
		return errors.New("synthetic metrics save failure")
	}
	return s.Store.SaveMetrics(ctx, result)
}

func (s *recomputeSaveStore) SaveMetricsForInput(ctx context.Context, input *ingest.MetricInput, result *ingest.SessionMetrics) error {
	if s.fail {
		return errors.New("synthetic metrics save failure")
	}
	return s.Store.SaveMetricsForInput(ctx, input, result)
}

func TestRecomputeMetricsPreservesActualProducerAndLastGoodValues(t *testing.T) {
	t.Parallel()
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		SessionID     string   `yaml:"sessionID"`
		ProjectHash   string   `yaml:"projectHash"`
		HostSlug      string   `yaml:"hostSlug"`
		PriorTurns    int      `yaml:"priorTurns"`
		StartedAt     int64    `yaml:"startedAt"`
		Cases         []struct {
			Name         string `yaml:"name"`
			VersionDelta int    `yaml:"versionDelta"`
			FailSave     bool   `yaml:"failSave"`
			WantTurns    int    `yaml:"wantTurns"`
			WantComputed int    `yaml:"wantComputed"`
			WantError    bool   `yaml:"wantError"`
		} `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(recomputeYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatal("metric recomputation fixture requires one document")
	}
	required := []string{"current-version-recomputed", "future-producer-preserved", "failed-save-preserves-last-good"}
	if !reflect.DeepEqual(required, fixture.RequiredNames) {
		t.Fatal("metric recomputation required-name manifest changed")
	}
	seen := make(map[string]bool)
	for _, row := range fixture.Cases {
		if row.Name == "" || seen[row.Name] {
			t.Fatalf("invalid metric fixture %q", row.Name)
		}
		seen[row.Name] = true
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			db, err := store.Open(filepath.Join(t.TempDir(), "metrics.db"), store.WithPoolSize(1))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			sid := mustSessionID(t, fixture.SessionID)
			meta := ingest.NewUnifiedMetadata()
			meta.SessionID, meta.ModelHarness = sid, ingest.HarnessClaudeCode
			meta.Source.Format = ingest.SourceFormatJSONL
			meta.HostSlug = ingest.HostSlug(fixture.HostSlug)
			meta.Project.Hash = ingest.ProjectHash(fixture.ProjectHash)
			meta.Timestamp.Start, meta.Timestamp.End, meta.Timestamp.Ingested = fixture.StartedAt, fixture.StartedAt, &fixture.StartedAt
			if err := db.InsertSessions(t.Context(), []ingest.StoreEntry{{Metadata: &meta}}); err != nil {
				t.Fatal(err)
			}
			if err := db.IndexSessionEntries(t.Context(), sid, []schema.SessionEntry{{SessionID: sid, Harness: ingest.HarnessClaudeCode, EntryIndex: 0, EntryType: schema.EntryTypeText, Role: schema.RoleUser}}); err != nil {
				t.Fatal(err)
			}
			version := metrics.CurrentComputeVersion + row.VersionDelta
			if err := db.SaveMetrics(t.Context(), &ingest.SessionMetrics{SessionID: sid, QualityMetrics: schema.QualityMetrics{TurnCount: &fixture.PriorTurns, ComputeVersion: &version}}); err != nil {
				t.Fatal(err)
			}
			before, err := db.GetMetrics(t.Context(), sid)
			if err != nil {
				t.Fatal(err)
			}
			engine := metrics.NewEngine(&recomputeSaveStore{Store: db, fail: row.FailSave})
			recompute, ok := any(engine).(ingest.MetricsRecomputer)
			if !ok {
				t.Fatal("production metrics engine cannot refresh successful current-run index work without clearing prior state")
			}
			count, err := recompute.RecomputeMetrics(t.Context(), []ingest.SessionID{sid})
			if count != row.WantComputed || (err != nil) != row.WantError {
				t.Fatalf("recompute count=%d error=%v", count, err)
			}
			after, err := db.GetMetrics(t.Context(), sid)
			if err != nil || after == nil || after.TurnCount == nil || *after.TurnCount != row.WantTurns {
				t.Fatalf("unexpected recomputed metrics: %+v %v", after, err)
			}
			if row.WantComputed == 0 && !reflect.DeepEqual(before, after) {
				t.Fatal("skipped/failed recomputation changed last-good values or producer stamp")
			}
			if row.WantComputed > 0 && (after.ComputeVersion == nil || *after.ComputeVersion != metrics.CurrentComputeVersion || after.ComputedAt == nil) {
				t.Fatal("successful computation omitted actual producer evidence")
			}
		})
	}
	for _, name := range required {
		if !seen[name] {
			t.Fatalf("missing metric recompute fixture %q", name)
		}
	}
}
