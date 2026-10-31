package store

import (
	"bytes"
	_ "embed"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitemigration"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/source_unavailability_upsert.yaml
var sourceUnavailabilityUpsertYAML []byte

type sourceUnavailabilityUpsertStep struct {
	Name        string `yaml:"name"`
	Reason      string `yaml:"reason"`
	WantChanged bool   `yaml:"wantChanged"`
}

func loadSourceUnavailabilityUpsertSteps(t *testing.T) []sourceUnavailabilityUpsertStep {
	t.Helper()
	var doc struct {
		RequiredNames []string                         `yaml:"requiredNames"`
		Steps         []sourceUnavailabilityUpsertStep `yaml:"steps"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(sourceUnavailabilityUpsertYAML))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		t.Fatalf("source unavailability upsert fixture must be one document: %v", err)
	}
	if len(doc.RequiredNames) == 0 {
		t.Fatal("source unavailability upsert fixture declares no required step names")
	}
	// This white-box store test cannot import internal/testutil, which imports
	// this package, so the blank and duplicate guards live here rather than
	// reusing RequireFixtureNames.
	seenRequired := make(map[string]bool, len(doc.RequiredNames))
	for _, name := range doc.RequiredNames {
		if strings.TrimSpace(name) == "" {
			t.Fatal("source unavailability upsert fixture declares a blank required step name")
		}
		if seenRequired[name] {
			t.Fatalf("source unavailability upsert fixture repeats required step name %q", name)
		}
		seenRequired[name] = true
	}
	names := make(map[string]bool, len(doc.Steps))
	for _, step := range doc.Steps {
		if step.Name == "" || names[step.Name] {
			t.Fatalf("invalid source unavailability upsert step %q", step.Name)
		}
		names[step.Name] = true
	}
	for _, name := range doc.RequiredNames {
		if !names[name] {
			t.Fatalf("missing source unavailability upsert step %q", name)
		}
	}
	return doc.Steps
}

func TestMigrationV69WidensSourceUnavailabilityCause(t *testing.T) {
	t.Parallel()
	conn, err := sqlite.OpenConn(filepath.Join(t.TempDir(), "upgrade.db"), sqlite.OpenReadWrite, sqlite.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := sqlitemigration.Migrate(t.Context(), conn, frozenSchema(68)); err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteScript(conn, `
INSERT INTO host_slugs(opaque_id, host_slug) VALUES('cause-host','cause-host');
INSERT INTO projects(project_hash) VALUES('cause-project');
INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format)
VALUES('cause-session','claude-code','fixture-model','cause-host','cause-project',1,2,3,'/synthetic/missing.jsonl','jsonl');
INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format)
VALUES('unusable-session','claude-code','fixture-model','cause-host','cause-project',1,2,3,'/synthetic/unusable.jsonl','jsonl');
INSERT INTO session_source_unavailability(session_id, reason)
VALUES('cause-session','original-source-unavailable-no-usable-saved-copy');
`, nil); err != nil {
		t.Fatal(err)
	}
	// A second stored row carries the other cause, so the rebuild has to copy
	// each row's own value: a copy step that writes one fixed cause fails on the
	// row holding the other. The predecessor CHECK this migration widens refuses
	// that cause, so check enforcement is off for this one insert and restored
	// before the refusal below.
	if err := sqlitex.ExecuteTransient(conn, `PRAGMA ignore_check_constraints = ON`, nil); err != nil {
		t.Fatalf("relax the predecessor check to seed the second cause: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn, `
INSERT INTO session_source_unavailability(session_id, reason) VALUES('unusable-session', ?)`,
		&sqlitex.ExecOptions{Args: []any{string(ingest.SourceUnavailableRecordUnusable)}}); err != nil {
		t.Fatalf("seed the second stored cause: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn, `PRAGMA ignore_check_constraints = OFF`, nil); err != nil {
		t.Fatalf("restore the predecessor check after seeding: %v", err)
	}
	// The new claim: the shipped table refuses the cause this migration adds, so
	// the widening is what a stored present-but-unusable source needs.
	if err := sqlitex.ExecuteTransient(conn, `UPDATE session_source_unavailability SET reason='original-source-present-record-unusable' WHERE session_id='cause-session'`, nil); err == nil {
		t.Fatal("predecessor schema accepted the present-but-unusable cause")
	}
	if err := sqlitemigration.Migrate(t.Context(), conn, dbSchema); err != nil {
		t.Fatal(err)
	}
	// The rebuild copies the stored rows, so each row has to survive the
	// drop-and-recreate with the cause it held. This read is taken before the
	// update below overwrites one of the causes: a copy step that wrote a fixed
	// value instead of the stored one would otherwise pass unnoticed.
	storedCause := func(session string) string {
		t.Helper()
		cause := ""
		if err := sqlitex.ExecuteTransient(conn, `SELECT reason FROM session_source_unavailability WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{session}, ResultFunc: func(stmt *sqlite.Stmt) error {
			cause = stmt.ColumnText(0)
			return nil
		}}); err != nil {
			t.Fatalf("read the cause the rebuild copied for %s: %v", session, err)
		}
		return cause
	}
	if got, want := storedCause("cause-session"), string(ingest.SourceUnavailableNoSavedCopy); got != want {
		t.Fatalf("the rebuild kept cause %q for cause-session, want the seeded %q", got, want)
	}
	if got, want := storedCause("unusable-session"), string(ingest.SourceUnavailableRecordUnusable); got != want {
		t.Fatalf("the rebuild kept cause %q for unusable-session, want the seeded %q", got, want)
	}
	if err := sqlitex.ExecuteTransient(conn, `UPDATE session_source_unavailability SET reason=? WHERE session_id='cause-session'`, &sqlitex.ExecOptions{Args: []any{string(ingest.SourceUnavailableRecordUnusable)}}); err != nil {
		t.Fatalf("widened check refused the present-but-unusable cause: %v", err)
	}
	// The changed-row count is read immediately: another statement can reset the
	// connection's last-change count, so a later read would not be about this
	// update. A rebuild that dropped the row leaves this update matching
	// nothing and reporting no error.
	updated := conn.Changes()
	if updated != 1 {
		t.Fatalf("the cause update changed %d rows, want 1: the row did not survive the rebuild", updated)
	}
	if got, want := storedCause("cause-session"), string(ingest.SourceUnavailableRecordUnusable); got != want {
		t.Fatalf("stored cause after the rebuild = %q, want %q", got, want)
	}
}

