package store

import (
	"context"
	"fmt"
	"hash/fnv"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// The consolidated-index recall family: every section-10 search_recall case
// as a real assertion over the production writer and the production query.
// Each subtest seeds an isolated store through ActivateGeneration (bodies)
// or IndexSessionEntries (mirror rows), then reads through SearchMergedOnConn
// — the same merged query the service runs — plus the raw MATCH oracle for
// the delete, sweep, and prune cases. The loader's manifest check still
// protects every name.
func TestSearchRecallFamily(t *testing.T) {
	t.Parallel()
	for _, c := range loadSearchRecallFixtures(t) {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			runSearchRecallCase(t, c)
		})
	}
}

func runSearchRecallCase(t *testing.T, c searchRecallCase) {
	t.Helper()
	switch c.Name {
	case "term-in-tool-output":
		recallToolField(t, c, "output")
	case "term-in-tool-input":
		recallToolField(t, c, "input")
	case "term-in-text-head":
		recallTextHead(t, c)
	case "term-beyond-2000-in-text-found":
		recallBeyond2000(t, c, "preview")
	case "term-beyond-2000-in-tool-output-found":
		recallBeyond2000(t, c, "output")
	case "preview-only-capture-parity":
		recallTextHead(t, c)
	case "preview-only-dedup-parity":
		recallDedupParity(t, c)
	case "dedup-no-longer-hides-full-preview-difference":
		recallDedupWidening(t, c)
	case "tool-row-preview-differs-beyond-2000-found":
		recallToolRowBeyond(t, c)
	case "pi-carrier-excluded":
		recallPiCarrierExcluded(t, c)
	case "superseded-only-term":
		recallSupersededOnly(t, c)
	case "earlier-partition-body-not-returned":
		recallEarlierPartition(t, c)
	case "non-native-term":
		recallNonNative(t, c, "preview")
	case "non-native-indexed-text-unchanged":
		recallNonNative(t, c, "input")
	case "delete-then-match-returns-nothing":
		recallDeleteThenMatch(t, c)
	case "union-view-rowid-pushdown":
		recallPushdown(t, c)
	case "transition-window-match-set":
		recallTransitionWindow(t, c)
	case "transition-window-fallback-filter":
		recallTransitionFallback(t, c)
	case "rowid-space-guard":
		recallRowidGuard(t, c)
	default:
		t.Fatalf("%s: no runner for this recall case", c.Name)
	}
}

func recallSID(name string) schema.SessionID {
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	sid, err := schema.NewSessionID(fmt.Sprintf("c2c2c2c2-c2c2-42c2-82c2-c2c2c2c2%04x", h.Sum32()%65536))
	if err != nil {
		panic(fmt.Sprintf("recall session id for %q: %v", name, err))
	}
	return sid
}

func recallRef(prefix string, index int) schema.SourceEntryRef {
	return schema.SourceEntryRef(fmt.Sprintf("r-%s-%d", prefix, index))
}

func recallEntry(sid schema.SessionID, index int, ref schema.SourceEntryRef) schema.SessionEntry {
	return schema.SessionEntry{
		SessionID:      sid,
		EntryIndex:     index,
		Harness:        defaults.HarnessClaudeCode,
		EntryType:      ingest.EntryTypeText,
		Role:           ingest.RoleUser,
		SourceEntryRef: ref,
	}
}

func withPreview(entry schema.SessionEntry, text string) schema.SessionEntry {
	entry.ContentPreview = &text
	return entry
}

func withToolInput(entry schema.SessionEntry, text string) schema.SessionEntry {
	entry.EntryType = ingest.EntryTypeToolUse
	entry.Role = ingest.RoleAssistant
	entry.ToolInput = &text
	return entry
}

func withToolOutput(entry schema.SessionEntry, text string) schema.SessionEntry {
	entry.EntryType = ingest.EntryTypeToolResult
	entry.Role = ingest.RoleTool
	entry.ToolOutput = &text
	return entry
}

