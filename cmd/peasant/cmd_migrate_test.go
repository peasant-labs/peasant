package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
	"github.com/spf13/cobra"
)

// TestBuildMigrateCommandFlags proves the command carries the
// reclaim-mirror flag set from the design (§7.2): --dry-run, --limit,
// --confirm, --json.
func TestBuildMigrateCommandFlags(t *testing.T) {
	cmd := BuildMigrateCommand()
	for _, name := range []string{"dry-run", "limit", "confirm", defaults.JSONFlagName} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("migrate is missing --%s", name)
		}
	}
	if got := cmd.Flags().Lookup("limit").DefValue; got != "0" {
		t.Errorf("--limit default = %q, want 0 (all)", got)
	}
}

// TestBuildMigrateCommandRegistered proves `peasant migrate` resolves from the
// production root command tree.
func TestBuildMigrateCommandRegistered(t *testing.T) {
	root := buildRootCommand()
	found := false
	for _, sub := range root.Commands() {
		if sub.Name() == "migrate" {
			found = true
		}
	}
	if !found {
		t.Fatal("`peasant migrate` is not registered on the root command")
	}
}

// TestRunVerifyRepairRequiresContent proves --repair without --content names
// the fix instead of running a schema check the caller did not ask for.
func TestRunVerifyRepairRequiresContent(t *testing.T) {
	err := runVerify(&cobra.Command{}, false, false, true)
	if err == nil || !strings.Contains(err.Error(), "--repair requires --content") {
		t.Fatalf("runVerify(repair without content) = %v, want the --repair refusal", err)
	}
}

// TestVerifyCommandFlags proves the additive flags exist on the production
// verify subcommand without changing the existing --verbose flag, and that
// the help no longer calls the content check unimplemented.
func TestVerifyCommandFlags(t *testing.T) {
	root := buildRootCommand()
	harvest, _, err := root.Find([]string{"harvest", "verify"})
	if err != nil || harvest == nil || harvest.Name() != "verify" {
		t.Fatalf("cannot resolve `peasant harvest verify`: %v", err)
	}
	for _, name := range []string{"verbose", "content", "repair"} {
		if harvest.Flags().Lookup(name) == nil {
			t.Errorf("harvest verify is missing --%s", name)
		}
	}
	if strings.Contains(harvest.Flags().Lookup("content").Usage, "not yet implemented") {
		t.Error("harvest verify --content help still calls the check unimplemented")
	}
}

// migrateTestSessionID builds one fixed session identity for the migrate
// command shape tests.
func migrateTestSessionID(t *testing.T) schema.SessionID {
	t.Helper()
	sid, err := schema.NewSessionID("d2d2d2d2-d2d2-42d2-82d2-d2d2d2d2d2d2")
	if err != nil {
		t.Fatal(err)
	}
	return sid
}

// TestWriteMigratePlanJSON proves the dry-run JSON shape: the session
// list, the per-phase totals, the disk check, and the advisory.
func TestWriteMigratePlanJSON(t *testing.T) {
	sid := migrateTestSessionID(t)
	cmd := BuildMigrateCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	plan := store.MigratePlan{
		Sessions:                 []schema.SessionID{sid},
		PendingIntents:           1,
		SupersededGenerations:    2,
		NeedsSearchConsolidation: true,
		MirrorRows:               10,
		EstimatedBytes:           2048,
		DiskFreeBytes:            30 << 30,
		DiskFreeOK:               true,
		Advisory:                 "no other writer running is recommended",
	}
	if err := writeMigratePlan(cmd, plan, true); err != nil {
		t.Fatalf("writeMigratePlan(json): %v", err)
	}
	var decoded struct {
		DryRun   bool     `json:"dry_run"`
		Sessions []string `json:"sessions"`
		Totals   struct {
			Sessions            int   `json:"sessions"`
			PendingIntents      int   `json:"pending_intents"`
			Superseded          int   `json:"superseded"`
			MirrorRows          int64 `json:"mirror_rows"`
			EstimatedBytes      int64 `json:"estimated_bytes"`
			SearchConsolidation bool  `json:"search_consolidation"`
		} `json:"totals"`
		Disk struct {
			FreeBytes int64 `json:"free_bytes"`
			OK        bool  `json:"ok"`
		} `json:"disk"`
		Advisory string `json:"advisory"`
	}
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("decode migrate plan JSON: %v\n%s", err, buf.String())
	}
	if !decoded.DryRun || len(decoded.Sessions) != 1 || decoded.Sessions[0] != string(sid) {
		t.Errorf("plan sessions = %v, want [%s]", decoded.Sessions, sid)
	}
	if decoded.Totals.Sessions != 1 || decoded.Totals.PendingIntents != 1 || decoded.Totals.Superseded != 2 {
		t.Errorf("plan totals = %+v, want sessions 1, intents 1, superseded 2", decoded.Totals)
	}
	if !decoded.Disk.OK || decoded.Advisory == "" {
		t.Errorf("plan disk/advisory = %+v/%q, want ok and non-empty", decoded.Disk, decoded.Advisory)
	}
}