func TestRecordSourceUnavailableReplacesChangedCause(t *testing.T) {
	t.Parallel()
	db, err := Open(filepath.Join(t.TempDir(), "upsert.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn, err := db.pool.Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteScript(conn, `
INSERT INTO host_slugs(opaque_id, host_slug) VALUES('upsert-host','upsert-host');
INSERT INTO projects(project_hash) VALUES('upsert-project');
INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format)
VALUES('11111111-1111-4111-8111-111111111111','claude-code','fixture-model','upsert-host','upsert-project',1,2,3,'/synthetic/missing.jsonl','jsonl');
`, nil); err != nil {
		db.pool.Put(conn)
		t.Fatal(err)
	}
	db.pool.Put(conn)
	sid := ingest.SessionID("11111111-1111-4111-8111-111111111111")
	for _, step := range loadSourceUnavailabilityUpsertSteps(t) {
		reason, err := ingest.NewSourceUnavailabilityReason(step.Reason)
		if err != nil {
			t.Fatalf("step %s: %v", step.Name, err)
		}
		changed, err := db.RecordSourceUnavailable(t.Context(), sid, reason)
		if err != nil {
			t.Fatalf("step %s: %v", step.Name, err)
		}
		if changed != step.WantChanged {
			t.Fatalf("step %s: changed=%t, want %t", step.Name, changed, step.WantChanged)
		}
		stored, err := db.ReadSourceUnavailability(t.Context(), sid)
		if err != nil || stored == nil || *stored != reason {
			t.Fatalf("step %s: stored %v, err %v, want %s", step.Name, stored, err, reason)
		}
	}
}
