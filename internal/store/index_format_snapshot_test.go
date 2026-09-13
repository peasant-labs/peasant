package store_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/index_format_snapshots.yaml
var indexFormatSnapshotsYAML []byte

type indexGuardScope string

const (
	indexGuardNil         indexGuardScope = "nil"
	indexGuardEmpty       indexGuardScope = "empty"
	indexGuardSupported   indexGuardScope = "supported"
	indexGuardUnsupported indexGuardScope = "unsupported"
	indexGuardAll         indexGuardScope = "all"
)

type indexFormatGuardCase struct {
	Name      string          `yaml:"name"`
	Scope     indexGuardScope `yaml:"scope"`
	Snapshot  bool            `yaml:"snapshot"`
	DenyReads bool            `yaml:"denyReads"`
	WantError string          `yaml:"wantError"`
	PrefixIDs int             `yaml:"prefixIDs"`
}
type indexFormatSnapshotCase struct {
	Name      string             `yaml:"name"`
	Operation indexReadOperation `yaml:"operation"`
}
type indexFormatSnapshotDocument struct {
	RequiredGuardNames    []string                  `yaml:"requiredGuardNames"`
	Guards                []indexFormatGuardCase    `yaml:"guards"`
	RequiredSnapshotNames []string                  `yaml:"requiredSnapshotNames"`
	Snapshots             []indexFormatSnapshotCase `yaml:"snapshots"`
}

