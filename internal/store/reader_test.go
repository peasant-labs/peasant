package store_test

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/schema"
)

// ---------------------------------------------------------------------------
// C1: FirstUserMessageBulk — parity with per-row FirstUserMessage
// ---------------------------------------------------------------------------

// seedUserEntry inserts a session_entries row with role=user and content_preview set,
// at the given entryIndex. The session row must already exist.
func seedUserEntry(t *testing.T, s *store.Store, sessionID string, entryIndex int, content string) {
	t.Helper()
	cp := content
	entries := []schema.SessionEntry{
		{
			SessionID:      schema.SessionID(sessionID),
			EntryIndex:     entryIndex,
			Harness:        defaults.HarnessClaudeCode,
			EntryType:      schema.EntryTypeText,
			Role:           schema.RoleUser,
			ContentPreview: &cp,
			Depth:          0,
		},
	}
	if err := s.IndexSessionEntries(context.Background(), schema.SessionID(sessionID), entries); err != nil {
		t.Fatalf("seedUserEntry(%q, %d): %v", sessionID, entryIndex, err)
	}
}

// TestFirstUserMessageBulk_ParityWithSingleRow verifies that FirstUserMessageBulk
// returns the same previews as per-row FirstUserMessage for every session, including
// the omit-missing case where a session has no user entry.
func TestFirstUserMessageBulk_ParityWithSingleRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := storetest.Open(t)

	// Seed three sessions.
	const (
		sid1 = "11111111-1111-1111-1111-111111111111"
		sid2 = "22222222-2222-2222-2222-222222222222"
		sid3 = "33333333-3333-3333-3333-333333333333" // no user entry
	)
	storetest.SeedSession(t, s, sid1)
	storetest.SeedSession(t, s, sid2)
	storetest.SeedSession(t, s, sid3)

	seedUserEntry(t, s, sid1, 0, "hello from session 1")
	seedUserEntry(t, s, sid2, 0, "hello from session 2")
	// sid3: no user entry — should be absent from the bulk map

	sessionIDs := []string{sid1, sid2, sid3}

	// Per-row baseline.
	perRow := map[string]string{}
	for _, id := range sessionIDs {
		preview, err := s.FirstUserMessage(ctx, id)
		if err != nil {
			t.Fatalf("FirstUserMessage(%q): %v", id, err)
		}
		if preview != "" {
			perRow[id] = preview
		}
	}

	// Bulk call.
	bulk, err := s.FirstUserMessageBulk(ctx, sessionIDs)
	if err != nil {
		t.Fatalf("FirstUserMessageBulk: %v", err)
	}

	// Bulk result must match per-row for all sessions that have a user entry.
	for id, want := range perRow {
		got, ok := bulk[id]
		if !ok {
			t.Errorf("bulk missing key %q (expected preview %q)", id, want)
			continue
		}
		if got != want {
			t.Errorf("bulk[%q] = %q, want %q", id, got, want)
		}
	}

	// sid3 must be absent from bulk (no user entry → omit).
	if _, ok := bulk[sid3]; ok {
		t.Errorf("bulk[%q]: expected absent (no user entry), got %q", sid3, bulk[sid3])
	}

	// Bulk must not contain unexpected extra keys.
	for id := range bulk {
		if _, ok := perRow[id]; !ok {
			t.Errorf("bulk has unexpected extra key %q", id)
		}
	}
}

// TestFirstUserMessageBulk_Empty verifies that an empty sessionIDs slice returns
// an empty map without error.
func TestFirstUserMessageBulk_Empty(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := storetest.Open(t)

	bulk, err := s.FirstUserMessageBulk(ctx, []string{})
	if err != nil {
		t.Fatalf("FirstUserMessageBulk([]): %v", err)
	}
	if len(bulk) != 0 {
		t.Errorf("expected empty map, got %v", bulk)
	}
}

// TestFirstUserMessageBulk_Truncation verifies that bulk previews are truncated to
// the same cap as per-row FirstUserMessage (SessionPreviewMaxChars runes).
func TestFirstUserMessageBulk_Truncation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := storetest.Open(t)

	const sid = "44444444-4444-4444-4444-444444444444"
	storetest.SeedSession(t, s, sid)

	// Build a string longer than SessionPreviewMaxChars (40 chars).
	long := ""
	for i := 0; i < defaults.SessionPreviewMaxChars*3; i++ {
		long += "x"
	}
	seedUserEntry(t, s, sid, 0, long)

	single, err := s.FirstUserMessage(ctx, sid)
	if err != nil {
		t.Fatalf("FirstUserMessage: %v", err)
	}
	bulk, err := s.FirstUserMessageBulk(ctx, []string{sid})
	if err != nil {
		t.Fatalf("FirstUserMessageBulk: %v", err)
	}

	if bulk[sid] != single {
		t.Errorf("truncation parity: bulk=%q single=%q", bulk[sid], single)
	}
	runeLen := len([]rune(bulk[sid]))
	if runeLen > defaults.SessionPreviewMaxChars {
		t.Errorf("bulk preview exceeds cap: got %d runes, cap %d", runeLen, defaults.SessionPreviewMaxChars)
	}
}

// ---------------------------------------------------------------------------
// D2: CountSessionsFiltered == len(ListSessionsFiltered) across filter matrix
// ---------------------------------------------------------------------------

