package store

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
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
	"zombiezen.com/go/sqlite"
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
			if err := sqlitemigration.Migrate(ctx, conn, frozenSchema(52)); err != nil {
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
			revision := c.PublicationRevision
			// Seeded as a V52 store wrote it, through the historical column
			// names and shapes. The current writers are deliberately NOT used
			// here: they write today's columns, so running them against a
			// historical schema either fails outright or, worse, produces a
			// state no installed database ever held.
			seedV52PublishedFullCapture(t, conn, publication, text, revision)
			beforeContent := rawContentRows(t, conn, sid)
			// Read the digest the seed actually stored, so the expectation
			// below is derived from the pre-upgrade database and not recomputed
			// by the same code the assertion is checking.
			seededCaptureHash := scalarText(t, conn, `SELECT full_capture_sha256 FROM session_content_captures WHERE session_id='`+string(sid)+`'`)
			if len(seededCaptureHash) != 64 {
				t.Fatalf("seeded capture digest=%q, want a 64-character digest", seededCaptureHash)
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
			// The stored capture must survive byte for byte. Compared as raw
			// rows, because a current reader cannot describe the predecessor
			// state and would only ever agree with itself.
			if afterContent := rawContentRows(t, conn, sid); !reflect.DeepEqual(beforeContent, afterContent) {
				t.Fatalf("the upgrade rewrote stored content:\nbefore %v\nafter  %v", beforeContent, afterContent)
			}
			// Current readers, now that the schema is current.
			afterEntries, afterCapture, err := newDB.LoadFullSessionEntries(ctx, sid, 0)
			if err != nil || len(afterEntries) != 1 || afterEntries[0].EntryIndex != 0 ||
				afterEntries[0].ContentPreview == nil || *afterEntries[0].ContentPreview != text {
				t.Fatalf("verified full capture is not readable after the upgrade: %+v %v", afterEntries, err)
			}
			wantCapture := ingest.SessionContentCapture{
				PublicationCaptureRevision: revision, SessionID: sid,
				Status: ingest.ContentCaptureComplete, SourceAuthority: ingest.ContentSourceNewIngest,
				CaptureFormat: ingest.ContentCaptureFormatFull, EntryCount: 1, ContentRowCount: 1,
				FullCaptureSHA256: seededCaptureHash, CapturedAtMs: v52SeedCapturedAtMs,
			}
			if !reflect.DeepEqual(afterCapture, wantCapture) {
				t.Fatalf("capture after the upgrade=%+v, want %+v", afterCapture, wantCapture)
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

// v52SeedCapturedAtMs is a fixed capture time, so the upgraded row is compared
// against a stated value rather than against whatever the clock said.
const v52SeedCapturedAtMs int64 = 1700000000000

// seedV52PublishedFullCapture writes one published session with a verified
// complete capture, using the columns a V52 store actually had. Note
// session_content_captures.capture_revision: at V52 that column carried the
// capture FORMAT as free text, which migration 59 later renames to
// capture_format and constrains. Seeding today's column name here would test
// the migration against a database that never existed.
func seedV52PublishedFullCapture(t *testing.T, conn *sqlite.Conn, entry ingest.StoreEntry, text string, revision int64) {
	t.Helper()
	m := entry.Metadata
	id := string(m.SessionID)
	preview := contentPreview(text)
	stored := []schema.SessionEntry{{
		SessionID: m.SessionID, EntryIndex: 0, Harness: schema.HarnessClaudeCode,
		Role: schema.RoleUser, EntryType: schema.EntryTypeText, ContentPreview: &preview,
	}}
	entriesHash, err := computeSessionEntriesHash(stored)
	if err != nil {
		t.Fatal(err)
	}
	full := stored[0]
	full.ContentPreview = &text
	captureHash, err := fullCaptureHash([]schema.SessionEntry{full})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{Args: args}); err != nil {
			t.Fatalf("seed historical row: %v\n%s", err, query)
		}
	}
	// The session must be attributed to the captured project, or the capture
	// and the row it describes disagree. This UPDATE names a column the v51
	// trigger watches, so it clears the indexed binding and the recovered
	// provenance; the statement after it restores both. That ordering is the
	// point, not an accident: a watched fact really did change here.
	exec(`INSERT OR IGNORE INTO projects (project_hash, canonical_cwd) VALUES (?,?)`, string(m.Project.Hash), m.Project.FilePath)
	exec(`UPDATE sessions SET project_hash=? WHERE session_id=?`, string(m.Project.Hash), id)
	exec(`UPDATE sessions SET index_version=?, indexed_at=?, session_entries_hash=?, session_cwd=?,
 cwd_provenance_kind=?, publication_capture_revision=?, indexed_publication_capture_revision=? WHERE session_id=?`,
		ingest.HarvesterVersionRegistry[ingest.HarnessClaudeCode].IndexerVersion, int64(4), entriesHash,
		m.CWD, string(entry.CWDProvenance), revision, revision, id)
	exec(`INSERT INTO session_metrics (session_id,turn_count,tool_calls,input_tokens,output_tokens,duration_minutes) VALUES (?,?,?,?,?,?)`,
		id, 1, 0, 0, 0, 0.0)
	exec(`INSERT INTO session_publication_metadata (session_id,capture_revision,schema_version,metadata_json,metadata_hash,content_hash,captured_at) VALUES (?,?,?,?,?,?,?)`,
		id, revision, m.SchemaVersion, string(body), m.MetadataHash, m.ContentHash, v52SeedCapturedAtMs)
	exec(`INSERT INTO session_entries (session_id,entry_index,provider,entry_type,role,content_preview) VALUES (?,?,?,?,?,?)`,
		id, 0, schema.HarnessClaudeCode.String(), string(schema.EntryTypeText), string(schema.RoleUser), preview)
	exec(`INSERT INTO session_content_captures (session_id,status,source_authority,transcript_origin,capture_revision,entry_count,content_row_count,full_capture_sha256,captured_at_ms,failure_code,failure_message,publication_capture_revision) VALUES (?,?,?,?,?,?,?,?,?,NULL,NULL,?)`,
		id, string(ingest.ContentCaptureComplete), string(ingest.ContentSourceNewIngest), 0,
		"full-content-v1", 1, 1, captureHash, v52SeedCapturedAtMs, revision)
	chunks := (len(text) + fullContentChunkBytes - 1) / fullContentChunkBytes
	exec(`INSERT INTO session_entry_full_content VALUES(?,?,?,?,?,?,?,?,?)`,
		id, 0, len(text), contentSHA(text), len(preview), contentSHA(preview),
		boolToInt(text == preview), chunks, v52SeedCapturedAtMs)
	for offset, index := 0, 0; offset < len(text); index++ {
		end := min(offset+fullContentChunkBytes, len(text))
		chunk := text[offset:end]
		exec(`INSERT INTO session_entry_full_content_chunks VALUES(?,?,?,?,?,?,?)`,
			id, 0, index, offset, len(chunk), contentSHA(chunk), []byte(chunk))
		offset = end
	}
}

// rawContentRows reads the stored content tables as text, without any current
// reader in the way, so the same comparison can be made before and after an
// upgrade. session_content_captures is excluded on purpose: migration 59
// renames one of its columns, so its shape legitimately differs and it is
// asserted separately, by value.
func rawContentRows(t *testing.T, conn *sqlite.Conn, id ingest.SessionID) []string {
	t.Helper()
	var rows []string
	for _, query := range []string{
		`SELECT entry_index,provider,entry_type,role,content_preview FROM session_entries WHERE session_id=? ORDER BY entry_index`,
		`SELECT entry_index,full_byte_length,full_sha256,preview_byte_length,preview_sha256,preview_is_full,chunk_count,captured_at_ms FROM session_entry_full_content WHERE session_id=? ORDER BY entry_index`,
		`SELECT entry_index,chunk_index,byte_offset,byte_length,chunk_sha256,hex(data) FROM session_entry_full_content_chunks WHERE session_id=? ORDER BY entry_index,chunk_index`,
	} {
		if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
			Args: []any{string(id)}, ResultFunc: func(stmt *sqlite.Stmt) error {
				row := make([]string, stmt.ColumnCount())
				for column := range row {
					row[column] = stmt.ColumnType(column).String() + ":" + stmt.ColumnText(column)
				}
				rows = append(rows, strings.Join(row, "|"))
				return nil
			},
		}); err != nil {
			t.Fatalf("read stored content rows: %v", err)
		}
	}
	return rows
}
