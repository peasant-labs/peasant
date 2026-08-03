package codemap_test

import (
	"context"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// Search is global FTS5 over the seeded activity fixture. The fixture's user
// turns carry known text:
//
//	session1 task@0 "Add caching to the ingest pipeline"
//	session1 task@4 "Refactor the ingest pipeline so the diff classifier ..."
//	session2 task@0 "fix the flaky b tests"
//
// so "pipeline" hits two entries (both in session1), "caching" exactly one,
// and "flaky" the session2 turn.

func TestSearch_FindsMatchingEntries(t *testing.T) {
	t.Parallel()
	svc, _ := newFixtureService(t, fxStubRepo())
	ctx := context.Background()

	got, err := svc.Search(ctx, "caching", 20)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got.Query != "caching" {
		t.Errorf("Query = %q, want %q", got.Query, "caching")
	}
	if len(got.Results) != 1 {
		t.Fatalf("results for 'caching' = %d, want 1: %+v", len(got.Results), got.Results)
	}
	r := got.Results[0]
	if r.SessionID != fxSession1 {
		t.Errorf("sessionId = %q, want %q", r.SessionID, fxSession1)
	}
	if r.EntryIndex != 0 {
		t.Errorf("entryIndex = %d, want 0 (task@0)", r.EntryIndex)
	}
	if r.Role != string(schema.RoleUser) {
		t.Errorf("role = %q, want %q", r.Role, schema.RoleUser)
	}
	if r.Project != fxCwd {
		t.Errorf("project = %q, want %q (raw canonical_cwd)", r.Project, fxCwd)
	}
	if r.ProjectHash != fxProjectHash {
		t.Errorf("projectHash = %q, want %q", r.ProjectHash, fxProjectHash)
	}
	if !strings.Contains(r.Snippet, "[") || !strings.Contains(r.Snippet, "]") {
		t.Errorf("snippet missing FTS markers: %q", r.Snippet)
	}
	if r.Score <= 0 {
		t.Errorf("score = %v, want > 0 (negated bm25)", r.Score)
	}
}

func TestSearch_MultiWordImplicitAnd(t *testing.T) {
	t.Parallel()
	svc, _ := newFixtureService(t, fxStubRepo())
	ctx := context.Background()

	// Both terms appear in session1 entries 0 and 4.
	got, err := svc.Search(ctx, "ingest pipeline", 20)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got.Results) != 2 {
		t.Fatalf("results for 'ingest pipeline' = %d, want 2: %+v", len(got.Results), got.Results)
	}
	for _, r := range got.Results {
		if r.SessionID != fxSession1 {
			t.Errorf("unexpected session %q in results", r.SessionID)
		}
	}

	// "caching" only co-occurs with "pipeline" in entry 0.
	got, err = svc.Search(ctx, "caching pipeline", 20)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got.Results) != 1 || got.Results[0].EntryIndex != 0 {
		t.Fatalf("results for 'caching pipeline' = %+v, want single entry@0", got.Results)
	}
}

func TestSearch_RankedBestFirst(t *testing.T) {
	t.Parallel()
	svc, _ := newFixtureService(t, fxStubRepo())
	ctx := context.Background()

	got, err := svc.Search(ctx, "pipeline", 20)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got.Results) < 2 {
		t.Fatalf("results for 'pipeline' = %d, want >= 2", len(got.Results))
	}
	// Result order is authoritative: scores are non-increasing.
	for i := 1; i < len(got.Results); i++ {
		if got.Results[i].Score > got.Results[i-1].Score {
			t.Errorf("results not ranked best-first: score[%d]=%v > score[%d]=%v",
				i, got.Results[i].Score, i-1, got.Results[i-1].Score)
		}
	}
}

func TestSearch_ShortQueryReturnsEmpty(t *testing.T) {
	t.Parallel()
	svc, _ := newFixtureService(t, fxStubRepo())
	ctx := context.Background()

	for _, q := range []string{"", " ", "a", "  z "} {
		got, err := svc.Search(ctx, q, 20)
		if err != nil {
			t.Fatalf("Search(%q): %v", q, err)
		}
		if got.Query != q {
			t.Errorf("Search(%q): Query = %q", q, got.Query)
		}
		if len(got.Results) != 0 {
			t.Errorf("Search(%q): results = %d, want 0 (short-circuit)", q, len(got.Results))
		}
	}
}

func TestSearch_RawFTSOperatorsNeverError(t *testing.T) {
	t.Parallel()
	svc, _ := newFixtureService(t, fxStubRepo())
	ctx := context.Background()

	// Each of these is an FTS5 syntax error if passed to MATCH unsanitized
	// (unbalanced quote, leading/dangling operators, bare special chars). The
	// sanitizer must quote every token so MATCH never errors.
	for _, q := range []string{
		`pipeline OR (`,
		`"unterminated`,
		`cach"ing`,
		`NEAR(pipeline`,
		`pipeline AND`,
		`* pipeline`,
		`pipeline:`,
		`* ()`,    // pure punctuation: every token tokenizes to nothing
		`-- (){}`, // ditto, no letters/digits anywhere
		`""`,
	} {
		got, err := svc.Search(ctx, q, 20)
		if err != nil {
			t.Errorf("Search(%q) errored, sanitizer should prevent FTS syntax errors: %v", q, err)
		}
		if got == nil || got.Results == nil {
			t.Errorf("Search(%q): payload/Results must be non-nil", q)
		}
	}

	// An all-punctuation query yields no usable tokens → empty results, never
	// an FTS error.
	got, err := svc.Search(ctx, `* ()`, 20)
	if err != nil {
		t.Fatalf("Search(all-punct): %v", err)
	}
	if len(got.Results) != 0 {
		t.Errorf("all-punctuation query: results = %d, want 0", len(got.Results))
	}
}