// TestWriteMigrateResultJSON proves the applied-pass JSON shape: the
// dispositions, the rollbacks with their dimensions, the warnings, the
// consolidation flag, and the retirement evaluation.
func TestWriteMigrateResultJSON(t *testing.T) {
	sid := migrateTestSessionID(t)
	cmd := BuildMigrateCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	result := store.MigrateResult{
		Converted:  1,
		RolledBack: 1,
		Marked:     1,
		Skipped:    2,
		BytesFreed: 4096,
		Rollbacks: []store.MigrateRollback{{
			SessionID: sid,
			Dimension: "bounded-shape",
			Reason:    "the shim shapes differently",
		}},
		Warnings:           []string{"drain session x: busy"},
		SearchConsolidated: true,
		Preconditions: []store.RetirementPreconditionStatus{{
			Name:     store.RetirementPreconditionProjectionGenerationsEmpty,
			Passed:   false,
			RowCount: 3,
			Detail:   "run `peasant migrate` with Release N, then upgrade",
		}},
		ReadyForNextRelease: false,
	}
	if err := writeMigrateResult(cmd, result, true, "/tmp/peasant.db"); err != nil {
		t.Fatalf("writeMigrateResult(json): %v", err)
	}
	var decoded struct {
		Converted  int64 `json:"converted"`
		RolledBack int64 `json:"rolled_back"`
		Marked     int64 `json:"marked"`
		Skipped    int64 `json:"skipped"`
		BytesFreed int64 `json:"bytes_freed"`
		Rollbacks  []struct {
			Session   string `json:"session"`
			Dimension string `json:"dimension"`
			Reason    string `json:"reason"`
		} `json:"rollbacks"`
		Warnings           []string `json:"warnings"`
		SearchConsolidated bool     `json:"search_consolidated"`
		Preconditions      []struct {
			Name     string `json:"name"`
			Passed   bool   `json:"passed"`
			RowCount int64  `json:"row_count"`
			Detail   string `json:"detail"`
		} `json:"preconditions"`
		ReadyForRelease bool   `json:"ready_for_release"`
		Database        string `json:"database"`
		Vacuum          string `json:"vacuum"`
	}
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("decode migrate result JSON: %v\n%s", err, buf.String())
	}
	if decoded.Converted != 1 || decoded.RolledBack != 1 || decoded.Marked != 1 || decoded.Skipped != 2 {
		t.Errorf("dispositions = %+v, want 1/1/1/2", decoded)
	}
	if len(decoded.Rollbacks) != 1 || decoded.Rollbacks[0].Dimension != "bounded-shape" {
		t.Errorf("rollbacks = %+v, want one bounded-shape entry", decoded.Rollbacks)
	}
	if len(decoded.Preconditions) != 1 || decoded.Preconditions[0].Passed || decoded.ReadyForRelease {
		t.Errorf("preconditions = %+v ready=%v, want one failing guard", decoded.Preconditions, decoded.ReadyForRelease)
	}
	if decoded.Database == "" || decoded.Vacuum == "" {
		t.Errorf("database/vacuum = %q/%q, want both set", decoded.Database, decoded.Vacuum)
	}
}

// TestWriteMigrateResultText proves the human report names the
// dispositions, the rollback reasons, and the failing preconditions.
func TestWriteMigrateResultText(t *testing.T) {
	sid := migrateTestSessionID(t)
	cmd := BuildMigrateCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	result := store.MigrateResult{
		Converted: 2,
		Rollbacks: []store.MigrateRollback{{SessionID: sid, Dimension: "capture-hash", Reason: "hash differs"}},
		Preconditions: []store.RetirementPreconditionStatus{{
			Name:     store.RetirementPreconditionSessionEntriesFTSAbsent,
			RowCount: 1,
			Detail:   "the retired search index is still present",
		}},
	}
	if err := writeMigrateResult(cmd, result, false, "/tmp/peasant.db"); err != nil {
		t.Fatalf("writeMigrateResult(text): %v", err)
	}
	text := buf.String()
	for _, want := range []string{"converted 2", string(sid), "capture-hash", "not ready for the next release", "session_entries_fts_absent"} {
		if !strings.Contains(text, want) {
			t.Errorf("migrate text report misses %q:\n%s", want, text)
		}
	}
}

// TestMigrateRateETA proves the progress math: no rate before the first
// session, a zero ETA on a finished pass, and a positive ETA mid-pass.
func TestMigrateRateETA(t *testing.T) {
	started := time.Now()
	if rate, eta := migrateRateETA(started, started, 0, 10); rate != "—" || eta != "—" {
		t.Errorf("empty pass rate/ETA = %q/%q, want em-dashes", rate, eta)
	}
	later := started.Add(10_000_000_000)
	if rate, eta := migrateRateETA(started, later, 10, 10); rate == "—" || eta != "0s" {
		t.Errorf("finished pass rate/ETA = %q/%q, want a rate and 0s", rate, eta)
	}
	if rate, eta := migrateRateETA(started, later, 5, 10); rate == "—" || eta == "—" || eta == "0s" {
		t.Errorf("mid-pass rate/ETA = %q/%q, want a rate and a positive ETA", rate, eta)
	}
}