// activateRecallEntries commits one harmonized generation of the given main
// entries (plus optional earlier-partition entries) through the production
// activation path.
func activateRecallEntries(t *testing.T, s *Store, sid schema.SessionID, genID string, entries []schema.SessionEntry, earlier []schema.SessionEntry) {
	t.Helper()
	content := make([]indexformat.ContentRecord, 0, len(entries)+len(earlier))
	blobs := map[schema.SourceEntryRef][]byte{}
	for i := range entries {
		if entries[i].SourceEntryRef == "" {
			entries[i].SourceEntryRef = recallRef(genID, entries[i].EntryIndex)
		}
		content = append(content, indexformat.ContentRecord{Ref: entries[i].SourceEntryRef})
		blobs[entries[i].SourceEntryRef] = []byte(recallContentBytes(entries[i]))
	}
	for i := range earlier {
		if earlier[i].SourceEntryRef == "" {
			earlier[i].SourceEntryRef = recallRef(genID+"-earlier", earlier[i].EntryIndex)
		}
		content = append(content, indexformat.ContentRecord{Ref: earlier[i].SourceEntryRef})
		blobs[earlier[i].SourceEntryRef] = []byte(recallContentBytes(earlier[i]))
	}
	firstRef := entries[0].SourceEntryRef
	inputCount := int64(1)
	generation := indexformat.Generation{
		ID:           genID,
		Completeness: indexformat.GenerationCompletenessComplete,
		Metadata: schema.UnifiedMetadata{
			SchemaVersion: ingest.CurrentSchemaVersion,
			SessionID:     sid,
			ModelHarness:  defaults.HarnessClaudeCode,
			Stats:         schema.SessionStats{TurnCount: len(entries), InputSubmissionCount: &inputCount},
		},
		Main:                 indexformat.Partition{Entries: entries},
		Content:              content,
		Aliases:              []indexformat.NativeAlias{{NativeKey: "native-0", Ref: firstRef}},
		SourceEvidenceDigest: strings.Repeat("a", 64),
		TitleRefs:            []schema.SourceEntryRef{firstRef},
	}
	if len(earlier) > 0 {
		generation.Earlier = []indexformat.EarlierPartition{{
			State:   schema.EarlierHistoryUncertainMigrated,
			Content: indexformat.Partition{Entries: earlier},
		}}
	}
	v2 := indexformat.V2{Generation: generation}
	filled := filledCandidateForValidation(t, v2, blobs)
	if _, err := s.ActivateGeneration(context.Background(), GenerationActivation{
		Generation:     filled,
		Blobs:          blobs,
		IndexerVersion: 1,
		IndexedAtMs:    100,
	}); err != nil {
		t.Fatalf("activate %s: %v", genID, err)
	}
}

func recallContentBytes(entry schema.SessionEntry) string {
	if entry.ToolOutput != nil {
		return *entry.ToolOutput
	}
	if entry.ToolInput != nil {
		return *entry.ToolInput
	}
	if entry.ContentPreview != nil {
		return *entry.ContentPreview
	}
	return ""
}

func recallProduction(t *testing.T, s *Store, term string) []UnifiedSearchHit {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatalf("take connection: %v", err)
	}
	defer s.pool.Put(conn)
	hits, err := SearchMergedOnConn(conn, `"`+term+`"`, 50, 0)
	if err != nil {
		t.Fatalf("production search for %q: %v", term, err)
	}
	return hits
}

func recallRawCount(t *testing.T, s *Store, table, term string) int {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatalf("take connection: %v", err)
	}
	defer s.pool.Put(conn)
	var n int
	if err := sqlitex.ExecuteTransient(conn, `SELECT COUNT(*) FROM `+table+` WHERE `+table+` MATCH ?`, &sqlitex.ExecOptions{
		Args:       []any{`"` + term + `"`},
		ResultFunc: func(stmt *sqlite.Stmt) error { n = int(stmt.ColumnInt64(0)); return nil },
	}); err != nil {
		t.Fatalf("raw MATCH %q on %s: %v", term, table, err)
	}
	return n
}

