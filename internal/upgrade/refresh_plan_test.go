package upgrade

import (
	"crypto/sha256"
	_ "embed"
	"fmt"
	"maps"
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
	if !plan.Approximate || !slices.Contains(plan.EvidenceNotRead, "content_pending") ||
		!slices.Contains(plan.UnappliedEffects, store.StampEffectSearchInvalidated) ||
		!slices.Contains(plan.UnappliedEffects, store.StampEffectRecountOwed) {
		t.Fatalf("missing census limitations: %+v", plan)
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

func TestPlanRefreshCurrentStoreIsApproximate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "current.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanRefresh(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if plan.LowerBound || !plan.Approximate || !slices.Contains(plan.EvidenceNotRead, "latest_attempt") {
		t.Fatalf("current store limitations: %+v", plan)
	}
}

func TestPlanRefreshUsesManagedProducerTarget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "current.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	baseline := ingest.HarvesterVersionRegistry[ingest.HarnessCodex]
	err = sqlitex.ExecuteTransient(conn, `INSERT INTO sessions
		(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format, schema_version, adapter_version, index_version, indexed_input_hash)
		VALUES ('managed-target', ?, 'model', 'host', 'project', 1, 2, 3, '/synthetic/source', 'jsonl', ?, ?, ?, ?)`,
		&sqlitex.ExecOptions{Args: []any{string(ingest.HarnessCodex), int(ingest.CurrentSchemaVersion), baseline.AdapterVersion, baseline.IndexerVersion, "0000000000000000000000000000000000000000000000000000000000000000"}})
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
	if len(plan.Sessions) != 1 || plan.Sessions[0].Decision.Reason != ingest.RefreshReasonAdapterBehind {
		t.Fatalf("managed adapter target not used: %+v", plan)
	}
}

//go:embed testdata/refresh_delegation.yaml
var refreshDelegationCases []byte

