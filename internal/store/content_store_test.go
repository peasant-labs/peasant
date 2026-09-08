package store_test

import (
	"context"
	_ "embed"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/full_content.yaml
var fullContentYAML []byte

type contentCase struct {
	Name            string
	Prefix          string
	Repeats         int
	Tail            string
	ReplacementTail string `yaml:"replacement_tail"`
	Entries         int
	Budget          int64
}
type contentSQLCase struct {
	Name string
	SQL  string
}
type contentFormatCase struct {
	Name   string
	Format string
}

// contentModeRefusalCase names one declared-but-unserved content mode. Exactly
// one of ReadMode and WriteMode is set; the empty one selects no operation.
type contentModeRefusalCase struct {
	Name              string                       `yaml:"name"`
	ReadMode          ingest.SessionEntryReadMode  `yaml:"read_mode"`
	WriteMode         ingest.SessionEntryWriteMode `yaml:"write_mode"`
	WantErrorContains string                       `yaml:"want_error_contains"`
}

type contentFixtures struct {
	Cases                   []contentCase
	Corruptions             []contentSQLCase
	ShapeChanges            []contentSQLCase         `yaml:"shape_changes"`
	CaptureFormatRejections []contentFormatCase      `yaml:"capture_format_rejections"`
	ModeRefusals            []contentModeRefusalCase `yaml:"mode_refusals"`
}

// contentCaseNamed selects an entry shape by NAME, so this fixture's order
// never decides what another test seeds.
func contentCaseNamed(t *testing.T, fixtures contentFixtures, name string) contentCase {
	t.Helper()
	for _, c := range fixtures.Cases {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("full content fixtures have no case %q; restore it or name an existing case", name)
	return contentCase{}
}

func loadContentFixtures(t *testing.T) contentFixtures {
	t.Helper()
	var f contentFixtures
	if err := yaml.Unmarshal(fullContentYAML, &f); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, c := range f.Cases {
		names[c.Name] = true
	}
	for _, c := range f.Corruptions {
		names[c.Name] = true
	}
	for _, c := range f.ShapeChanges {
		names[c.Name] = true
	}
	for _, c := range f.CaptureFormatRejections {
		names[c.Name] = true
	}
	for _, c := range f.ModeRefusals {
		names[c.Name] = true
		if (c.ReadMode == "") == (c.WriteMode == "") {
			t.Fatalf("mode refusal %q must name exactly one of read_mode and write_mode", c.Name)
		}
		if c.ReadMode != "" {
			if _, err := ingest.NewSessionEntryReadMode(string(c.ReadMode)); err != nil {
				t.Fatalf("mode refusal %q: %v", c.Name, err)
			}
		}
		if c.WriteMode != "" {
			if _, err := ingest.NewSessionEntryWriteMode(string(c.WriteMode)); err != nil {
				t.Fatalf("mode refusal %q: %v", c.Name, err)
			}
		}
		if c.WantErrorContains == "" {
			t.Fatalf("mode refusal %q must state the refusal it expects", c.Name)
		}
	}
	for _, name := range []string{"long_unicode", "oversized_progress", "unicode_preview_boundary", "missing_chunk", "damaged_chunk", "wrong_capture_hash", "wrong_tool_input", "wrong_manifest_hash", "extra", "parent_id", "derived_ext", "derived_command", "timestamp", "tool_output", "unknown_capture_format", "available_read_refused", "format_conversion_write_refused"} {
		if !names[name] {
			t.Fatalf("required fixture %s missing", name)
		}
	}
	return f
}
func contentEntries(id ingest.SessionID, c contentCase) []schema.SessionEntry {
	entries := batchTestEntries(id, "full", c.Entries)
	for i := range entries {
		entries[i].ContentPreview = strPtr(strings.Repeat(c.Prefix, c.Repeats) + c.Tail)
		entries[i].ToolInput = strPtr(`{"path":"` + strings.Repeat("segment/", 400) + `file.go"}`)
		entries[i].ToolOutput = strPtr(strings.Repeat("tool-output", 500))
		entries[i].Extra = strPtr(`{"model_id":"synthetic","command_name":"inspect","command_args":"synthetic"}`)
	}
	return entries
}
func writeFull(t *testing.T, s *store.Store, id ingest.SessionID, entries []schema.SessionEntry, mode ingest.SessionEntryWriteMode) ingest.SessionEntryWriteResult {
	t.Helper()
	r := s.IndexSessionEntryBatch(context.Background(), []ingest.SessionEntryWrite{{SessionID: id, Result: indexformat.V1{Entries: entries}, IndexVersion: 1, RequireFullContent: true, Mode: mode, IndexerVersion: ingest.HarvesterVersionRegistry[ingest.HarnessClaudeCode].IndexerVersion, IndexedAtMs: 1700000005000}})[0]
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	return r
}
func capture(t *testing.T, s *store.Store, id ingest.SessionID) ingest.SessionContentCapture {
	t.Helper()
	c, ok, err := s.GetSessionContentCapture(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("capture missing: %v", err)
	}
	return c
}
func execContentSQL(t *testing.T, s *store.Store, query string) {
	t.Helper()
	c := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(c)
	if err := sqlitex.ExecuteScript(c, query, nil); err != nil {
		t.Fatal(err)
	}
}

func TestFullContentDurabilityAndPaging(t *testing.T) {
	for _, f := range loadContentFixtures(t).Cases {
		t.Run(f.Name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "content.db")
			s, err := store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			id := ingest.SessionID("aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa")
			seedSession(t, s, string(id))
			entries := contentEntries(id, f)
			writeFull(t, s, id, entries, ingest.SessionEntryWriteReplaceAll)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			preview, err := s.ListEntries(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if len(*preview[0].ContentPreview) > 2000 || !utf8.ValidString(*preview[0].ContentPreview) {
				t.Fatal("preview not UTF-8 bounded")
			}
			if *preview[0].ToolInput != *entries[0].ToolInput || *preview[0].ToolOutput != *entries[0].ToolOutput {
				t.Fatal("semantic tool strings changed")
			}
			from, total := 0, 0
			for calls := 0; ; calls++ {
				if calls > len(entries) {
					t.Fatal("pagination failed to progress")
				}
				p, err := s.ReadSessionEntries(context.Background(), id, ingest.SessionEntryReadOptions{Mode: ingest.SessionEntryReadFullContent, FromIndex: from, SoftMaxBytes: f.Budget})
				if err != nil {
					t.Fatal(err)
				}
				for _, e := range p.Entries {
					if !reflect.DeepEqual(e, entries[total]) {
						t.Fatalf("entry %d changed", total)
					}
					total++
				}
				if p.BytesRead > f.Budget && len(p.Entries) != 1 {
					t.Fatal("oversized page should contain one entry")
				}
				if p.NextIndex == nil {
					break
				}
				if *p.NextIndex <= from {
					t.Fatal("nonadvancing cursor")
				}
				from = *p.NextIndex
			}
			if total != len(entries) {
				t.Fatal("missing entries")
			}
			old := capture(t, s, id)
			previewHash := sessionEntriesHash(t, s, id)
			if !writeFull(t, s, id, entries, ingest.SessionEntryWriteReplaceAll).Skipped {
				t.Fatal("identical complete capture did not skip")
			}
			entries[0].ContentPreview = strPtr(strings.Repeat(f.Prefix, f.Repeats) + f.ReplacementTail)
			if writeFull(t, s, id, entries, ingest.SessionEntryWriteReplaceAll).Skipped {
				t.Fatal("different full tail incorrectly skipped")
			}
			if capture(t, s, id).FullCaptureSHA256 == old.FullCaptureSHA256 {
				t.Fatal("tail omitted from hash")
			}
			if sessionEntriesHash(t, s, id) != previewHash {
				t.Fatal("same prefix preview hash changed")
			}
			p, err := s.ReadSessionEntries(context.Background(), id, ingest.SessionEntryReadOptions{Mode: ingest.SessionEntryReadFullContent, Limit: 1})
			if err != nil {
				t.Fatal(err)
			}
			if *p.Entries[0].ContentPreview != *entries[0].ContentPreview {
				t.Fatal("stale tail after rewrite")
			}
		})
	}
}
func TestFullContentCorruptionRefusedAndRepaired(t *testing.T) {
	f := loadContentFixtures(t)
	for _, c := range f.Corruptions {
		t.Run(c.Name, func(t *testing.T) {
			s := openTestStore(t)
			id := ingest.SessionID("aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa")
			seedSession(t, s, string(id))
			entries := contentEntries(id, f.Cases[0])
			writeFull(t, s, id, entries, ingest.SessionEntryWriteReplaceAll)
			execContentSQL(t, s, c.SQL)
			if _, err := s.ReadSessionEntries(context.Background(), id, ingest.SessionEntryReadOptions{Mode: ingest.SessionEntryReadFullContent}); err == nil {
				t.Fatal("corrupt full capture accepted")
			}
			if writeFull(t, s, id, entries, ingest.SessionEntryWriteReplaceAll).Skipped {
				t.Fatal("corrupt capture skipped")
			}
			if _, err := s.ReadSessionEntries(context.Background(), id, ingest.SessionEntryReadOptions{Mode: ingest.SessionEntryReadFullContent}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestFullContentBackfillShapeRollback(t *testing.T) {
	f := loadContentFixtures(t)
	for _, c := range f.ShapeChanges {
		t.Run(c.Name, func(t *testing.T) {
			s := openTestStore(t)
			id := ingest.SessionID("aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa")
			seedSession(t, s, string(id))
			entries := contentEntries(id, f.Cases[0])
			writeFull(t, s, id, entries, ingest.SessionEntryWriteReplaceAll)
			execContentSQL(t, s, c.SQL)
			old := capture(t, s, id)
			hash := sessionEntriesHash(t, s, id)
			before, err := s.ListEntries(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			r := s.IndexSessionEntryBatch(context.Background(), []ingest.SessionEntryWrite{{SessionID: id, Result: indexformat.V1{Entries: entries}, IndexVersion: 1, RequireFullContent: true, Mode: ingest.SessionEntryWriteContentBackfill}})[0]
			if !errors.Is(r.Err, store.ContentBackfillShapeMismatch) {
				t.Fatalf("want shape mismatch, got %v", r.Err)
			}
			after, err := s.ListEntries(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) || capture(t, s, id) != old || sessionEntriesHash(t, s, id) != hash {
				t.Fatal("failed backfill changed stored state")
			}
		})
	}
}

func TestFullContentLegacyBackfillAndKeyset(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id := ingest.SessionID("aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa")
	next := ingest.SessionID("bbbbbbbb-1111-4111-8111-aaaaaaaaaaaa")
	seedSession(t, s, string(id))
	seedSession(t, s, string(next))
	f := loadContentFixtures(t).Cases[0]
	entries := contentEntries(id, f)
	preview := append([]schema.SessionEntry(nil), entries...)
	for i := range preview {
		preview[i].ContentPreview = strPtr((*entries[i].ContentPreview)[:1998])
	}
	// 1998 ends after the next three-byte character, before its four-byte emoji.
	if err := s.IndexSessionEntries(ctx, id, preview); err != nil {
		t.Fatal(err)
	}
	if capture(t, s, id).Status != ingest.ContentCaptureIncomplete {
		t.Fatal("legacy caller certified complete")
	}
	if _, err := s.ReadSessionEntries(ctx, id, ingest.SessionEntryReadOptions{Mode: ingest.SessionEntryReadFullContent}); err == nil {
		t.Fatal("legacy full read succeeded")
	}
	targets, err := s.ListContentCaptureIncompleteSessions(ctx, 1)
	if err != nil || len(targets) != 1 || targets[0] != id {
		t.Fatalf("first targets: %v %v", targets, err)
	}
	later, err := s.ListContentCaptureIncompleteSessionsAfter(ctx, id, 1)
	if err != nil || len(later) != 1 || later[0].SessionID != next {
		t.Fatalf("later targets: %v %v", later, err)
	}
	if later[0].Harness != ingest.HarnessClaudeCode || later[0].StartMs != 1700000000000 {
		t.Fatalf("later target lost its harness or start time: %+v", later[0])
	}
	hash := sessionEntriesHash(t, s, id)
	writeFull(t, s, id, entries, ingest.SessionEntryWriteContentBackfill)
	if sessionEntriesHash(t, s, id) != hash || capture(t, s, id).Status != ingest.ContentCaptureComplete {
		t.Fatal("content-only backfill changed preview hash or remained incomplete")
	}
}

// A page of sessions this build cannot harness-parse must not come back empty.
// The listing filters them in SQL, so they never consume a LIMIT slot; if they
// were dropped in Go afterwards, a full page of them would look like the end of
// the table and every recoverable session behind it would never be visited.
func TestContentRecoveryTargetsSkipUnknownHarnessWithoutEndingTheWalk(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	unknownFirst := ingest.SessionID("aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa")
	unknownSecond := ingest.SessionID("bbbbbbbb-1111-4111-8111-aaaaaaaaaaaa")
	recoverable := ingest.SessionID("cccccccc-1111-4111-8111-aaaaaaaaaaaa")
	for _, id := range []ingest.SessionID{unknownFirst, unknownSecond, recoverable} {
		seedSession(t, s, string(id))
	}
	// Only a database written by a build whose harness set is wider than this
	// one can hold these rows, so the CHECK mirror is suspended to write them.
	execContentSQL(t, s, `PRAGMA ignore_check_constraints=ON;
UPDATE sessions SET model_harness='harness-from-a-later-build' WHERE session_id IN ('`+string(unknownFirst)+`','`+string(unknownSecond)+`');
PRAGMA ignore_check_constraints=OFF;`)
	if got := storedHarness(t, s, unknownFirst); got != "harness-from-a-later-build" {
		t.Fatalf("the unrecognised harness was not stored: %q", got)
	}

	// One eligible row is behind two ineligible ones, so a page of one proves
	// the LIMIT counted only eligible rows.
	targets, err := s.ListContentCaptureIncompleteSessionsAfter(ctx, "", 1)
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if len(targets) != 1 || targets[0].SessionID != recoverable {
		t.Fatalf("a page of one did not reach the recoverable session behind the unrecognised rows: %+v", targets)
	}
	if targets[0].Harness != ingest.HarnessClaudeCode {
		t.Fatalf("recoverable target lost its harness: %+v", targets[0])
	}
	ids, err := s.ListContentCaptureIncompleteSessions(ctx, 3)
	if err != nil {
		t.Fatalf("full page: %v", err)
	}
	if len(ids) != 1 || ids[0] != recoverable {
		t.Fatalf("unrecognised sessions were offered as recovery targets: %v", ids)
	}
	after, err := s.ListContentCaptureIncompleteSessionsAfter(ctx, recoverable, 1)
	if err != nil || len(after) != 0 {
		t.Fatalf("the walk did not end after the last eligible session: %v %v", after, err)
	}
}

func storedHarness(t *testing.T, s *store.Store, id ingest.SessionID) string {
	t.Helper()
	c := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(c)
	got := ""
	if err := sqlitex.ExecuteTransient(c, `SELECT model_harness FROM sessions WHERE session_id=?`, &sqlitex.ExecOptions{Args: []any{string(id)}, ResultFunc: func(st *sqlite.Stmt) error {
		got = st.ColumnText(0)
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestFullContentHotReadsAvoidPayloadTables(t *testing.T) {
	s := openTestStore(t)
	id := ingest.SessionID("aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa")
	seedSession(t, s, string(id))
	writeFull(t, s, id, contentEntries(id, loadContentFixtures(t).Cases[0]), ingest.SessionEntryWriteReplaceAll)
	// Install the deny policy on every pool connection; even preparing a read of
	// either payload table must fail, so a successful hot read proves separation.
	a := takeConn(t, s.PoolForTest())
	b := takeConn(t, s.PoolForTest())
	auth := sqlite.AuthorizeFunc(func(action sqlite.Action) sqlite.AuthResult {
		if strings.HasPrefix(action.Table(), "session_entry_full_content") {
			return sqlite.AuthResultDeny
		}
		return sqlite.AuthResultOK
	})
	if err := a.SetAuthorizer(auth); err != nil {
		t.Fatal(err)
	}
	if err := b.SetAuthorizer(auth); err != nil {
		t.Fatal(err)
	}
	s.PoolForTest().Put(a)
	s.PoolForTest().Put(b)
	if _, err := s.ListEntries(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListEntriesRange(context.Background(), id, 0, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadSessionEntries(context.Background(), id, ingest.SessionEntryReadOptions{Mode: ingest.SessionEntryReadPreview}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadSessionEntries(context.Background(), id, ingest.SessionEntryReadOptions{Mode: ingest.SessionEntryReadFullContent}); err == nil {
		t.Fatal("deny policy did not reject full reads")
	}
}

func TestFullContentBackfillFailurePreservesAnnotationsAndLaterWrites(t *testing.T) {
	s := openTestStore(t)
	seedReindexSession(t, s)
	id := ingest.SessionID(reindexSessionID)
	f := loadContentFixtures(t).Cases[0]
	entries := contentEntries(id, f)
	writeFull(t, s, id, entries, ingest.SessionEntryWriteReplaceAll)
	annotation := annotateReindexEntry(t, s, 0, 1)
	old := capture(t, s, id)
	hash := sessionEntriesHash(t, s, id)
	entries[0].ContentPreview = strPtr(strings.Repeat(f.Prefix, f.Repeats) + f.ReplacementTail)
	execContentSQL(t, s, `CREATE TRIGGER fail_content_chunk BEFORE INSERT ON session_entry_full_content_chunks WHEN NEW.session_id='eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee' AND NEW.chunk_index=1 BEGIN SELECT RAISE(ABORT,'synthetic chunk failure'); END;`)
	later := ingest.SessionID("ffffffff-1111-4111-8111-aaaaaaaaaaaa")
	seedSession(t, s, string(later))
	r := s.IndexSessionEntryBatch(context.Background(), []ingest.SessionEntryWrite{
		{SessionID: id, Result: indexformat.V1{Entries: entries}, IndexVersion: 1, RequireFullContent: true, Mode: ingest.SessionEntryWriteContentBackfill, IndexerVersion: 999, IndexedAtMs: 1},
		{SessionID: later, Result: indexformat.V1{Entries: contentEntries(later, f)}, IndexVersion: 1, RequireFullContent: true},
	})
	if r[0].Err == nil || r[0].Written || r[1].Err != nil || !r[1].Written {
		t.Fatalf("savepoint outcomes: %+v", r)
	}
	if capture(t, s, id) != old || sessionEntriesHash(t, s, id) != hash {
		t.Fatal("failed chunk replacement changed capture or index hash")
	}
	assertIndexState(t, s, id, ingest.HarvesterVersionRegistry[ingest.HarnessClaudeCode].IndexerVersion, 1700000005000)
	assertReindexTargetSpan(t, s, annotation, 0, 1)
	p, err := s.ReadSessionEntries(context.Background(), id, ingest.SessionEntryReadOptions{Mode: ingest.SessionEntryReadFullContent, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(*p.Entries[0].ContentPreview, f.Tail) {
		t.Fatal("failed write replaced old content")
	}
	execContentSQL(t, s, `DROP TRIGGER fail_content_chunk;`)
	writeFull(t, s, id, entries, ingest.SessionEntryWriteContentBackfill)
	assertReindexTargetSpan(t, s, annotation, 0, 1)
	assertIndexState(t, s, id, ingest.HarvesterVersionRegistry[ingest.HarnessClaudeCode].IndexerVersion, 1700000005000)
	// A canonical replacement also carries existing entry anchors.
	entries[0].ToolOutput = strPtr("updated semantic tool output")
	writeFull(t, s, id, entries, ingest.SessionEntryWriteReplaceAll)
	assertReindexTargetSpan(t, s, annotation, 0, 1)
}

// A caller-supplied capture format outside the canonical set is refused at the
// Go boundary with an answerable message, and the stored capture is untouched.
func TestFullContentWriteRefusesUnknownCaptureFormat(t *testing.T) {
	fixtures := loadContentFixtures(t)
	for _, rejection := range fixtures.CaptureFormatRejections {
		t.Run(rejection.Name, func(t *testing.T) {
			s := openTestStore(t)
			id := ingest.SessionID("aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa")
			seedSession(t, s, string(id))
			entries := contentEntries(id, contentCaseNamed(t, fixtures, "long_unicode"))
			writeFull(t, s, id, entries, ingest.SessionEntryWriteReplaceAll)
			before := capture(t, s, id)
			if before.CaptureFormat != ingest.ContentCaptureFormatFull {
				t.Fatalf("seeded capture is not full: %s", before.CaptureFormat)
			}
			r := s.IndexSessionEntryBatch(context.Background(), []ingest.SessionEntryWrite{{
				SessionID: id, Result: indexformat.V1{Entries: entries}, IndexVersion: 1, RequireFullContent: true,
				IndexerVersion: ingest.HarvesterVersionRegistry[ingest.HarnessClaudeCode].IndexerVersion, IndexedAtMs: 1700000006000,
				ContentCapture: ingest.SessionContentCaptureWrite{
					Status: ingest.ContentCaptureComplete, SourceAuthority: ingest.ContentSourceNewIngest,
					CaptureFormat: ingest.ContentCaptureFormat(rejection.Format),
				},
			}})[0]
			if r.Err == nil {
				t.Fatal("an unknown capture format was stored instead of refused")
			}
			if !strings.Contains(r.Err.Error(), "outside the closed set") {
				t.Fatalf("refusal does not name the closed set: %v", r.Err)
			}
			if after := capture(t, s, id); !reflect.DeepEqual(after, before) {
				t.Fatalf("refused write changed the stored capture: %+v want %+v", after, before)
			}
		})
	}
}

// TestContentModeRefusalsPreserveStoredCapture proves that a mode this build
// declares but does not serve refuses with an actionable error, serves no
// entries, and leaves the stored capture and its producer evidence untouched.
func TestContentModeRefusalsPreserveStoredCapture(t *testing.T) {
	fixtures := loadContentFixtures(t)
	for _, refusal := range fixtures.ModeRefusals {
		t.Run(refusal.Name, func(t *testing.T) {
			t.Parallel()
			s := openTestStore(t)
			ctx := context.Background()
			id := ingest.SessionID("aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa")
			seedSession(t, s, string(id))
			entries := contentEntries(id, contentCaseNamed(t, fixtures, "long_unicode"))
			writeFull(t, s, id, entries, ingest.SessionEntryWriteReplaceAll)
			before, beforeHash := capture(t, s, id), sessionEntriesHash(t, s, id)

			var err error
			switch {
			case refusal.ReadMode != "":
				var page ingest.SessionEntryReadPage
				page, err = s.ReadSessionEntries(ctx, id, ingest.SessionEntryReadOptions{Mode: refusal.ReadMode})
				if len(page.Entries) != 0 {
					t.Fatalf("refused read served %d entries", len(page.Entries))
				}
			case refusal.WriteMode != "":
				err = s.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{
					SessionID: id, Result: indexformat.V1{Entries: entries}, IndexVersion: 1,
					RequireFullContent: true, Mode: refusal.WriteMode,
					IndexerVersion: ingest.HarvesterVersionRegistry[ingest.HarnessClaudeCode].IndexerVersion,
					IndexedAtMs:    1700000009000,
				}})[0].Err
			}
			if err == nil {
				t.Fatalf("unserved mode was accepted")
			}
			if !strings.Contains(err.Error(), refusal.WantErrorContains) {
				t.Fatalf("refusal %q does not tell the caller %q", err, refusal.WantErrorContains)
			}
			if after := capture(t, s, id); after != before {
				t.Fatalf("refused mode changed the stored capture:\nbefore %+v\nafter  %+v", before, after)
			}
			if sessionEntriesHash(t, s, id) != beforeHash {
				t.Fatal("refused mode changed the stored entry projection")
			}
		})
	}
}