func recallExec(t *testing.T, s *Store, sql string, args ...any) {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatalf("take connection: %v", err)
	}
	defer s.pool.Put(conn)
	if err := sqlitex.ExecuteTransient(conn, sql, &sqlitex.ExecOptions{Args: args}); err != nil {
		t.Fatalf("exec: %v\nsql: %.200s", err, sql)
	}
}

func assertRecallHits(t *testing.T, c searchRecallCase, hits []UnifiedSearchHit) {
	t.Helper()
	if c.WantCount != nil && len(hits) != *c.WantCount {
		t.Fatalf("%s: production hits = %d, want %d (%v)", c.Name, len(hits), *c.WantCount, hits)
	}
	if len(c.WantEntryIndexes) > 0 {
		if len(hits) != len(c.WantEntryIndexes) {
			t.Fatalf("%s: production hits = %d, want entries %v", c.Name, len(hits), c.WantEntryIndexes)
		}
		for i, want := range c.WantEntryIndexes {
			if hits[i].EntryIndex != want {
				t.Fatalf("%s: hit %d entry = %d, want %d", c.Name, i, hits[i].EntryIndex, want)
			}
		}
	}
	if c.WantNoDuplicates {
		seen := map[string]struct{}{}
		for _, hit := range hits {
			key := hit.SessionID + "\x00" + fmt.Sprintf("%d", hit.EntryIndex)
			if _, ok := seen[key]; ok {
				t.Fatalf("%s: duplicate pair %q in the match set", c.Name, key)
			}
			seen[key] = struct{}{}
		}
	}
}

func recallToolField(t *testing.T, c searchRecallCase, field string) {
	t.Helper()
	s, _ := openGenerationStore(t)
	sid := recallSID(c.Name)
	seedGenerationSession(t, s, string(sid))
	prefix := strings.ReplaceAll(c.Name, "-", "")
	e0 := withPreview(recallEntry(sid, 0, recallRef(prefix, 0)), "neutral head text")
	e1 := withToolInput(recallEntry(sid, 1, recallRef(prefix, 1)), "neutral tool input")
	e2 := withToolOutput(recallEntry(sid, 2, recallRef(prefix, 2)), "neutral tool output")
	switch field {
	case "input":
		e1.ToolInput = &c.Query
	case "output":
		e2.ToolOutput = &c.Query
	}
	activateRecallEntries(t, s, sid, "gen-"+prefix, []schema.SessionEntry{e0, e1, e2}, nil)
	assertRecallHits(t, c, recallProduction(t, s, c.Query))
}

func recallTextHead(t *testing.T, c searchRecallCase) {
	t.Helper()
	s, _ := openGenerationStore(t)
	sid := recallSID(c.Name)
	seedGenerationSession(t, s, string(sid))
	prefix := strings.ReplaceAll(c.Name, "-", "")
	e0 := withPreview(recallEntry(sid, 0, recallRef(prefix, 0)), "head text "+c.Query+" follows")
	e1 := withToolInput(recallEntry(sid, 1, recallRef(prefix, 1)), "neutral input")
	e2 := withToolOutput(recallEntry(sid, 2, recallRef(prefix, 2)), "neutral output")
	activateRecallEntries(t, s, sid, "gen-"+prefix, []schema.SessionEntry{e0, e1, e2}, nil)
	assertRecallHits(t, c, recallProduction(t, s, c.Query))
}