// makeFilteredSession seeds a minimal session with controllable harness and project name.
// projectHash must be a unique 64-hex-char string per session to avoid FK collisions.
func makeFilteredSession(t *testing.T, s *store.Store, sessionID, projectName, projectHash string, h defaults.Harness, startMs int64) {
	t.Helper()
	ingestedMs := startMs + 10
	entry := ingest.StoreEntry{
		Metadata: &schema.UnifiedMetadata{
			SessionID:    schema.SessionID(sessionID),
			ModelHarness: h,
			Model:        schema.ModelID("test-model"),
			HostSlug:     schema.HostSlug("testslug"),
			Project: schema.ProjectContext{
				Hash:     schema.ProjectHash(projectHash),
				Name:     projectName,
				FilePath: "/" + projectName,
			},
			Timestamp: schema.TimestampInfo{Start: startMs, End: startMs + 1, Ingested: &ingestedMs},
			Source:    schema.SourceInfo{FilePath: "/f", Format: schema.SourceFormatJSONL},
		},
	}
	if err := s.InsertSessions(context.Background(), []ingest.StoreEntry{entry}); err != nil {
		t.Fatalf("makeFilteredSession(%q): %v", sessionID, err)
	}
}

func TestCountSessionsFiltered_EqualsListLen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := storetest.Open(t)

	// Seed a variety of sessions across two harnesses and two project names.
	// Each session gets a unique 64-hex-char project hash (projects.project_hash PK).
	const (
		sidA1  = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
		sidA2  = "aaaaaaaa-aaaa-aaaa-aaaa-bbbbbbbbbbbb"
		sidB1  = "bbbbbbbb-bbbb-bbbb-bbbb-aaaaaaaaaaaa"
		sidB2  = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
		hashA1 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		hashA2 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaabb"
		hashB1 = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		hashB2 = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbaa"
	)
	makeFilteredSession(t, s, sidA1, "alpha", hashA1, defaults.HarnessClaudeCode, 1_000_000)
	makeFilteredSession(t, s, sidA2, "alpha", hashA2, defaults.HarnessClaudeCode, 2_000_000)
	makeFilteredSession(t, s, sidB1, "beta", hashB1, defaults.HarnessOpenCode, 3_000_000)
	makeFilteredSession(t, s, sidB2, "beta", hashB2, defaults.HarnessOpenCode, 4_000_000)

	harnessCC := string(defaults.HarnessClaudeCode)
	harnessOC := string(defaults.HarnessOpenCode)
	projAlpha := "alpha"
	projBeta := "beta"
	var since1 int64 = 1_500_000
	var until3 int64 = 3_500_000

	tests := []struct {
		name string
		f    store.SessionListFilter
	}{
		{"no-filter", store.SessionListFilter{}},
		{"harness-cc", store.SessionListFilter{SessionFilter: store.SessionFilter{ModelHarness: &harnessCC}}},
		{"harness-oc", store.SessionListFilter{SessionFilter: store.SessionFilter{ModelHarness: &harnessOC}}},
		{"project-alpha", store.SessionListFilter{ProjectName: &projAlpha}},
		{"project-beta", store.SessionListFilter{ProjectName: &projBeta}},
		{"since-1.5M", store.SessionListFilter{SessionFilter: store.SessionFilter{StartFrom: &since1}}},
		{"until-3.5M", store.SessionListFilter{SessionFilter: store.SessionFilter{StartBefore: &until3}}},
		{"since+until", store.SessionListFilter{SessionFilter: store.SessionFilter{StartFrom: &since1, StartBefore: &until3}}},
		{"harness+project", store.SessionListFilter{SessionFilter: store.SessionFilter{ModelHarness: &harnessCC}, ProjectName: &projAlpha}},
		{"harness+project+since", store.SessionListFilter{
			SessionFilter: store.SessionFilter{ModelHarness: &harnessCC, StartFrom: &since1},
			ProjectName:   &projAlpha,
		}},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rows, err := s.ListSessionsFiltered(ctx, tc.f)
			if err != nil {
				t.Fatalf("ListSessionsFiltered: %v", err)
			}
			wantCount := len(rows)

			gotCount, err := s.CountSessionsFiltered(ctx, tc.f)
			if err != nil {
				t.Fatalf("CountSessionsFiltered: %v", err)
			}
			if gotCount != wantCount {
				t.Errorf("CountSessionsFiltered=%d, want %d (len(ListSessionsFiltered))", gotCount, wantCount)
			}
		})
	}
}

// TestCountSessionsFiltered_IgnoresLimit verifies that CountSessionsFiltered with Limit>0
// returns the total count (not limited to Limit).
func TestCountSessionsFiltered_IgnoresLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := storetest.Open(t)

	// Seed 3 sessions.
	for _, id := range []string{
		"cccccccc-cccc-cccc-cccc-aaaaaaaaaaaa",
		"cccccccc-cccc-cccc-cccc-bbbbbbbbbbbb",
		"cccccccc-cccc-cccc-cccc-cccccccccccc",
	} {
		storetest.SeedSession(t, s, id)
	}

	// List without limit to establish baseline.
	all, err := s.ListSessionsFiltered(ctx, store.SessionListFilter{})
	if err != nil {
		t.Fatalf("ListSessionsFiltered: %v", err)
	}
	total := len(all)
	if total < 3 {
		t.Fatalf("expected at least 3 sessions, got %d", total)
	}

	// CountSessionsFiltered with Limit=1 must still return total.
	limited := store.SessionListFilter{Limit: 1}
	count, err := s.CountSessionsFiltered(ctx, limited)
	if err != nil {
		t.Fatalf("CountSessionsFiltered: %v", err)
	}
	if count != total {
		t.Errorf("CountSessionsFiltered(Limit=1)=%d, want total %d", count, total)
	}
}
