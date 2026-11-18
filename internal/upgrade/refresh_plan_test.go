package upgrade

import (
	"crypto/sha256"
	_ "embed"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/refresh_plan.yaml
var refreshPlanCases []byte

//go:embed testdata/refresh_plan.manifest.yaml
var refreshPlanManifest []byte

func oldRefreshStore(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "old.db")
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite, sqlite.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`CREATE TABLE sessions(session_id TEXT, model_harness TEXT, schema_version INTEGER)`,
		`INSERT INTO sessions VALUES('old-session','claude',1)`,
		`PRAGMA user_version = 9`,
	} {
		if err := sqlitex.ExecuteTransient(conn, sql, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPlanRefreshBehind(t *testing.T) {
	var fixture struct {
		Cases []struct {
			Name    string            `yaml:"name"`
			Version int               `yaml:"version"`
			Effect  store.StampEffect `yaml:"effect"`
		} `yaml:"cases"`
	}
	var manifest struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	if err := yaml.Unmarshal(refreshPlanCases, &fixture); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(refreshPlanManifest, &manifest); err != nil {
		t.Fatal(err)
	}
	path := oldRefreshStore(t)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	before := sha256.Sum256(data)
	plan, err := PlanRefresh(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(data) != before {
		t.Fatal("planning changed database")
	}
	if !plan.LowerBound || len(plan.Sessions) != 1 || plan.Sessions[0].Harness != string(ingest.HarnessClaudeCode) {
		t.Fatalf("unexpected plan: %+v", plan)
	}
	for _, effect := range []store.StampEffect{store.StampEffectHarnessRename, store.StampEffectIndexInvalidated} {
		if !slices.Contains(plan.Sessions[0].Effects, effect) {
			t.Errorf("session census omitted migration effect %s", effect)
		}
	}
	for _, field := range []string{"adapter_version", "indexed_input_hash"} {
		if !slices.Contains(plan.Unknown, RefreshUnknown{"old-session", field}) {
			t.Fatalf("missing unknown %s: %+v", field, plan.Unknown)
		}
	}
	names := []string{}
	for _, c := range fixture.Cases {
		names = append(names, c.Name)
		found := false
		for _, s := range plan.Schema.Steps {
			if s.Version == c.Version && s.Kind == store.UpgradeStepSchema && s.StampEffect == c.Effect {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: step %d effect %q missing", c.Name, c.Version, c.Effect)
		}
	}
	for _, name := range manifest.RequiredNames {
		if !slices.Contains(names, name) {
			t.Errorf("missing fixture %s", name)
		}
	}
}

func TestPlanRefreshDelegates(t *testing.T) {
	path := oldRefreshStore(t)
	rows, _, err := store.ReadRefreshEvidence(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := PlanRefresh(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	target := ingest.HarvesterVersionRegistry[ingest.HarnessClaudeCode]
	_, canExtract := ingest.DefaultAdapterRegistry[ingest.HarnessClaudeCode](nil, nil, salt.Salt{}).(ingest.TranscriptMetadataExtractor)
	want := map[ingest.RefreshReason]int{}
	for _, row := range rows {
		d := ingest.DecideRefresh(ingest.RefreshEvidence{StoredRow: true, MetadataSchemaOld: !ingest.MetadataSchemaVersionIsCurrent(row.MetadataVersion), AdapterVersion: 1, AdapterTarget: target.AdapterVersion, AdapterCanExtract: canExtract, IndexerVersion: 0, IndexerTarget: target.IndexerVersion, IndexProofPresent: false})
		want[d.Reason]++
	}
	for reason, count := range want {
		if plan.Estimate.ByReason[reason] != count {
			t.Fatalf("%s: got %d want %d", reason, plan.Estimate.ByReason[reason], count)
		}
	}
}

//go:embed testdata/refresh_harness_rename.yaml
var refreshHarnessRenameCases []byte

func TestPlanRefreshHistoricalHarnessRename(t *testing.T) {
	var fixture struct {
		Cases []struct {
			Name, Input, Output string
			Renamed, Unknown    bool
		} `yaml:"cases"`
		RequiredNames []string `yaml:"requiredNames"`
	}
	if err := yaml.Unmarshal(refreshHarnessRenameCases, &fixture); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(fixture.Cases))
	for _, c := range fixture.Cases {
		names = append(names, c.Name)
		t.Run(c.Name, func(t *testing.T) {
			path := oldRefreshStore(t)
			conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite)
			if err != nil {
				t.Fatal(err)
			}
			err = sqlitex.ExecuteTransient(conn, "UPDATE sessions SET model_harness = ?", &sqlitex.ExecOptions{Args: []any{c.Input}})
			if err != nil {
				t.Fatal(err)
			}
			if err := conn.Close(); err != nil {
				t.Fatal(err)
			}
			plan, err := PlanRefresh(t.Context(), path)
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.Sessions) != 1 {
				t.Fatalf("sessions = %+v", plan.Sessions)
			}
			session := plan.Sessions[0]
			if session.Harness != c.Output || slices.Contains(session.Effects, store.StampEffectHarnessRename) != c.Renamed {
				t.Fatalf("session = %+v", session)
			}
			unknown := slices.Contains(plan.Unknown, RefreshUnknown{"old-session", "harness"})
			if unknown != c.Unknown {
				t.Fatalf("unknown evidence = %+v", plan.Unknown)
			}
		})
	}
	for _, name := range fixture.RequiredNames {
		if !slices.Contains(names, name) {
			t.Errorf("missing fixture %s", name)
		}
	}
}