func recallBeyond2000(t *testing.T, c searchRecallCase, field string) {
	t.Helper()
	s, _ := openGenerationStore(t)
	sid := recallSID(c.Name)
	seedGenerationSession(t, s, string(sid))
	prefix := strings.ReplaceAll(c.Name, "-", "")
	pad := strings.Repeat("x", 2200)
	long := pad + " " + c.Query + " " + strings.Repeat("y", 500)
	e0 := withPreview(recallEntry(sid, 0, recallRef(prefix, 0)), "neutral head")
	e1 := withToolInput(recallEntry(sid, 1, recallRef(prefix, 1)), "neutral input")
	e2 := withToolOutput(recallEntry(sid, 2, recallRef(prefix, 2)), "neutral output")
	switch field {
	case "preview":
		e0.ContentPreview = &long
	case "output":
		e2.ToolOutput = &long
	}
	activateRecallEntries(t, s, sid, "gen-"+prefix, []schema.SessionEntry{e0, e1, e2}, nil)
	assertRecallHits(t, c, recallProduction(t, s, c.Query))
}

func recallDedupParity(t *testing.T, c searchRecallCase) {
	t.Helper()
	s, _ := openGenerationStore(t)
	sid := recallSID(c.Name)
	seedGenerationSession(t, s, string(sid))
	prefix := strings.ReplaceAll(c.Name, "-", "")
	preview := "shared echo " + c.Query
	parent := withPreview(recallEntry(sid, 0, recallRef(prefix, 0)), preview)
	child := withPreview(recallEntry(sid, 1, recallRef(prefix, 1)), preview)
	child.Depth = 1
	parentIndex := 0
	child.ParentIndex = &parentIndex
	e2 := withToolOutput(recallEntry(sid, 2, recallRef(prefix, 2)), "neutral output")
	activateRecallEntries(t, s, sid, "gen-"+prefix, []schema.SessionEntry{parent, child, e2}, nil)
	assertRecallHits(t, c, recallProduction(t, s, c.Query))
}

func recallDedupWidening(t *testing.T, c searchRecallCase) {
	t.Helper()
	s, _ := openGenerationStore(t)
	sid := recallSID(c.Name)
	seedGenerationSession(t, s, string(sid))
	prefix := strings.ReplaceAll(c.Name, "-", "")
	shared := strings.Repeat("p", 2000)
	parent := withPreview(recallEntry(sid, 0, recallRef(prefix, 0)), shared+" parenttail")
	child := withPreview(recallEntry(sid, 1, recallRef(prefix, 1)), shared+" "+c.Query+" childtail")
	child.Depth = 1
	parentIndex := 0
	child.ParentIndex = &parentIndex
	e2 := withToolOutput(recallEntry(sid, 2, recallRef(prefix, 2)), "neutral output")
	activateRecallEntries(t, s, sid, "gen-"+prefix, []schema.SessionEntry{parent, child, e2}, nil)
	assertRecallHits(t, c, recallProduction(t, s, c.Query))
}

func recallToolRowBeyond(t *testing.T, c searchRecallCase) {
	t.Helper()
	s, _ := openGenerationStore(t)
	sid := recallSID(c.Name)
	seedGenerationSession(t, s, string(sid))
	prefix := strings.ReplaceAll(c.Name, "-", "")
	pad := strings.Repeat("t", 2200)
	long := pad + " " + c.Query + " tail"
	e0 := withPreview(recallEntry(sid, 0, recallRef(prefix, 0)), "neutral head")
	e1 := withToolInput(recallEntry(sid, 1, recallRef(prefix, 1)), "neutral input")
	e2 := withToolOutput(recallEntry(sid, 2, recallRef(prefix, 2)), long)
	e2.ContentPreview = &long
	activateRecallEntries(t, s, sid, "gen-"+prefix, []schema.SessionEntry{e0, e1, e2}, nil)
	assertRecallHits(t, c, recallProduction(t, s, c.Query))
}

