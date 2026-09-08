package store

import (
	"bytes"
	"context"
	_ "embed"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite/sqlitemigration"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/migrations/v50_pi_harness.yaml
var migrationPiYAML []byte

//go:embed testdata/migrations/v50_pi_harness.manifest.yaml
var migrationPiManifest []byte

func TestMigrationV53PiPreservesCurrentStore(t *testing.T) {
	var f struct {
		Cases []struct {
			Name                string   `yaml:"name"`
			Seed                []string `yaml:"seed"`
			FullSession         string   `yaml:"fullSession"`
			FullPrefix          string   `yaml:"fullPrefix"`
			FullRepetitions     int      `yaml:"fullRepetitions"`
			FullTail            string   `yaml:"fullTail"`
			PublicationRevision int64    `yaml:"publicationRevision"`
			FullParent          string   `yaml:"fullParent"`
			FullProject         string   `yaml:"fullProject"`
			FullHost            string   `yaml:"fullHost"`
			FullCWD             string   `yaml:"fullCwd"`
			TriggerModel        string   `yaml:"triggerModel"`
			Assertions          []struct {
				Query string `yaml:"query"`
				Want  string `yaml:"want"`
			} `yaml:"assertions"`
			Rejects []string `yaml:"rejects"`
		} `yaml:"cases"`
	}
	decode := func(raw []byte, dest any) {
		t.Helper()
		d := yaml.NewDecoder(bytes.NewReader(raw))
		d.KnownFields(true)
		if err := d.Decode(dest); err != nil {
			t.Fatal(err)
		}
		var trailing any
		if err := d.Decode(&trailing); err != io.EOF {
			t.Fatalf("trailing YAML: %v", err)
		}
	}
	decode(migrationPiYAML, &f)
	var manifest struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	decode(migrationPiManifest, &manifest)
	names := make(map[string]bool)
	for _, c := range f.Cases {
		if c.Name == "" || names[c.Name] {
			t.Fatal("empty or duplicate migration scenario name")
		}
		names[c.Name] = true
	}
	for _, name := range manifest.RequiredNames {
		if !names[name] {
			t.Fatalf("missing migration scenario %q", name)
		}
		delete(names, name)
	}
	if len(names) > 0 {
		t.Fatal("unmanifested migration scenario")
	}
	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			ctx := context.Background()
			dbPath := filepath.Join(t.TempDir(), "upgrade.db")
			pool, err := sqlitex.NewPool(dbPath, sqlitex.PoolOptions{PoolSize: 1, PrepareConn: preparePragmas})
			if err != nil {
				t.Fatal(err)
			}
			conn, err := pool.Take(ctx)
			if err != nil {
				pool.Close()
				t.Fatal(err)
			}
			before := sqlitemigration.Schema{Migrations: dbSchema.Migrations[:52], MigrationOptions: dbSchema.MigrationOptions[:52]}
			if err := sqlitemigration.Migrate(ctx, conn, before); err != nil {
				pool.Put(conn)
				pool.Close()
				t.Fatal(err)
			}
			for _, query := range c.Seed {
				if err := sqlitex.ExecuteTransient(conn, query, nil); err != nil {
					pool.Put(conn)
					pool.Close()
					t.Fatalf("seed: %v", err)
				}
			}
			sid, err := ingest.NewSessionID(c.FullSession)
			if err != nil || c.FullRepetitions <= 0 || c.FullPrefix == "" || c.FullTail == "" || c.PublicationRevision <= 0 || c.FullParent == "" || c.FullProject == "" || c.FullHost == "" || c.FullCWD == "" || c.TriggerModel == "" {
				t.Fatalf("invalid full-content migration fixture: %v", err)
			}
			oldDB, err := Open(dbPath, WithSkipMigrations(), WithPoolSize(1))
			if err != nil {
				t.Fatal(err)
			}
			text := strings.Repeat(c.FullPrefix, c.FullRepetitions) + c.FullTail
			publication := makeStoreEntry(t, c.FullSession, c.FullProject, c.FullHost, defaults.HarnessClaudeCode, 1, 0, 0)
			publication.PublicationCapture = true
			publication.CWDProvenance = ingest.CWDSourceExact
			publication.Session.Origin = sessionorigin.User
			publication.Metadata.CWD = c.FullCWD
			parentID, parentErr := ingest.NewSessionID(c.FullParent)
			if parentErr != nil {
				t.Fatal(parentErr)
			}
			publication.Metadata.ParentUUID = &parentID
			publication.Metadata.ContentHash = schema.ComputeTranscriptHash([]byte(text))
			publication.Metadata.MetadataHash = schema.ComputeMetadataHash(publication.Metadata)
			revisions, err := oldDB.InsertSessionsWithRevisions(ctx, []ingest.StoreEntry{publication})
			if err != nil {
				t.Fatalf("seed publication capture through production store: %v", err)
			}
			revision := revisions[sid]
			if revision != c.PublicationRevision {
				t.Fatalf("publication revision = %d want %d", revision, c.PublicationRevision)
			}
			entries := []schema.SessionEntry{{SessionID: sid, EntryIndex: 0, Harness: schema.HarnessClaudeCode, Role: schema.RoleUser, EntryType: schema.EntryTypeText, ContentPreview: &text}}
			written := oldDB.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{SessionID: sid, Entries: entries, RequireFullContent: true, CaptureRevision: revision, IndexVersion: ingest.CurrentIndexVersion, IndexedAtMs: 4}})
			if len(written) != 1 || written[0].Err != nil {
				t.Fatalf("seed full content through production writer: %+v", written)
			}
			beforeEntries, beforeCapture, err := oldDB.LoadFullSessionEntries(ctx, sid, 0)
			if err != nil {
				t.Fatal(err)
			}
			if err := oldDB.Close(); err != nil {
				t.Fatal(err)
			}
			if err := sqlitemigration.Migrate(ctx, conn, dbSchema); err != nil {
				pool.Put(conn)
				pool.Close()
				t.Fatal(err)
			}
			pool.Put(conn)
			pool.Close()

			pool, err = sqlitex.NewPool(dbPath, sqlitex.PoolOptions{PoolSize: 1, PrepareConn: preparePragmas})
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			conn, err = pool.Take(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Put(conn)
			newDB, err := Open(dbPath, WithPoolSize(1))
			if err != nil {
				t.Fatal(err)
			}
			afterEntries, afterCapture, err := newDB.LoadFullSessionEntries(ctx, sid, 0)
			if err != nil || !reflect.DeepEqual(beforeEntries, afterEntries) || !reflect.DeepEqual(beforeCapture, afterCapture) {
				t.Fatalf("verified full capture changed across migration/reopen: %v", err)
			}
			bundle, err := newDB.LoadPublicationInput(ctx, sid)
			if err != nil || bundle.Readiness != ingest.PublicationReady || bundle.CaptureRevision != c.PublicationRevision || !reflect.DeepEqual(bundle.Metadata, *publication.Metadata) || bundle.ContentCapture.PublicationCaptureRevision != c.PublicationRevision {
				t.Fatalf("publication proof changed across migration/reopen: readiness=%s revision=%d metadataMatch=%t contentRevision=%d err=%v", bundle.Readiness, bundle.CaptureRevision, reflect.DeepEqual(bundle.Metadata, *publication.Metadata), bundle.ContentCapture.PublicationCaptureRevision, err)
			}
			for _, a := range c.Assertions {
				if got := scalarText(t, conn, a.Query); got != a.Want {
					t.Fatalf("query %s = %q want %q", a.Query, got, a.Want)
				}
			}
			if err := sqlitex.ExecuteTransient(conn, `UPDATE sessions SET model_id=? WHERE session_id=?`, &sqlitex.ExecOptions{Args: []any{c.TriggerModel, string(sid)}}); err != nil {
				t.Fatalf("change trigger-watched session fact: %v", err)
			}
			invalidated, err := newDB.LoadPublicationInput(ctx, sid)
			if err != nil || invalidated.Readiness != ingest.PublicationNeedsIngest || len(invalidated.Entries) != 0 {
				t.Fatalf("recreated trigger did not invalidate publication loader state: readiness=%s entries=%d err=%v", invalidated.Readiness, len(invalidated.Entries), err)
			}
			// Exact canonical membership is schema-owned, not a copied harness count.
			for _, h := range schema.Harnesses() {
				if err := sqlitex.ExecuteTransient(conn, `UPDATE sessions SET model_harness=? WHERE session_id=?`, &sqlitex.ExecOptions{Args: []any{string(h), c.FullParent}}); err != nil {
					t.Fatalf("sessions rejected %s: %v", h, err)
				}
				if err := sqlitex.ExecuteTransient(conn, `INSERT INTO daily_summary_harness(date_utc,model_harness) VALUES ('2026-01-02',?)`, &sqlitex.ExecOptions{Args: []any{string(h)}}); err != nil {
					t.Fatalf("daily summary rejected %s: %v", h, err)
				}
			}
			for _, query := range c.Rejects {
				if err := sqlitex.ExecuteTransient(conn, query, nil); err == nil {
					t.Fatalf("closed constraint accepted %s", query)
				}
			}
			if err := newDB.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