func loadIndexFormatSnapshotFixtures(t *testing.T) indexFormatSnapshotDocument {
	t.Helper()
	var document indexFormatSnapshotDocument
	decoder := yaml.NewDecoder(bytes.NewReader(indexFormatSnapshotsYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("snapshot fixtures require one document: %v", err)
	}
	guardNames, snapshotNames := make(map[string]bool), make(map[string]bool)
	for _, row := range document.Guards {
		if row.Name == "" || guardNames[row.Name] {
			t.Fatalf("invalid guard fixture: %+v", row)
		}
		switch row.Scope {
		case indexGuardNil, indexGuardEmpty, indexGuardSupported, indexGuardUnsupported, indexGuardAll:
		default:
			t.Fatalf("invalid scope %q", row.Scope)
		}
		guardNames[row.Name] = true
	}
	for _, row := range document.Snapshots {
		if row.Name == "" || snapshotNames[row.Name] || (row.Operation != indexReadEntries && row.Operation != indexReadRange) {
			t.Fatalf("invalid snapshot fixture: %+v", row)
		}
		snapshotNames[row.Name] = true
	}
	if err := testutil.RequireFixtureNames("index snapshot", "guard", document.RequiredGuardNames, guardNames); err != nil {
		t.Fatal(err)
	}
	if err := testutil.RequireFixtureNames("index snapshot", "reader", document.RequiredSnapshotNames, snapshotNames); err != nil {
		t.Fatal(err)
	}
	return document
}

func TestIndexFormatGuardRequiresExplicitScopeAndSnapshot(t *testing.T) {
	for _, row := range loadIndexFormatSnapshotFixtures(t).Guards {
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			db := openTestStore(t)
			supported := schema.SessionID(testutil.TestSessionUUID)
			unsupported := schema.SessionID(testutil.TestSessionUUID2)
			seedSession(t, db, string(supported))
			seedSession(t, db, string(unsupported))
			conn := takeConn(t, db.Pool())
			defer db.Pool().Put(conn)
			if err := sqlitex.ExecuteTransient(conn, "UPDATE sessions SET index_format_version = 99 WHERE session_id = ?", &sqlitex.ExecOptions{Args: []any{string(unsupported)}}); err != nil {
				t.Fatal(err)
			}
			if row.DenyReads {
				if err := conn.SetAuthorizer(sqlite.AuthorizeFunc(func(action sqlite.Action) sqlite.AuthResult {
					if action.Type() == sqlite.OpRead {
						return sqlite.AuthResultDeny
					}
					return sqlite.AuthResultOK
				})); err != nil {
					t.Fatal(err)
				}
				defer conn.SetAuthorizer(nil)
			}
			var err error
			if row.Snapshot {
				end := sqlitex.Save(conn)
				defer end(&err)
			}
			switch row.Scope {
			case indexGuardNil:
				err = db.ValidateIndexFormatsOnConn(conn, nil)
			case indexGuardEmpty:
				err = db.ValidateIndexFormatsOnConn(conn, []schema.SessionID{})
			case indexGuardSupported:
				err = db.ValidateIndexFormatsOnConn(conn, []schema.SessionID{supported})
			case indexGuardUnsupported:
				ids := make([]schema.SessionID, row.PrefixIDs)
				for i := range ids {
					ids[i] = supported
				}
				ids = append(ids, unsupported)
				err = db.ValidateIndexFormatsOnConn(conn, ids)
			case indexGuardAll:
				err = db.ValidateAllIndexFormatsOnConn(conn)
			}
			if row.WantError != "" {
				if err == nil || !strings.Contains(err.Error(), row.WantError) {
					t.Fatalf("guard error=%v, want %q", err, row.WantError)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestIndexFormatReadersKeepBaseAndExtraInOneSnapshot(t *testing.T) {
	for _, row := range loadIndexFormatSnapshotFixtures(t).Snapshots {
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "snapshot.db")
			reader, err := store.Open(path, store.WithPoolSize(1))
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			writer, err := store.Open(path, store.WithPoolSize(1))
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Close()
			sid := schema.SessionID(testutil.TestSessionUUID)
			seedSession(t, reader, string(sid))
			entries := batchTestEntries(sid, "old snapshot", 1)
			entries[0].Extra = strPtr(`{"model_id":"old-model"}`)
			if err := reader.IndexSessionEntries(t.Context(), sid, entries); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			ready, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			defer once.Do(func() { close(release) })
			var paused atomic.Bool
			conn := takeConn(t, reader.Pool())
			if err := conn.SetAuthorizer(sqlite.AuthorizeFunc(func(action sqlite.Action) sqlite.AuthResult {
				if action.Type() == sqlite.OpRead && action.Table() == "session_entries_ext" && paused.CompareAndSwap(false, true) {
					close(ready)
					select {
					case <-release:
					case <-ctx.Done():
						return sqlite.AuthResultDeny
					}
				}
				return sqlite.AuthResultOK
			})); err != nil {
				t.Fatal(err)
			}
			reader.Pool().Put(conn)
			type readResult struct {
				entries []schema.SessionEntry
				err     error
			}
			finished := make(chan readResult, 1)
			go func() {
				var value readResult
				if row.Operation == indexReadRange {
					value.entries, value.err = reader.ListEntriesRange(ctx, sid, 0, 0)
				} else {
					value.entries, value.err = reader.ListEntries(ctx, sid)
				}
				finished <- value
			}()
			select {
			case <-ready:
			case value := <-finished:
				t.Fatalf("reader returned before base/extra barrier: %v", value.err)
			case <-ctx.Done():
				t.Fatal("reader did not reach base/extra barrier")
			}
			writeConn, err := writer.Pool().Take(ctx)
			if err != nil {
				t.Fatal(err)
			}
			// Another build commits a newer representation while the old reader is
			// between its base-row and ext-row statements. No source files exist.
			err = sqlitex.ExecuteScript(writeConn, `
UPDATE sessions SET index_format_version = 99;
UPDATE session_entries SET content_preview = 'new snapshot';
UPDATE session_entries_ext SET value_text = 'new-model' WHERE key = 'model_id';`, nil)
			writer.Pool().Put(writeConn)
			if err != nil {
				t.Fatal(err)
			}
			once.Do(func() { close(release) })
			var value readResult
			select {
			case value = <-finished:
			case <-ctx.Done():
				t.Fatal("snapshot reader did not finish")
			}
			if value.err != nil || len(value.entries) != 1 || value.entries[0].ContentPreview == nil || *value.entries[0].ContentPreview != "old snapshot-0" {
				t.Fatalf("mixed base snapshot: %+v %v", value.entries, value.err)
			}
			var extra map[string]any
			if value.entries[0].Extra == nil {
				t.Fatal("snapshot lost ext values")
			}
			if err := json.Unmarshal([]byte(*value.entries[0].Extra), &extra); err != nil {
				t.Fatal(err)
			}
			if extra["model_id"] != "old-model" {
				t.Fatalf("mixed base/ext generations: %v", extra)
			}
			_, err = reader.ListEntries(ctx, sid)
			var unsupported *store.UnsupportedIndexFormatError
			if !errors.As(err, &unsupported) || unsupported.Version != 99 {
				t.Fatalf("next snapshot did not observe newer format: %v", err)
			}
		})
	}
}