func recallPiCarrierExcluded(t *testing.T, c searchRecallCase) {
	t.Helper()
	s, _ := openGenerationStore(t)
	sid := recallSID(c.Name)
	seedGenerationSession(t, s, string(sid))
	// A legacy carrier row: the writer refuses new carrier content, so the
	// row is inserted directly. The legacy postings exist; the production
	// query must exclude them.
	recallExec(t, s, `INSERT INTO session_entries(session_id, entry_index, provider, entry_type, role, content_preview, depth, part_type) VALUES (?, 0, 'claude-code', 'text', 'user', ?, 0, 'pi.carrier')`, string(sid), "carrier text "+c.Query)
	if got := recallRawCount(t, s, "session_entries_fts", c.Query); c.WantRawMatchCount != nil && got != *c.WantRawMatchCount {
		t.Fatalf("%s: raw legacy MATCH = %d, want %d", c.Name, got, *c.WantRawMatchCount)
	}
	assertRecallHits(t, c, recallProduction(t, s, c.Query))
}

func recallSupersededOnly(t *testing.T, c searchRecallCase) {
	t.Helper()
	s, _ := openGenerationStore(t)
	sid := recallSID(c.Name)
	seedGenerationSession(t, s, string(sid))
	prefix := strings.ReplaceAll(c.Name, "-", "")
	old0 := withPreview(recallEntry(sid, 0, recallRef(prefix+"-old", 0)), "old text "+c.Query)
	old1 := withToolInput(recallEntry(sid, 1, recallRef(prefix+"-old", 1)), "old input")
	old2 := withToolOutput(recallEntry(sid, 2, recallRef(prefix+"-old", 2)), "old output")
	activateRecallEntries(t, s, sid, "gen-"+prefix+"-old", []schema.SessionEntry{old0, old1, old2}, nil)
	new0 := withPreview(recallEntry(sid, 0, recallRef(prefix+"-new", 0)), "new text without the old term")
	new1 := withToolInput(recallEntry(sid, 1, recallRef(prefix+"-new", 1)), "new input")
	new2 := withToolOutput(recallEntry(sid, 2, recallRef(prefix+"-new", 2)), "new output")
	activateRecallEntries(t, s, sid, "gen-"+prefix+"-new", []schema.SessionEntry{new0, new1, new2}, nil)
	// The superseded postings remain until the sweep, but the production
	// query joins the active mapping by digest and must not return them.
	if c.WantRawMatchCount != nil {
		if got := recallRawCount(t, s, "session_search_fts", c.Query); got != *c.WantRawMatchCount {
			t.Fatalf("%s: raw MATCH = %d, want %d", c.Name, got, *c.WantRawMatchCount)
		}
	}
	assertRecallHits(t, c, recallProduction(t, s, c.Query))
}

func recallEarlierPartition(t *testing.T, c searchRecallCase) {
	t.Helper()
	s, _ := openGenerationStore(t)
	sid := recallSID(c.Name)
	seedGenerationSession(t, s, string(sid))
	prefix := strings.ReplaceAll(c.Name, "-", "")
	main := withPreview(recallEntry(sid, 0, recallRef(prefix+"-main", 0)), "main text "+c.WantReverseQuery)
	// An earlier-partition entry at the colliding index carries only the
	// earlier term. It is indexed, but the production join requires the
	// active main-partition mapping by digest and must not return it.
	earlier := withPreview(recallEntry(sid, 0, recallRef(prefix+"-earlier", 0)), "earlier text "+c.Query)
	activateRecallEntries(t, s, sid, "gen-"+prefix, []schema.SessionEntry{main}, []schema.SessionEntry{earlier})
	if c.WantRawMatchCount != nil {
		if got := recallRawCount(t, s, "session_search_fts", c.Query); got != *c.WantRawMatchCount {
			t.Fatalf("%s: raw MATCH = %d, want %d", c.Name, got, *c.WantRawMatchCount)
		}
	}
	assertRecallHits(t, c, recallProduction(t, s, c.Query))
	reverse := recallProduction(t, s, c.WantReverseQuery)
	if c.WantReverseCount != nil && len(reverse) != *c.WantReverseCount {
		t.Fatalf("%s: reverse hits = %d, want %d", c.Name, len(reverse), *c.WantReverseCount)
	}
	for i, want := range c.WantReverseEntryIndexes {
		if reverse[i].EntryIndex != want {
			t.Fatalf("%s: reverse hit %d entry = %d, want %d", c.Name, i, reverse[i].EntryIndex, want)
		}
	}
}

