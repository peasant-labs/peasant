package store

import (
	"bytes"
	"context"
	_ "embed"
	"io"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite/sqlitemigration"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/migrations/v52_legacy_capture.yaml
var migrationV52YAML []byte

type migrationV52Case struct {
	Name           string   `yaml:"name"`
	SessionID      string   `yaml:"sessionID"`
	PreviewText    string   `yaml:"previewText"`
	WantEntryCount int      `yaml:"wantEntryCount"`
	Seed           []string `yaml:"seed"`
}

type migrationV52Document struct {
	ProjectHash   string             `yaml:"projectHash"`
	HostSlug      string             `yaml:"hostSlug"`
	Seed          []string           `yaml:"seed"`
	RequiredNames []string           `yaml:"requiredNames"`
	Cases         []migrationV52Case `yaml:"cases"`
}

func loadMigrationV52Fixtures(t *testing.T) migrationV52Document {
	t.Helper()
	var document migrationV52Document
	decoder := yaml.NewDecoder(bytes.NewReader(migrationV52YAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("v52 migration fixtures require one document: %v", err)
	}
	names := make(map[string]bool)
	for _, row := range document.Cases {
		if row.Name == "" || names[row.Name] || row.SessionID == "" || len(row.Seed) == 0 {
			t.Fatalf("invalid v52 migration fixture %q", row.Name)
		}
		if (row.WantEntryCount > 0) != (row.PreviewText != "") {
			t.Fatalf("case %q must name the preview text it expects exactly when it seeds an entry", row.Name)
		}
		names[row.Name] = true
	}
	for _, name := range document.RequiredNames {
		if !names[name] {
			t.Fatalf("missing v52 migration scenario %q", name)
		}
		delete(names, name)
	}
	if len(names) > 0 {
		t.Fatalf("unmanifested v52 migration scenario: %v", names)
	}
	return document
}

// TestMigrationV52LegacyCaptureIncomplete upgrades a GENUINE V51 database.
//
// V51 had no content-capture tables at all, so every session it stored holds a
// bounded preview projection and no durable prose. The upgrade must say exactly
// that: an incomplete capture with no source authority, no fabricated digest
// and the legacy capture format, so a later reader cannot mistake an old
// preview for a whole session. The preview itself is preserved and stays
// readable, because it is real recorded content even though it is not
// complete.
//
// The predecessor state is built by migrating to the frozen 51-migration
// prefix and seeding it with the historical INSERT statements a V51 store
// actually wrote. It is deliberately NOT built by opening the current schema
// and deleting the newer objects: that leaves every later column in place, so
// the copy inside migration 53 never runs against the row shape it was written
// for, and it proves nothing about a database a person actually has.
func TestMigrationV52LegacyCaptureIncomplete(t *testing.T) {
	document := loadMigrationV52Fixtures(t)
	for _, row := range document.Cases {
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "legacy.db")
			seedFrozenSchema(t, ctx, path, 51, append(append([]string(nil), document.Seed...), row.Seed...))

			db, err := Open(path, WithPoolSize(1))
			if err != nil {
				t.Fatalf("upgrade a genuine V51 database: %v", err)
			}
			defer db.Close()
			id := ingest.SessionID(row.SessionID)

			capture, found, err := db.GetSessionContentCapture(ctx, id)
			if err != nil || !found {
				t.Fatalf("upgraded session declares no capture at all: found=%v err=%v", found, err)
			}
			if capture.Status != ingest.ContentCaptureIncomplete ||
				capture.SourceAuthority != ingest.ContentSourceNone ||
				capture.CaptureFormat != ingest.ContentCaptureFormatLegacyPreviewOnly ||
				capture.FailureCode != "legacy_preview_only" ||
				capture.FullCaptureSHA256 != "" ||
				capture.PublicationCaptureRevision != 0 ||
				capture.EntryCount != row.WantEntryCount ||
				capture.ContentRowCount != 0 {
				t.Fatalf("legacy capture was inferred as something the database cannot prove: %+v", capture)
			}
			if _, err := db.ReadSessionEntries(ctx, id, ingest.SessionEntryReadOptions{Mode: ingest.SessionEntryReadFullContent}); err == nil {
				t.Fatal("a legacy bounded preview was served as verified complete content")
			}

			entries, err := db.ListEntries(ctx, id)
			if err != nil || len(entries) != row.WantEntryCount {
				t.Fatalf("upgraded projection=%d entries, want %d: %v", len(entries), row.WantEntryCount, err)
			}
			if row.WantEntryCount == 0 {
				return
			}
			if entries[0].ContentPreview == nil || *entries[0].ContentPreview != row.PreviewText {
				t.Fatalf("the upgrade changed the recorded preview: %+v", entries[0].ContentPreview)
			}
			// The preview is not complete, and it is still content a person
			// recorded, so every mounted previewer must be able to show it.
			available, err := db.ReadSessionAvailable(ctx, row.SessionID)
			if err != nil || available == nil || len(available.Entries) != row.WantEntryCount {
				t.Fatalf("a legacy session became unpreviewable after the upgrade: %+v %v", available, err)
			}
		})
	}
}

// seedFrozenSchema builds the database exactly as it shipped after n
// migrations, runs the supplied historical statements against it, and closes
// it, so the caller can then open it through the ordinary production path and
// let the remaining migrations run.
func seedFrozenSchema(t *testing.T, ctx context.Context, path string, n int, statements []string) {
	t.Helper()
	pool, err := sqlitex.NewPool(path, sqlitex.PoolOptions{PoolSize: 1, PrepareConn: preparePragmas})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	conn, err := pool.Take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Put(conn)
	if err := sqlitemigration.Migrate(ctx, conn, frozenSchema(n)); err != nil {
		t.Fatalf("build the frozen %d-migration schema: %v", n, err)
	}
	for _, statement := range statements {
		if err := sqlitex.ExecuteTransient(conn, statement, nil); err != nil {
			t.Fatalf("seed historical row %q: %v", statement, err)
		}
	}
}
