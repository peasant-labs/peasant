package store_test

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"io"
	"reflect"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/metrics"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/artifact_seeds.yaml
var artifactSeedsYAML []byte

type artifactSeedFixtures struct {
	RequiredNames    []string            `yaml:"requiredNames"`
	SessionID        string              `yaml:"sessionID"`
	ProjectHash      string              `yaml:"projectHash"`
	HostSlug         string              `yaml:"hostSlug"`
	StartedAt        int64               `yaml:"startedAt"`
	OriginalStats    schema.SessionStats `yaml:"originalStats"`
	ReplacementStats schema.SessionStats `yaml:"replacementStats"`
	Cases            []struct {
		Name           string `yaml:"name"`
		Computed       bool   `yaml:"computed"`
		AdapterVersion *int   `yaml:"adapterVersion"`
	} `yaml:"cases"`
}

func TestMetricsUsesRetainedSeedInsteadOfPriorComputedTokens(t *testing.T) {
	t.Parallel()
	fixture := loadArtifactSeedFixtures(t)
	db := openTestStore(t)
	entry := makeStoreEntry(t, fixture.SessionID, fixture.ProjectHash, fixture.HostSlug, defaults.HarnessClaudeCode, fixture.StartedAt, 0, 0)
	entry.Metadata.Stats = fixture.ReplacementStats
	if err := db.InsertSessions(t.Context(), []ingest.StoreEntry{entry}); err != nil {
		t.Fatal(err)
	}
	sid := entry.Metadata.SessionID
	if err := db.IndexSessionEntries(t.Context(), sid, []schema.SessionEntry{{SessionID: sid, Harness: defaults.HarnessClaudeCode, EntryIndex: 0, EntryType: schema.EntryTypeText, Role: schema.RoleUser}}); err != nil {
		t.Fatal(err)
	}
	prior := &ingest.SessionMetrics{SessionID: sid, QualityMetrics: schema.QualityMetrics{InputTokens: &fixture.OriginalStats.TokensIn, OutputTokens: &fixture.OriginalStats.TokensOut, ComputeVersion: intPtr(7)}}
	if err := db.SaveMetrics(t.Context(), prior); err != nil {
		t.Fatal(err)
	}
	engine := metrics.NewEngine(db)
	engine.SetForce(true)
	if count, err := engine.ComputeMetrics(t.Context(), []ingest.SessionID{sid}); err != nil || count != 1 {
		t.Fatalf("compute from retained seed: count=%d err=%v", count, err)
	}
	computed, err := db.GetMetrics(t.Context(), sid)
	if err != nil || computed == nil || computed.InputTokens == nil || *computed.InputTokens != fixture.ReplacementStats.TokensIn || computed.OutputTokens == nil || *computed.OutputTokens != fixture.ReplacementStats.TokensOut {
		t.Fatalf("computation reused prior computed token values as native evidence: %+v %v", computed, err)
	}
	conn := takeConn(t, db.Pool())
	err = sqlitex.ExecuteTransient(conn, "UPDATE sessions SET metric_seed_json = NULL WHERE session_id = ?", &sqlitex.ExecOptions{Args: []any{string(sid)}})
	db.Pool().Put(conn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.ComputeMetrics(t.Context(), []ingest.SessionID{sid}); err != nil {
		t.Fatal(err)
	}
	unknown, err := db.GetMetrics(t.Context(), sid)
	if err != nil || unknown == nil || unknown.InputTokens != nil || unknown.OutputTokens != nil {
		t.Fatalf("missing retained seed became guessed native tokens: %+v %v", unknown, err)
	}
}

func loadArtifactSeedFixtures(t *testing.T) artifactSeedFixtures {
	t.Helper()
	var fixture artifactSeedFixtures
	decoder := yaml.NewDecoder(bytes.NewReader(artifactSeedsYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatal("artifact seed fixture must contain exactly one YAML document")
	}
	required := []string{"computed-metrics-survive", "uncomputed-seeds-stay-uncomputed", "unknown-adapter-stays-unknown"}
	if !reflect.DeepEqual(required, fixture.RequiredNames) {
		t.Fatal("required artifact seed fixture manifest changed")
	}
	seen := make(map[string]bool)
	for _, row := range fixture.Cases {
		if row.Name == "" || seen[row.Name] {
			t.Fatalf("invalid artifact seed fixture %q", row.Name)
		}
		seen[row.Name] = true
	}
	for _, name := range required {
		if !seen[name] {
			t.Fatalf("missing artifact seed fixture %q", name)
		}
	}
	return fixture
}

func TestMetadataUpsertPreservesMetricsAndRetainsIndependentSeeds(t *testing.T) {
	t.Parallel()
	fixture := loadArtifactSeedFixtures(t)
	for _, row := range fixture.Cases {
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			db := openTestStore(t)
			entry := makeStoreEntry(t, fixture.SessionID, fixture.ProjectHash, fixture.HostSlug, defaults.HarnessClaudeCode, fixture.StartedAt, 0, 0)
			entry.Metadata.Stats = fixture.OriginalStats
			if err := db.InsertSessions(t.Context(), []ingest.StoreEntry{entry}); err != nil {
				t.Fatal(err)
			}
			if row.Computed {
				computed := &ingest.SessionMetrics{SessionID: entry.Metadata.SessionID, QualityMetrics: schema.QualityMetrics{
					InputTokens: intPtr(fixture.OriginalStats.TokensIn), OutputTokens: intPtr(fixture.OriginalStats.TokensOut),
					TurnCount: intPtr(fixture.OriginalStats.TurnCount), ComputeVersion: intPtr(7), ComputedAt: &fixture.StartedAt,
				}}
				if err := db.SaveMetrics(t.Context(), computed); err != nil {
					t.Fatal(err)
				}
			}
			before, err := db.GetMetrics(t.Context(), entry.Metadata.SessionID)
			if err != nil || before == nil {
				t.Fatalf("read prior metrics: %+v %v", before, err)
			}
			entry.Metadata.Stats = fixture.ReplacementStats
			entry.Metadata.AdapterVersion = row.AdapterVersion
			if err := db.InsertSessions(t.Context(), []ingest.StoreEntry{entry}); err != nil {
				t.Fatal(err)
			}
			after, err := db.GetMetrics(t.Context(), entry.Metadata.SessionID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Errorf("metadata replacement changed prior metrics: before=%+v after=%+v err=%v", before, after, err)
			}
			conn := takeConn(t, db.Pool())
			defer db.Pool().Put(conn)
			var seed schema.SessionStats
			var adapterVersion *int
			err = sqlitex.ExecuteTransient(conn, "SELECT adapter_version, metric_seed_json FROM sessions WHERE session_id = ?", &sqlitex.ExecOptions{
				Args: []any{fixture.SessionID}, ResultFunc: func(stmt *sqlite.Stmt) error {
					if stmt.ColumnType(0) != sqlite.TypeNull {
						value := stmt.ColumnInt(0)
						adapterVersion = &value
					}
					return json.Unmarshal([]byte(stmt.ColumnText(1)), &seed)
				},
			})
			if err != nil || !reflect.DeepEqual(seed, fixture.ReplacementStats) || !reflect.DeepEqual(adapterVersion, row.AdapterVersion) {
				t.Fatalf("retained seed/producer mismatch: seed=%+v adapter=%v err=%v", seed, adapterVersion, err)
			}
		})
	}
}