func recallNonNative(t *testing.T, c searchRecallCase, field string) {
	t.Helper()
	s, _ := openGenerationStore(t)
	sid := recallSID(c.Name)
	seedGenerationSession(t, s, string(sid))
	sessionID := sid
	var entries []schema.SessionEntry
	e0 := withPreview(recallEntry(sid, 0, recallRef("nonnative", 0)), "neutral head")
	e1 := withToolInput(recallEntry(sid, 1, recallRef("nonnative", 1)), "neutral input")
	switch field {
	case "preview":
		e0.ContentPreview = &c.Query
	case "input":
		e1.ToolInput = &c.Query
	}
	entries = []schema.SessionEntry{e0, e1}
	if err := s.IndexSessionEntries(context.Background(), sessionID, entries); err != nil {
		t.Fatalf("index mirror rows: %v", err)
	}
	assertRecallHits(t, c, recallProduction(t, s, c.Query))
}

func recallDeleteThenMatch(t *testing.T, c searchRecallCase) {
	t.Helper()
	s, _ := openGenerationStore(t)
	sid := recallSID(c.Name)
	seedGenerationSession(t, s, string(sid))
	prefix := strings.ReplaceAll(c.Name, "-", "")
	e0 := withPreview(recallEntry(sid, 0, recallRef(prefix, 0)), "doomed text "+c.Query)
	e1 := withToolInput(recallEntry(sid, 1, recallRef(prefix, 1)), "neutral input")
	e2 := withToolOutput(recallEntry(sid, 2, recallRef(prefix, 2)), "neutral output")
	activateRecallEntries(t, s, sid, "gen-"+prefix, []schema.SessionEntry{e0, e1, e2}, nil)
	if got := recallProduction(t, s, c.Query); len(got) != 1 {
		t.Fatalf("%s: before delete hits = %d, want 1", c.Name, len(got))
	}
	// A clean delete verifies first, so the trigger un-indexes exactly and
	// no rebuild flag is needed.
	var record EntryRecord
	func() {
		conn, err := s.pool.Take(context.Background())
		if err != nil {
			t.Fatalf("take connection: %v", err)
		}
		defer s.pool.Put(conn)
		if err := sqlitex.ExecuteTransient(conn, `SELECT `+sqlSelectBodyColumns+` FROM session_entry_bodies WHERE session_id = ?`, &sqlitex.ExecOptions{
			Args: []any{string(sid)},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				if record.BodyDigest == "" {
					record = scanEntryRecord(stmt)
				}
				return nil
			},
		}); err != nil {
			t.Fatalf("read a body: %v", err)
		}
	}()
	if !verifyBodyForDelete(record) {
		t.Fatal("healthy body fails the delete-time check")
	}
	// The sweep deletes the mapping rows before the now-unreferenced
	// bodies, so the foreign key allows the body delete and the trigger
	// un-indexes exactly.
	recallExec(t, s, `DELETE FROM session_generation_entries WHERE session_id = ?`, string(sid))
	recallExec(t, s, `DELETE FROM session_entry_bodies WHERE session_id = ?`, string(sid))
	if got := recallRawCount(t, s, "session_search_fts", c.Query); c.WantRawMatchCount != nil && got != *c.WantRawMatchCount {
		t.Fatalf("%s: raw MATCH after delete = %d, want %d", c.Name, got, *c.WantRawMatchCount)
	}
	assertRecallHits(t, c, recallProduction(t, s, c.Query))
	if state, err := s.SearchState(context.Background()); err != nil {
		t.Fatalf("read search state: %v", err)
	} else if state.NeedsRebuild {
		t.Fatal("clean delete set the rebuild flag")
	}
}