func TestPlanRefreshDelegates(t *testing.T) {
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name          string
			AdapterOffset int `yaml:"adapterOffset"`
			IndexerOffset int `yaml:"indexerOffset"`
			Proof         bool
			MetadataOld   bool `yaml:"metadataOld"`
			Reason        ingest.RefreshReason
		} `yaml:"cases"`
	}
	if err := yaml.Unmarshal(refreshDelegationCases, &fixture); err != nil {
		t.Fatal(err)
	}
	target := ingest.GenerationRepairTargets[ingest.HarnessClaudeCode]
	_, canExtract := ingest.DefaultAdapterRegistry[ingest.HarnessClaudeCode](nil, nil, salt.Salt{}).(ingest.TranscriptMetadataExtractor)
	var names []string
	var seeds []refreshSeed
	for _, c := range fixture.Cases {
		names = append(names, c.Name)
		seeds = append(seeds, refreshSeed{ID: c.Name, Harness: ingest.HarnessClaudeCode, AdapterOffset: offset(c.AdapterOffset), IndexerOffset: c.IndexerOffset, MetadataOld: c.MetadataOld, Proof: c.Proof})
	}
	for _, name := range fixture.RequiredNames {
		if !slices.Contains(names, name) {
			t.Errorf("missing fixture %s", name)
		}
	}
	path := currentRefreshStore(t, seeds...)
	rows, _, err := store.ReadRefreshEvidence(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := PlanRefresh(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	reasons := map[ingest.RefreshReason]int{}
	lanes := map[ingest.RefreshLane]int{}
	decisions := map[string]ingest.RefreshDecision{}
	for _, row := range rows {
		adapter := 1
		if row.AdapterVersion != nil {
			adapter = *row.AdapterVersion
		}
		d := ingest.DecideRefresh(ingest.RefreshEvidence{StoredRow: true, MetadataSchemaOld: ingest.MetadataNeedsNativeRefresh(row.MetadataVersion), AdapterVersion: adapter, AdapterStampPresent: row.AdapterVersion != nil, AdapterTarget: target.AdapterVersion, AdapterCanExtract: canExtract, IndexerVersion: row.IndexerVersion, IndexerTarget: target.IndexerVersion, IndexProofPresent: row.IndexedInputHash != nil})
		decisions[row.SessionID] = d
		reasons[d.Reason]++
		lanes[d.Lane]++
	}
	if !maps.Equal(plan.Estimate.ByReason, reasons) || !maps.Equal(plan.Estimate.ByLane, lanes) {
		t.Fatalf("census = %+v, want reasons %v lanes %v", plan.Estimate, reasons, lanes)
	}
	if len(plan.Sessions) != len(decisions) {
		t.Fatalf("sessions = %+v, want %v", plan.Sessions, decisions)
	}
	for _, session := range plan.Sessions {
		if !session.Decided || session.Decision != decisions[session.SessionID] {
			t.Errorf("session = %+v want %+v", session, decisions[session.SessionID])
		}
	}
	for _, c := range fixture.Cases {
		if decisions[c.Name].Reason != c.Reason {
			t.Errorf("%s reason = %s, want %s", c.Name, decisions[c.Name].Reason, c.Reason)
		}
	}
	// Migration 77's recount does not feed decideRefresh; migration 62's
	// content-pending work is not evidence this planner reads.
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteTransient(conn, "PRAGMA user_version = 56", nil); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	behind, err := PlanRefresh(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(behind.UnappliedEffects, store.StampEffectSearchInvalidated) || !slices.Contains(behind.UnappliedEffects, store.StampEffectRecountOwed) {
		t.Fatalf("unapplied effects = %v", behind.UnappliedEffects)
	}
	if len(behind.Sessions) != len(decisions) {
		t.Fatalf("behind sessions = %+v", behind.Sessions)
	}
	for _, session := range behind.Sessions {
		if !session.Decided || session.Decision != decisions[session.SessionID] {
			t.Errorf("pending effects changed %s: %+v, want %+v", session.SessionID, session.Decision, decisions[session.SessionID])
		}
	}
}

func TestPlanRefreshVersionBoundaries(t *testing.T) {
	const boundaryFixturePath = "../store/testdata/refresh_boundaries.yaml"
	data, err := os.ReadFile(boundaryFixturePath)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name             string
			Version, Indexer int
			Adapter, Proof   bool
		} `yaml:"cases"`
	}
	if err := yaml.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range fixture.Cases {
		names = append(names, c.Name)
		t.Run(c.Name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "behind.db")
			conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite, sqlite.OpenCreate)
			if err != nil {
				t.Fatal(err)
			}
			columns := "session_id TEXT, model_harness TEXT, schema_version INTEGER"
			values := "'session', 'claude-code', 1"
			if c.Version >= 10 {
				columns += ", index_version INTEGER"
				values += ", 7"
			}
			if c.Version >= 55 {
				columns += ", adapter_version INTEGER"
				values += ", 8"
			}
			if c.Version >= 56 {
				columns += ", indexed_input_hash TEXT"
				values += ", 'proof'"
			}
			for _, sql := range []string{"CREATE TABLE sessions(" + columns + ")", "INSERT INTO sessions VALUES(" + values + ")", fmt.Sprintf("PRAGMA user_version = %d", c.Version)} {
				if err := sqlitex.ExecuteTransient(conn, sql, nil); err != nil {
					t.Fatal(err)
				}
			}
			if err := conn.Close(); err != nil {
				t.Fatal(err)
			}
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
			if slices.Contains(plan.Unknown, RefreshUnknown{"session", "adapter_version"}) == c.Adapter || slices.Contains(plan.Unknown, RefreshUnknown{"session", "indexed_input_hash"}) == c.Proof || len(plan.Unknown) != boolCount(!c.Adapter)+boolCount(!c.Proof) {
				t.Fatalf("unknown = %+v", plan.Unknown)
			}
		})
	}
	for _, name := range fixture.RequiredNames {
		if !slices.Contains(names, name) {
			t.Errorf("missing fixture %s", name)
		}
	}
}

func boolCount(b bool) int {
	if b {
		return 1
	}
	return 0
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
			if c.Unknown {
				if session.Decided || plan.Estimate.Undecided != 1 || len(plan.Estimate.ByLane) != 0 || len(plan.Estimate.ByReason) != 0 {
					t.Fatalf("unknown harness was decided: %+v", plan)
				}
			} else if !session.Decided || plan.Estimate.Undecided != 0 {
				t.Fatalf("known harness was not decided: %+v", plan)
			}
		})
	}
	for _, name := range fixture.RequiredNames {
		if !slices.Contains(names, name) {
			t.Errorf("missing fixture %s", name)
		}
	}
}
