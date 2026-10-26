package store

import (
	"bytes"
	_ "embed"
	"io"
	"path/filepath"
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
	predecessor := sqlitemigration.Schema{Migrations: dbSchema.Migrations[:68], MigrationOptions: dbSchema.MigrationOptions[:68]}
	if err := sqlitemigration.Migrate(t.Context(), conn, predecessor); err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteScript(conn, `
INSERT INTO host_slugs(opaque_id, host_slug) VALUES('cause-host','cause-host');
INSERT INTO projects(project_hash) VALUES('cause-project');
INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format)
VALUES('cause-session','claude-code','fixture-model','cause-host','cause-project',1,2,3,'/synthetic/missing.jsonl','jsonl');
INSERT INTO session_entries(session_id, entry_index, provider, entry_type, role, content_preview)
VALUES('cause-session',0,'claude-code','text','user','prior readable content');
INSERT INTO session_source_unavailability(session_id, reason)
VALUES('cause-session','original-source-unavailable-no-usable-saved-copy');
`, nil); err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteTransient(conn, `UPDATE session_source_unavailability SET reason='original-source-present-record-unusable' WHERE session_id='cause-session'`, nil); err == nil {
		t.Fatal("predecessor schema accepted the present-but-unusable cause")
	}
	if err := sqlitemigration.Migrate(t.Context(), conn, dbSchema); err != nil {
		t.Fatal(err)
	}
	found := false
	if err := sqlitex.ExecuteTransient(conn, `SELECT reason, content_preview FROM session_source_unavailability JOIN sessions USING(session_id) JOIN session_entries USING(session_id) WHERE session_id='cause-session'`, &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
		found = true
		if stmt.ColumnText(0) != string(ingest.SourceUnavailableNoSavedCopy) || stmt.ColumnText(1) != "prior readable content" {
			t.Fatalf("rebuild changed stored cause or content: reason=%q preview=%q", stmt.ColumnText(0), stmt.ColumnText(1))
		}
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("rebuild lost the recorded missing-source cause")
	}
	if err := sqlitex.ExecuteTransient(conn, `UPDATE session_source_unavailability SET reason=? WHERE session_id='cause-session'`, &sqlitex.ExecOptions{Args: []any{string(ingest.SourceUnavailableRecordUnusable)}}); err != nil {
		t.Fatalf("widened check refused the present-but-unusable cause: %v", err)
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