func recallPushdown(t *testing.T, c searchRecallCase) {
	t.Helper()
	s, _ := openGenerationStore(t)
	sid := recallSID(c.Name)
	seedGenerationSession(t, s, string(sid))
	prefix := strings.ReplaceAll(c.Name, "-", "")
	e0 := withPreview(recallEntry(sid, 0, recallRef(prefix, 0)), "pushdown text "+c.Query)
	activateRecallEntries(t, s, sid, "gen-"+prefix, []schema.SessionEntry{e0}, nil)
	assertRecallHits(t, c, recallProduction(t, s, c.Query))
	if !c.WantPushdown {
		return
	}
	var plan string
	func() {
		conn, err := s.pool.Take(context.Background())
		if err != nil {
			t.Fatalf("take connection: %v", err)
		}
		defer s.pool.Put(conn)
		if err := sqlitex.ExecuteTransient(conn, `EXPLAIN QUERY PLAN SELECT * FROM session_search_source WHERE rid = 1`, &sqlitex.ExecOptions{
			ResultFunc: func(stmt *sqlite.Stmt) error {
				plan += stmt.ColumnText(3) + "\n"
				return nil
			},
		}); err != nil {
			t.Fatalf("explain the union view: %v", err)
		}
	}()
	if !strings.Contains(plan, "session_entries USING INTEGER PRIMARY KEY") {
		t.Fatalf("union plan misses the mirror rowid search:\n%s", plan)
	}
	if !strings.Contains(plan, "session_entry_bodies USING INTEGER PRIMARY KEY") {
		t.Fatalf("union plan misses the body rowid search:\n%s", plan)
	}
}

func recallTransitionWindow(t *testing.T, c searchRecallCase) {
	t.Helper()
	s, _ := openGenerationStore(t)
	sid := recallSID(c.Name)
	seedGenerationSession(t, s, string(sid))
	prefix := strings.ReplaceAll(c.Name, "-", "")
	e0 := withPreview(recallEntry(sid, 0, recallRef(prefix, 0)), "transition text "+c.Query)
	activateRecallEntries(t, s, sid, "gen-"+prefix, []schema.SessionEntry{e0}, nil)
	// The short dual-representation window: the converted mirror rows are
	// still present post-commit. The representation filter drops the stale
	// side, so the unpaginated match set carries the pair exactly once.
	recallExec(t, s, `INSERT INTO session_entries(session_id, entry_index, provider, entry_type, role, content_preview, depth) VALUES (?, 0, 'claude-code', 'text', 'user', ?, 0)`, string(sid), "transition text "+c.Query)
	assertRecallHits(t, c, recallProduction(t, s, c.Query))
}

func recallTransitionFallback(t *testing.T, c searchRecallCase) {
	t.Helper()
	if !c.FallbackDualSource {
		t.Fatalf("%s: the fallback case needs its dual-source window; set fallbackDualSource", c.Name)
	}
	s, _ := openGenerationStore(t)
	sid := recallSID(c.Name)
	seedGenerationSession(t, s, string(sid))
	prefix := strings.ReplaceAll(c.Name, "-", "")
	e0 := withPreview(recallEntry(sid, 0, recallRef(prefix, 0)), "transition text "+c.Query)
	activateRecallEntries(t, s, sid, "gen-"+prefix, []schema.SessionEntry{e0}, nil)
	// The per-session-commit fallback shortens the dual window to one
	// session at a time, but a stale leftover can still sit beside a
	// genuine mirror hit from another session. Seed both: the leftover for
	// the converted session and a real mirror row elsewhere sharing the
	// term. The filter must drop the stale side and keep both live hits,
	// so a fallback path that filters the wrong side fails the count and
	// one that skips filtering fails the pair check.
	recallExec(t, s, `INSERT INTO session_entries(session_id, entry_index, provider, entry_type, role, content_preview, depth) VALUES (?, 0, 'claude-code', 'text', 'user', ?, 0)`, string(sid), "transition text "+c.Query)
	mirrorSID := recallSID(c.Name + "-mirror")
	seedGenerationSession(t, s, string(mirrorSID))
	mirrorEntry := withPreview(recallEntry(mirrorSID, 0, recallRef(prefix+"-mirror", 0)), "transition text "+c.Query)
	if err := s.IndexSessionEntries(context.Background(), mirrorSID, []schema.SessionEntry{mirrorEntry}); err != nil {
		t.Fatalf("index genuine mirror row: %v", err)
	}
	hits := recallProduction(t, s, c.Query)
	assertRecallHits(t, c, hits)
	sessions := map[string]int{}
	for _, hit := range hits {
		sessions[hit.SessionID]++
		if hit.EntryIndex != 0 {
			t.Fatalf("%s: hit entry = %d, want the index-0 pair on both sides", c.Name, hit.EntryIndex)
		}
	}
	if len(sessions) != 2 {
		t.Fatalf("%s: %d distinct sessions, want the converted session and the genuine mirror session", c.Name, len(sessions))
	}
	if sessions[string(sid)] != 1 || sessions[string(mirrorSID)] != 1 {
		t.Fatalf("%s: per-session hits = %v, want exactly one pair per side", c.Name, sessions)
	}
}