// TestSearch_NoEchoDuplicates guards the full-depth echo: production indexers
// store a message's text on BOTH the depth=0 parent (content_preview) and its
// depth=1 text child, so the FTS index holds two rows with identical text. A
// single logical message must return exactly one search hit, not two.
func TestSearch_NoEchoDuplicates(t *testing.T) {
	t.Parallel()
	svc, s := newFixtureService(t, fxStubRepo())
	ctx := context.Background()

	sid := "55555555-5555-5555-5555-555555555555"
	base := fxBase()
	seedSession(t, s, sid, "", base+6000, base+7000)

	sessionID := schema.SessionID(sid)
	echo := "zorptastic singular keyword"
	parentIdx := 0
	mk := func(idx, depth int, preview string) schema.SessionEntry {
		p := preview
		e := schema.SessionEntry{
			SessionID:      sessionID,
			EntryIndex:     idx,
			Harness:        ingest.HarnessClaudeCode,
			EntryType:      ingest.EntryTypeText,
			Role:           ingest.RoleAssistant,
			Depth:          depth,
			ContentPreview: &p,
		}
		if depth > 0 {
			e.ParentIndex = &parentIdx // points at the depth=0 parent (entry 0)
		}
		return e
	}
	// Parent (depth=0) + its echo text child (depth=1, same preview) — exactly
	// what claude_indexer emits for a single-text-block message.
	if err := s.IndexSessionEntries(ctx, sessionID, []schema.SessionEntry{
		mk(0, 0, echo),
		mk(1, 1, echo),
	}); err != nil {
		t.Fatalf("index echo pair: %v", err)
	}

	got, err := svc.Search(ctx, "zorptastic", 20)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got.Results) != 1 {
		t.Fatalf("echo'd message returned %d results, want 1 (dedup): %+v", len(got.Results), got.Results)
	}
	// The surviving hit is the deep-linkable depth=0 parent (entry_index 0).
	if got.Results[0].EntryIndex != 0 {
		t.Errorf("surviving hit entryIndex = %d, want 0 (the depth=0 parent)", got.Results[0].EntryIndex)
	}
}

// TestSearch_UniqueMultiBlockTextSurvives proves the dedup is surgical: a
// depth>0 text block that is NOT an echo of its parent (e.g. a second text
// block, whose text the parent's first-block preview never contains) is still
// searchable.
func TestSearch_UniqueMultiBlockTextSurvives(t *testing.T) {
	t.Parallel()
	svc, s := newFixtureService(t, fxStubRepo())
	ctx := context.Background()

	sid := "66666666-6666-6666-6666-666666666666"
	base := fxBase()
	seedSession(t, s, sid, "", base+8000, base+9000)

	sessionID := schema.SessionID(sid)
	parentIdx := 0
	firstBlock := "alpha intro text"
	secondBlock := "omega distinct conclusion"
	p0, p1, p2 := firstBlock, firstBlock, secondBlock
	if err := s.IndexSessionEntries(ctx, sessionID, []schema.SessionEntry{
		{SessionID: sessionID, EntryIndex: 0, Harness: ingest.HarnessClaudeCode, EntryType: ingest.EntryTypeText, Role: ingest.RoleAssistant, Depth: 0, ContentPreview: &p0},
		{SessionID: sessionID, EntryIndex: 1, Harness: ingest.HarnessClaudeCode, EntryType: ingest.EntryTypeText, Role: ingest.RoleAssistant, Depth: 1, ParentIndex: &parentIdx, ContentPreview: &p1}, // echo of parent → dropped
		{SessionID: sessionID, EntryIndex: 2, Harness: ingest.HarnessClaudeCode, EntryType: ingest.EntryTypeText, Role: ingest.RoleAssistant, Depth: 1, ParentIndex: &parentIdx, ContentPreview: &p2}, // unique 2nd block → kept
	}); err != nil {
		t.Fatalf("index multi-block: %v", err)
	}

	// The unique second block is findable...
	if got, err := svc.Search(ctx, "omega", 20); err != nil {
		t.Fatalf("Search omega: %v", err)
	} else if len(got.Results) != 1 {
		t.Errorf("'omega' (unique 2nd block) = %d results, want 1: %+v", len(got.Results), got.Results)
	}
	// ...and the echoed first block still collapses to one hit.
	if got, err := svc.Search(ctx, "alpha", 20); err != nil {
		t.Fatalf("Search alpha: %v", err)
	} else if len(got.Results) != 1 {
		t.Errorf("'alpha' (echoed 1st block) = %d results, want 1: %+v", len(got.Results), got.Results)
	}
}

func TestSearch_LimitClamped(t *testing.T) {
	t.Parallel()
	svc, _ := newFixtureService(t, fxStubRepo())
	ctx := context.Background()

	got, err := svc.Search(ctx, "pipeline", 1)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got.Results) != 1 {
		t.Errorf("limit=1: results = %d, want 1", len(got.Results))
	}

	// limit<=0 falls back to the default (>= our 2 matches), not zero.
	got, err = svc.Search(ctx, "pipeline", 0)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got.Results) != 2 {
		t.Errorf("limit=0: results = %d, want 2 (default applied)", len(got.Results))
	}
}