func recallRowidGuard(t *testing.T, c searchRecallCase) {
	t.Helper()
	s, _ := openGenerationStore(t)
	sid := recallSID(c.Name)
	seedGenerationSession(t, s, string(sid))
	prefix := strings.ReplaceAll(c.Name, "-", "")
	e0 := withPreview(recallEntry(sid, 0, recallRef(prefix, 0)), "rowid text "+c.Query)
	activateRecallEntries(t, s, sid, "gen-"+prefix, []schema.SessionEntry{e0}, nil)
	assertRecallHits(t, c, recallProduction(t, s, c.Query))
	var firstBody int64
	func() {
		conn, err := s.pool.Take(context.Background())
		if err != nil {
			t.Fatalf("take connection: %v", err)
		}
		defer s.pool.Put(conn)
		if err := sqlitex.ExecuteTransient(conn, `SELECT min(body_id) FROM session_entry_bodies`, &sqlitex.ExecOptions{
			ResultFunc: func(stmt *sqlite.Stmt) error { firstBody = stmt.ColumnInt64(0); return nil },
		}); err != nil {
			t.Fatalf("read first body_id: %v", err)
		}
	}()
	if c.WantFirstBodyID != nil && firstBody != *c.WantFirstBodyID {
		t.Fatalf("first body_id = %d, want %d", firstBody, *c.WantFirstBodyID)
	}
	var maxMirror int64
	func() {
		conn, err := s.pool.Take(context.Background())
		if err != nil {
			t.Fatalf("take connection: %v", err)
		}
		defer s.pool.Put(conn)
		if err := sqlitex.ExecuteTransient(conn, `SELECT coalesce(max(rowid), 0) FROM session_entries`, &sqlitex.ExecOptions{
			ResultFunc: func(stmt *sqlite.Stmt) error { maxMirror = stmt.ColumnInt64(0); return nil },
		}); err != nil {
			t.Fatalf("read max mirror rowid: %v", err)
		}
	}()
	if maxMirror >= BodyRowIDBase {
		t.Fatalf("max(session_entries.rowid) = %d reaches the body base", maxMirror)
	}
	if err := s.CheckSessionEntriesRowidCeiling(context.Background()); err != nil {
		t.Fatalf("ceiling check refuses a healthy store: %v", err)
	}
	if c.WantCeilingRefused {
		recallExec(t, s, `INSERT INTO session_entries(rowid, session_id, entry_index, provider, entry_type, role, depth) VALUES (?, ?, 0, 'claude-code', 'text', 'user', 0)`, int64(BodyRowIDBase), string(sid))
		if err := s.CheckSessionEntriesRowidCeiling(context.Background()); err == nil {
			t.Fatal("ceiling check allows max(session_entries.rowid) at the body base")
		}
	}
}
