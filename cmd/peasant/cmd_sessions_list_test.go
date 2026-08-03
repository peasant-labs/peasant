package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/schema"
)

// seedOpts controls how seedTestSessionFull and mustSeedMany behave.
type seedOpts struct {
	SessionID    string
	Harness      defaults.Harness
	ModelID      string
	StartMs      int64
	ProjectPath  string
	ProjectHash  string
	ProjectName  string
	GitRemote    *string
	TurnCount    int
	TokensIn     int
	TokensOut    int
	FirstUserMsg string
}

// applyDefaults fills empty seedOpts fields with deterministic defaults so
// callers only have to set the fields their test actually cares about.
func (opts seedOpts) applyDefaults() seedOpts {
	if opts.Harness == "" {
		opts.Harness = defaults.HarnessClaudeCode
	}
	if opts.ModelID == "" {
		opts.ModelID = "claude-opus-4-6"
	}
	if opts.ProjectPath == "" {
		opts.ProjectPath = "/home/user/myproject"
	}
	if opts.ProjectHash == "" {
		opts.ProjectHash = "testhash0000000000000000000000000000000000000000000000000000000"
	}
	return opts
}

// storeEntryFor builds the ingest.StoreEntry for a seeded session.
// Assumes opts has been passed through applyDefaults.
func (opts seedOpts) storeEntryFor() ingest.StoreEntry {
	ingested := opts.StartMs
	endMs := opts.StartMs + 60_000
	return ingest.StoreEntry{
		Metadata: &schema.UnifiedMetadata{
			SchemaVersion: schema.MetadataSchemaVersion,
			SessionID:     schema.SessionID(opts.SessionID),
			ModelHarness:  opts.Harness,
			Model:         schema.ModelID(opts.ModelID),
			HostSlug:      schema.HostSlug("github.com--test--repo"),
			Timestamp: schema.TimestampInfo{
				Start:    opts.StartMs,
				End:      endMs,
				Ingested: &ingested,
			},
			Project: schema.ProjectContext{
				Hash:     schema.ProjectHash(opts.ProjectHash),
				Name:     opts.ProjectName,
				FilePath: opts.ProjectPath,
			},
			Source: schema.SourceInfo{
				Format: schema.SourceFormatJSONL,
			},
			Git: schema.GitContext{
				Remote: opts.GitRemote,
			},
			Stats: schema.SessionStats{
				TurnCount:     opts.TurnCount,
				ToolCallCount: 0,
				TokensIn:      opts.TokensIn,
				TokensOut:     opts.TokensOut,
			},
		},
	}
}

// firstUserEntryFor builds the first-user-message session_entries row, or nil
// when opts.FirstUserMsg is empty.
func (opts seedOpts) firstUserEntryFor() []schema.SessionEntry {
	if opts.FirstUserMsg == "" {
		return nil
	}
	return []schema.SessionEntry{
		{
			SessionID:      schema.SessionID(opts.SessionID),
			EntryIndex:     0,
			Harness:        opts.Harness,
			EntryType:      schema.EntryTypeText,
			Role:           schema.RoleUser,
			ContentPreview: &opts.FirstUserMsg,
		},
	}
}

// openSeedStore opens the store under the given data-dir override (dir, the same
// value passed as --data-dir to the command), copying the golden DB into place
// only if the DB file does not yet exist. Guarding the copy prevents redundant re-copies
// (and data loss) when openSeedStore is called more than once within the same test that
// shares the same dir. Caller must close the returned store.
func openSeedStore(t *testing.T, dir, name string) *store.Store {
	t.Helper()
	dataDir := string(defaults.ResolveDataDirPathWith(dir))
	dbPath := string(defaults.ResolveDBFilePathWith(dir))
	if err := os.MkdirAll(dataDir, defaults.PrivateDirPerm); err != nil {
		t.Fatalf("%s: create data dir: %v", name, err)
	}
	// Only copy the golden DB if the target file does not already exist.
	// This guards against clobbering previously seeded data when openSeedStore
	// is invoked multiple times within a single test pointing at the same path.
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		storetest.CopyGoldenTo(t, dbPath)
	}
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("%s: open store: %v", name, err)
	}
	return db
}

// seedOne inserts a single session (with optional first-user entry) into the store.
func seedOne(t *testing.T, ctx context.Context, db *store.Store, name string, opts seedOpts) {
	t.Helper()
	opts = opts.applyDefaults()
	if err := db.InsertSessions(ctx, []ingest.StoreEntry{opts.storeEntryFor()}); err != nil {
		t.Fatalf("%s: insert session %q: %v", name, opts.SessionID, err)
	}
	if entries := opts.firstUserEntryFor(); entries != nil {
		if err := db.IndexSessionEntries(ctx, schema.SessionID(opts.SessionID), entries); err != nil {
			t.Fatalf("%s: index entries for %q: %v", name, opts.SessionID, err)
		}
	}
}

// seedTestSessionFull seeds a session with extended project and entry information.
// It allows specifying canonical_remote (via git remote URL) and canonical_cwd for
// testing project filtering. It also optionally seeds session_entries for preview.
func seedTestSessionFull(t *testing.T, dir string, opts seedOpts) {
	t.Helper()
	db := openSeedStore(t, dir, "seedTestSessionFull")
	defer db.Close()
	seedOne(t, context.Background(), db, "seedTestSessionFull", opts)
}

// mustSeedMany seeds multiple sessions for list tests. Sessions are seeded with
// decreasing start timestamps so the newest-first order is predictable.
func mustSeedMany(t *testing.T, dir string, sessions []seedOpts) {
	t.Helper()
	db := openSeedStore(t, dir, "mustSeedMany")
	defer db.Close()
	ctx := context.Background()
	for _, opts := range sessions {
		seedOne(t, ctx, db, "mustSeedMany", opts)
	}
}

// TestCLI_SessionsList_TableOutput verifies that peasant sessions list produces
// a table with expected columns for seeded sessions.
func TestCLI_SessionsList_TableOutput(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	now := time.Now().UnixMilli()
	seedTestSessionFull(t, dir, seedOpts{
		SessionID:    testSessionUUID,
		StartMs:      now,
		FirstUserMsg: "Hello, implement a login feature",
	})

	output, err := executeSessionsCmd(t, dir, []string{"list"})
	if err != nil {
		t.Fatalf("sessions list: unexpected error: %v\noutput: %s", err, output)
	}

	// Should contain 8-char prefix of session ID.
	idPrefix := testSessionUUID[:8]
	if !strings.Contains(output, idPrefix) {
		t.Errorf("expected session ID prefix %q in output; got:\n%s", idPrefix, output)
	}
	// Preview should be present (truncated to 40 chars).
	if !strings.Contains(output, "Hello, implement") {
		t.Errorf("expected first user message preview in output; got:\n%s", output)
	}
}

// TestCLI_SessionsList_DefaultLimit verifies the default limit of 20 sessions.
func TestCLI_SessionsList_DefaultLimit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Seed 25 sessions with different IDs.
	uuids := []string{
		"00000000-0000-0000-0000-000000000001",
		"00000000-0000-0000-0000-000000000002",
		"00000000-0000-0000-0000-000000000003",
		"00000000-0000-0000-0000-000000000004",
		"00000000-0000-0000-0000-000000000005",
		"00000000-0000-0000-0000-000000000006",
		"00000000-0000-0000-0000-000000000007",
		"00000000-0000-0000-0000-000000000008",
		"00000000-0000-0000-0000-000000000009",
		"00000000-0000-0000-0000-000000000010",
		"00000000-0000-0000-0000-000000000011",
		"00000000-0000-0000-0000-000000000012",
		"00000000-0000-0000-0000-000000000013",
		"00000000-0000-0000-0000-000000000014",
		"00000000-0000-0000-0000-000000000015",
		"00000000-0000-0000-0000-000000000016",
		"00000000-0000-0000-0000-000000000017",
		"00000000-0000-0000-0000-000000000018",
		"00000000-0000-0000-0000-000000000019",
		"00000000-0000-0000-0000-000000000020",
		"00000000-0000-0000-0000-000000000021",
		"00000000-0000-0000-0000-000000000022",
		"00000000-0000-0000-0000-000000000023",
		"00000000-0000-0000-0000-000000000024",
		"00000000-0000-0000-0000-000000000025",
	}
	opts := make([]seedOpts, len(uuids))
	for i, uid := range uuids {
		opts[i] = seedOpts{
			SessionID: uid,
			StartMs:   int64(i+1) * 1000,
		}
	}
	mustSeedMany(t, dir, opts)

	output, err := executeSessionsCmd(t, dir, []string{"list"})
	if err != nil {
		t.Fatalf("sessions list default limit: unexpected error: %v\noutput: %s", err, output)
	}

	// The footer should appear when there are more than the limit.
	if !strings.Contains(output, "20 of 25") {
		t.Errorf("expected '20 of 25' footer; got:\n%s", output)
	}
}

// TestCLI_SessionsList_JSON verifies that --json produces a valid JSON array.
func TestCLI_SessionsList_JSON(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	now := time.Now().UnixMilli()
	seedTestSessionFull(t, dir, seedOpts{
		SessionID: testSessionUUID,
		StartMs:   now,
	})

	output, err := executeSessionsCmd(t, dir, []string{"list", "--json"})
	if err != nil {
		t.Fatalf("sessions list --json: unexpected error: %v\noutput: %s", err, output)
	}

	var result []map[string]interface{}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("sessions list --json: invalid JSON: %v\noutput: %s", err, output)
	}
	if len(result) == 0 {
		t.Error("sessions list --json: expected at least one session in array")
	}
	// Verify expected keys are present.
	for _, key := range []string{"id", "date", "project", "turns", "tokens", "preview"} {
		if _, ok := result[0][key]; !ok {
			t.Errorf("sessions list --json: missing key %q in first object; keys: %v", key, keysOf(result[0]))
		}
	}
}

// TestCLI_SessionsList_ProjectFilter verifies --project filters by canonical_remote LIKE.
func TestCLI_SessionsList_ProjectFilter(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	remote1 := "https://github.com/myorg/myrepo.git"
	remote2 := "https://github.com/otherorg/otherrepo.git"

	now := time.Now().UnixMilli()
	mustSeedMany(t, dir, []seedOpts{
		{
			SessionID:   "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
			StartMs:     now,
			ProjectHash: "aaaa0000000000000000000000000000000000000000000000000000000000aa",
			ProjectPath: "/home/user/myrepo",
			GitRemote:   &remote1,
		},
		{
			SessionID:   "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
			StartMs:     now - 1000,
			ProjectHash: "bbbb0000000000000000000000000000000000000000000000000000000000bb",
			ProjectPath: "/home/user/otherrepo",
			GitRemote:   &remote2,
		},
	})

	// Filter by "myrepo" — should match canonical_remote LIKE '%myrepo%'.
	output, err := executeSessionsCmd(t, dir, []string{"list", "--project", "myrepo"})
	if err != nil {
		t.Fatalf("sessions list --project myrepo: unexpected error: %v\noutput: %s", err, output)
	}
	if !strings.Contains(output, "aaaaaaaa"[:8]) {
		t.Errorf("expected aaaaaaaa session in output; got:\n%s", output)
	}
	if strings.Contains(output, "bbbbbbbb"[:8]) {
		t.Errorf("expected bbbbbbbb session NOT in output; got:\n%s", output)
	}
}

// TestCLI_SessionsList_ProjectFilter_CWDFallback verifies --project falls back to
// basename(canonical_cwd) when canonical_remote doesn't match.
func TestCLI_SessionsList_ProjectFilter_CWDFallback(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	now := time.Now().UnixMilli()
	// No git remote — should match by cwd basename.
	mustSeedMany(t, dir, []seedOpts{
		{
			SessionID:   "cccccccc-cccc-cccc-cccc-cccccccccccc",
			StartMs:     now,
			ProjectHash: "cccc0000000000000000000000000000000000000000000000000000000000cc",
			ProjectPath: "/home/user/specialproject",
		},
		{
			SessionID:   "dddddddd-dddd-dddd-dddd-dddddddddddd",
			StartMs:     now - 1000,
			ProjectHash: "dddd0000000000000000000000000000000000000000000000000000000000dd",
			ProjectPath: "/home/user/otherproject",
		},
	})

	output, err := executeSessionsCmd(t, dir, []string{"list", "--project", "specialproject"})
	if err != nil {
		t.Fatalf("sessions list --project specialproject: unexpected error: %v\noutput: %s", err, output)
	}
	if !strings.Contains(output, "cccccccc"[:8]) {
		t.Errorf("expected cccccccc session in output; got:\n%s", output)
	}
	if strings.Contains(output, "dddddddd"[:8]) {
		t.Errorf("expected dddddddd session NOT in output; got:\n%s", output)
	}
}

// TestCLI_SessionsList_ProjectFilter_NonMatchingRemoteBasenameMatch verifies that the
// basename fallback triggers even when a git remote is present, as long as the remote
// does NOT match the --project value.
//
// Filter logic (reader.go:607-618):
//
//	canonical_remote LIKE '%p%'
//	OR (
//	    (canonical_remote IS NULL OR canonical_remote NOT LIKE '%p%')
//	    AND basename(canonical_cwd) == p
//	)
//
// Scenario: a session has remote="https://github.com/otherorg/otherrepo.git" (does NOT
// contain "specialproject") and cwd="/home/user/specialproject" (basename DOES match).
// Expected outcome: the session MATCHES --project specialproject via the basename fallback,
// even though a remote is present.
func TestCLI_SessionsList_ProjectFilter_NonMatchingRemoteBasenameMatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// A remote that does NOT contain the project name "specialproject".
	nonMatchingRemote := "https://github.com/otherorg/otherrepo.git"

	now := time.Now().UnixMilli()
	mustSeedMany(t, dir, []seedOpts{
		{
			// This session has a NON-matching remote but a matching cwd basename.
			// Expected: MATCHES via basename fallback.
			SessionID:   "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee",
			StartMs:     now,
			ProjectHash: "ee000000000000000000000000000000000000000000000000000000000000ee",
			ProjectPath: "/home/user/specialproject",
			GitRemote:   &nonMatchingRemote,
		},
		{
			// This session has a NON-matching remote AND a NON-matching cwd basename.
			// Expected: does NOT match.
			SessionID:   "ffffffff-ffff-ffff-ffff-ffffffffffff",
			StartMs:     now - 1000,
			ProjectHash: "ff000000000000000000000000000000000000000000000000000000000000ff",
			ProjectPath: "/home/user/otherproject",
			GitRemote:   &nonMatchingRemote,
		},
	})

	output, err := executeSessionsCmd(t, dir, []string{"list", "--project", "specialproject"})
	if err != nil {
		t.Fatalf("sessions list --project specialproject (non-matching remote): unexpected error: %v\noutput: %s", err, output)
	}
	// The session with a non-matching remote but matching cwd basename should appear.
	if !strings.Contains(output, "eeeeeeee"[:8]) {
		t.Errorf("expected eeeeeeee session (non-matching remote, matching basename) in output; got:\n%s", output)
	}
	// The session with neither a matching remote nor a matching basename should not appear.
	if strings.Contains(output, "ffffffff"[:8]) {
		t.Errorf("expected ffffffff session NOT in output (no remote match, no basename match); got:\n%s", output)
	}
}

// TestCLI_SessionsList_Since verifies --since filters sessions by date.
func TestCLI_SessionsList_Since(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	now := time.Now()
	oldMs := now.Add(-30 * 24 * time.Hour).UnixMilli()   // 30 days ago
	recentMs := now.Add(-1 * 24 * time.Hour).UnixMilli() // 1 day ago

	mustSeedMany(t, dir, []seedOpts{
		{
			SessionID:   "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee",
			StartMs:     recentMs,
			ProjectHash: "eeee0000000000000000000000000000000000000000000000000000000000ee",
		},
		{
			SessionID:   "ffffffff-ffff-ffff-ffff-ffffffffffff",
			StartMs:     oldMs,
			ProjectHash: "ffff0000000000000000000000000000000000000000000000000000000000ff",
		},
	})

	// --since 7d should include recent session but not 30-day-old one.
	output, err := executeSessionsCmd(t, dir, []string{"list", "--since", "7d"})
	if err != nil {
		t.Fatalf("sessions list --since 7d: unexpected error: %v\noutput: %s", err, output)
	}
	if !strings.Contains(output, "eeeeeeee"[:8]) {
		t.Errorf("expected recent session in output; got:\n%s", output)
	}
	if strings.Contains(output, "ffffffff"[:8]) {
		t.Errorf("expected old session NOT in output; got:\n%s", output)
	}
}

// TestCLI_SessionsList_Harness verifies --harness filters by model_harness.
func TestCLI_SessionsList_Harness(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	now := time.Now().UnixMilli()
	mustSeedMany(t, dir, []seedOpts{
		{
			SessionID:   "11111111-1111-1111-1111-111111111111",
			Harness:     defaults.HarnessClaudeCode,
			StartMs:     now,
			ProjectHash: "1111000000000000000000000000000000000000000000000000000000001111",
		},
		{
			SessionID:   "22222222-2222-2222-2222-222222222222",
			Harness:     defaults.HarnessOpenCode,
			StartMs:     now - 1000,
			ProjectHash: "2222000000000000000000000000000000000000000000000000000000002222",
		},
	})

	output, err := executeSessionsCmd(t, dir, []string{"list", "--harness", string(defaults.HarnessClaudeCode)})
	if err != nil {
		t.Fatalf("sessions list --harness claude-code: unexpected error: %v\noutput: %s", err, output)
	}
	if !strings.Contains(output, "11111111"[:8]) {
		t.Errorf("expected claude session in output; got:\n%s", output)
	}
	if strings.Contains(output, "22222222"[:8]) {
		t.Errorf("expected opencode session NOT in output; got:\n%s", output)
	}
}

// TestCLI_SessionsList_SortAndReverse verifies --sort and --reverse flags.
func TestCLI_SessionsList_SortAndReverse(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	now := time.Now().UnixMilli()
	// Seed two sessions with different token counts.
	mustSeedMany(t, dir, []seedOpts{
		{
			SessionID:   "33333333-3333-3333-3333-333333333333",
			StartMs:     now,
			ProjectHash: "3333000000000000000000000000000000000000000000000000000000003333",
			TokensIn:    100,
			TokensOut:   50,
		},
		{
			SessionID:   "44444444-4444-4444-4444-444444444444",
			StartMs:     now - 1000,
			ProjectHash: "4444000000000000000000000000000000000000000000000000000000004444",
			TokensIn:    500,
			TokensOut:   200,
		},
	})

	// --sort tokens (DESC by default) — higher tokens first.
	output, err := executeSessionsCmd(t, dir, []string{"list", "--sort", string(defaults.SessionSortTokens)})
	if err != nil {
		t.Fatalf("sessions list --sort tokens: unexpected error: %v\noutput: %s", err, output)
	}
	pos33 := strings.Index(output, "33333333"[:8])
	pos44 := strings.Index(output, "44444444"[:8])
	if pos33 < 0 || pos44 < 0 {
		t.Fatalf("expected both sessions in output; got:\n%s", output)
	}
	// 44444444 has more tokens, so it should appear first (lower index in output).
	if pos44 > pos33 {
		t.Errorf("expected 44444444 (higher tokens) before 33333333; got:\n%s", output)
	}

	// --sort tokens --reverse (ASC) — lower tokens first.
	output, err = executeSessionsCmd(t, dir, []string{"list", "--sort", string(defaults.SessionSortTokens), "--reverse"})
	if err != nil {
		t.Fatalf("sessions list --sort tokens --reverse: unexpected error: %v\noutput: %s", err, output)
	}
	pos33 = strings.Index(output, "33333333"[:8])
	pos44 = strings.Index(output, "44444444"[:8])
	if pos33 < 0 || pos44 < 0 {
		t.Fatalf("expected both sessions in output (reverse); got:\n%s", output)
	}
	// With ASC, 33333333 (fewer tokens) should appear first.
	if pos33 > pos44 {
		t.Errorf("expected 33333333 (lower tokens) before 44444444 in reverse; got:\n%s", output)
	}
}

// TestCLI_SessionsList_Limit verifies --limit restricts output.
func TestCLI_SessionsList_Limit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	now := time.Now().UnixMilli()
	// Seed 10 sessions.
	opts := make([]seedOpts, 10)
	uuids := []string{
		"55555555-5555-5555-5555-555555555551",
		"55555555-5555-5555-5555-555555555552",
		"55555555-5555-5555-5555-555555555553",
		"55555555-5555-5555-5555-555555555554",
		"55555555-5555-5555-5555-555555555555",
		"55555555-5555-5555-5555-555555555556",
		"55555555-5555-5555-5555-555555555557",
		"55555555-5555-5555-5555-555555555558",
		"55555555-5555-5555-5555-555555555559",
		"55555555-5555-5555-5555-55555555555a",
	}
	hashes := []string{
		"5551000000000000000000000000000000000000000000000000000000005551",
		"5552000000000000000000000000000000000000000000000000000000005552",
		"5553000000000000000000000000000000000000000000000000000000005553",
		"5554000000000000000000000000000000000000000000000000000000005554",
		"5555000000000000000000000000000000000000000000000000000000005555",
		"5556000000000000000000000000000000000000000000000000000000005556",
		"5557000000000000000000000000000000000000000000000000000000005557",
		"5558000000000000000000000000000000000000000000000000000000005558",
		"5559000000000000000000000000000000000000000000000000000000005559",
		"555a000000000000000000000000000000000000000000000000000000005550",
	}
	for i := range opts {
		opts[i] = seedOpts{
			SessionID:   uuids[i],
			StartMs:     now - int64(i)*1000,
			ProjectHash: hashes[i],
		}
	}
	mustSeedMany(t, dir, opts)

	output, err := executeSessionsCmd(t, dir, []string{"list", "--limit", "3"})
	if err != nil {
		t.Fatalf("sessions list --limit 3: unexpected error: %v\noutput: %s", err, output)
	}
	// Footer should show 3 of 10.
	if !strings.Contains(output, "3 of 10") {
		t.Errorf("expected '3 of 10' footer; got:\n%s", output)
	}
}

// TestCLI_SessionsBare verifies that bare `peasant sessions` shows help AND listing.
func TestCLI_SessionsBare(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	now := time.Now().UnixMilli()
	seedTestSessionFull(t, dir, seedOpts{
		SessionID: testSessionUUID,
		StartMs:   now,
	})

	// Bare sessions command with no subcommand (empty args to BuildSessionsCommand).
	output, err := executeSessionsCmd(t, dir, []string{})
	if err != nil {
		t.Fatalf("bare sessions: unexpected error: %v\noutput: %s", err, output)
	}

	// Should include help text (Usage section).
	if !strings.Contains(output, "Usage") && !strings.Contains(output, "usage") && !strings.Contains(output, "Available Commands") {
		t.Errorf("bare sessions: expected help text in output; got:\n%s", output)
	}
	// Should also include the session listing.
	idPrefix := testSessionUUID[:8]
	if !strings.Contains(output, idPrefix) {
		t.Errorf("bare sessions: expected session ID prefix %q in listing; got:\n%s", idPrefix, output)
	}
}

// TestCLI_SessionsList_EmptyDB verifies that listing an empty DB shows a friendly message.
func TestCLI_SessionsList_EmptyDB(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Seed the golden DB but add no sessions.
	dbPath := string(defaults.ResolveDBFilePathWith(dir))
	if err := os.MkdirAll(string(defaults.ResolveDataDirPathWith(dir)), defaults.PrivateDirPerm); err != nil {
		t.Fatalf("create data dir: %v", err)
	}
	storetest.CopyGoldenTo(t, dbPath)

	output, err := executeSessionsCmd(t, dir, []string{"list"})
	if err != nil {
		t.Fatalf("sessions list empty: unexpected error: %v\noutput: %s", err, output)
	}
	if !strings.Contains(output, "no sessions") && !strings.Contains(output, "0 sessions") {
		t.Errorf("expected empty message; got:\n%s", output)
	}
}

// ---------------------------------------------------------------------------
// Store-level tests (ListSessionsFiltered + FirstUserMessage)
// ---------------------------------------------------------------------------

// TestStore_ListSessionsFiltered_All verifies basic listing with no filters.
func TestStore_ListSessionsFiltered_All(t *testing.T) {
	t.Parallel()
	db := storetest.Open(t)
	ctx := t.Context()

	storetest.SeedSession(t, db, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	storetest.SeedSession(t, db, "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")

	rows, err := db.ListSessionsFiltered(ctx, store.SessionListFilter{
		SortField: defaults.SessionSortDate,
		SortDesc:  true,
	})
	if err != nil {
		t.Fatalf("ListSessionsFiltered: %v", err)
	}
	if len(rows) < 2 {
		t.Errorf("expected at least 2 sessions; got %d", len(rows))
	}
}

// TestStore_ListSessionsFiltered_Provider verifies provider (ModelHarness) filtering.
func TestStore_ListSessionsFiltered_Provider(t *testing.T) {
	t.Parallel()
	db := storetest.Open(t)
	ctx := t.Context()

	storetest.SeedSession(t, db, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")

	// Filter by claude (all seeded sessions are claude).
	claudeStr := string(defaults.HarnessClaudeCode)
	rows, err := db.ListSessionsFiltered(ctx, store.SessionListFilter{
		SessionFilter: store.SessionFilter{ModelHarness: &claudeStr},
		SortField:     defaults.SessionSortDate,
		SortDesc:      true,
	})
	if err != nil {
		t.Fatalf("ListSessionsFiltered by provider: %v", err)
	}
	for _, row := range rows {
		if row.ModelHarness != string(defaults.HarnessClaudeCode) {
			t.Errorf("expected provider %q; got %q", defaults.HarnessClaudeCode, row.ModelHarness)
		}
	}
}

// TestStore_ListSessionsFiltered_Limit verifies LIMIT is respected.
func TestStore_ListSessionsFiltered_Limit(t *testing.T) {
	t.Parallel()
	db := storetest.Open(t)
	ctx := t.Context()

	storetest.SeedSession(t, db, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	storetest.SeedSession(t, db, "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	storetest.SeedSession(t, db, "cccccccc-cccc-cccc-cccc-cccccccccccc")

	rows, err := db.ListSessionsFiltered(ctx, store.SessionListFilter{
		SortField: defaults.SessionSortDate,
		SortDesc:  true,
		Limit:     2,
	})
	if err != nil {
		t.Fatalf("ListSessionsFiltered with limit: %v", err)
	}
	if len(rows) > 2 {
		t.Errorf("expected at most 2 sessions with limit=2; got %d", len(rows))
	}
}

// TestStore_FirstUserMessage_Basic verifies the first user message is returned.
func TestStore_FirstUserMessage_Basic(t *testing.T) {
	t.Parallel()
	db := storetest.Open(t)
	ctx := t.Context()

	storetest.SeedSession(t, db, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	msg := "Please write a unit test for this function"
	entries := []schema.SessionEntry{
		{
			SessionID:      schema.SessionID("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"),
			EntryIndex:     0,
			Harness:        defaults.HarnessClaudeCode,
			EntryType:      schema.EntryTypeText,
			Role:           schema.RoleUser,
			ContentPreview: &msg,
		},
	}
	if err := db.IndexSessionEntries(ctx, schema.SessionID("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"), entries); err != nil {
		t.Fatalf("IndexSessionEntries: %v", err)
	}

	preview, err := db.FirstUserMessage(ctx, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	if err != nil {
		t.Fatalf("FirstUserMessage: %v", err)
	}
	if preview == "" {
		t.Error("expected non-empty preview")
	}
	// Should be truncated to at most SessionPreviewMaxChars.
	runeCount := 0
	for range preview {
		runeCount++
	}
	if runeCount > defaults.SessionPreviewMaxChars {
		t.Errorf("preview exceeds %d runes: %d", defaults.SessionPreviewMaxChars, runeCount)
	}
	// Should contain beginning of the message.
	if !strings.HasPrefix(preview, "Please write") {
		t.Errorf("expected preview to start with 'Please write'; got %q", preview)
	}
}

// TestStore_FirstUserMessage_UnicodeSafe verifies unicode-aware truncation.
func TestStore_FirstUserMessage_UnicodeSafe(t *testing.T) {
	t.Parallel()
	db := storetest.Open(t)
	ctx := t.Context()

	storetest.SeedSession(t, db, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	// A message that is longer than 40 runes, with multi-byte characters.
	longMsg := "こんにちは世界、私はAIアシスタントです。このメッセージは日本語で書かれています。"
	entries := []schema.SessionEntry{
		{
			SessionID:      schema.SessionID("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"),
			EntryIndex:     0,
			Harness:        defaults.HarnessClaudeCode,
			EntryType:      schema.EntryTypeText,
			Role:           schema.RoleUser,
			ContentPreview: &longMsg,
		},
	}
	if err := db.IndexSessionEntries(ctx, schema.SessionID("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"), entries); err != nil {
		t.Fatalf("IndexSessionEntries: %v", err)
	}

	preview, err := db.FirstUserMessage(ctx, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	if err != nil {
		t.Fatalf("FirstUserMessage: %v", err)
	}
	runeCount := 0
	for range preview {
		runeCount++
	}
	if runeCount > defaults.SessionPreviewMaxChars {
		t.Errorf("unicode preview exceeds %d runes: %d runes in %q", defaults.SessionPreviewMaxChars, runeCount, preview)
	}
}

// TestStore_FirstUserMessage_NoEntries verifies an empty preview when no entries exist.
func TestStore_FirstUserMessage_NoEntries(t *testing.T) {
	t.Parallel()
	db := storetest.Open(t)
	ctx := t.Context()

	storetest.SeedSession(t, db, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")

	preview, err := db.FirstUserMessage(ctx, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	if err != nil {
		t.Fatalf("FirstUserMessage with no entries: %v", err)
	}
	if preview != "" {
		t.Errorf("expected empty preview for session with no entries; got %q", preview)
	}
}

// TestStore_FirstUserMessage_MissingSession verifies an empty preview for unknown session.
func TestStore_FirstUserMessage_MissingSession(t *testing.T) {
	t.Parallel()
	db := storetest.Open(t)
	ctx := t.Context()

	preview, err := db.FirstUserMessage(ctx, "nonexistent-session-id")
	if err != nil {
		t.Fatalf("FirstUserMessage for unknown session: %v", err)
	}
	if preview != "" {
		t.Errorf("expected empty preview for missing session; got %q", preview)
	}
}

// TestCLI_SessionsList_Until verifies --until filters sessions by upper date bound.
// Sessions more recent than the cutoff should be excluded; older sessions should appear.
func TestCLI_SessionsList_Until(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	now := time.Now()
	oldMs := now.Add(-30 * 24 * time.Hour).UnixMilli()   // 30 days ago
	recentMs := now.Add(-1 * 24 * time.Hour).UnixMilli() // 1 day ago

	mustSeedMany(t, dir, []seedOpts{
		{
			SessionID:   "aaaa1111-aaaa-aaaa-aaaa-aaaa11111111",
			StartMs:     recentMs,
			ProjectHash: "aa110000000000000000000000000000000000000000000000000000000011aa",
		},
		{
			SessionID:   "bbbb2222-bbbb-bbbb-bbbb-bbbb22222222",
			StartMs:     oldMs,
			ProjectHash: "bb220000000000000000000000000000000000000000000000000000000022bb",
		},
	})

	// --until 7d means "sessions before 7 days ago" — the recent session (1d ago) should be
	// excluded; the old session (30d ago) should be included.
	output, err := executeSessionsCmd(t, dir, []string{"list", "--until", "7d"})
	if err != nil {
		t.Fatalf("sessions list --until 7d: unexpected error: %v\noutput: %s", err, output)
	}
	if strings.Contains(output, "aaaa1111"[:8]) {
		t.Errorf("expected recent session NOT in --until 7d output; got:\n%s", output)
	}
	if !strings.Contains(output, "bbbb2222"[:8]) {
		t.Errorf("expected old session (30d ago) in --until 7d output; got:\n%s", output)
	}
}

// TestCLI_SessionsList_Tag verifies --tag filters sessions by tag value.
// Only sessions that have the specified tag should appear in the output.
func TestCLI_SessionsList_Tag(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	now := time.Now().UnixMilli()
	mustSeedMany(t, dir, []seedOpts{
		{
			SessionID:   "cccc3333-cccc-cccc-cccc-cccc33333333",
			StartMs:     now,
			ProjectHash: "cc330000000000000000000000000000000000000000000000000000000033cc",
		},
		{
			SessionID:   "dddd4444-dddd-dddd-dddd-dddd44444444",
			StartMs:     now - 1000,
			ProjectHash: "dd440000000000000000000000000000000000000000000000000000000044dd",
		},
	})

	// Add a tag to the first session only.
	dbPath := string(defaults.ResolveDBFilePathWith(dir))
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("TestCLI_SessionsList_Tag: open store: %v", err)
	}
	defer db.Close()
	if err := db.AddTag(context.Background(), ingest.SessionID("cccc3333-cccc-cccc-cccc-cccc33333333"), "mytag"); err != nil {
		t.Fatalf("TestCLI_SessionsList_Tag: AddTag: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("TestCLI_SessionsList_Tag: close store after AddTag: %v", err)
	}

	output, err := executeSessionsCmd(t, dir, []string{"list", "--tag", "mytag"})
	if err != nil {
		t.Fatalf("sessions list --tag mytag: unexpected error: %v\noutput: %s", err, output)
	}
	if !strings.Contains(output, "cccc3333"[:8]) {
		t.Errorf("expected tagged session in output; got:\n%s", output)
	}
	if strings.Contains(output, "dddd4444"[:8]) {
		t.Errorf("expected untagged session NOT in output; got:\n%s", output)
	}
}

// TestCLI_SessionsList_InvalidHarness verifies the --harness error message uses brace form.
func TestCLI_SessionsList_InvalidHarness(t *testing.T) {
	t.Parallel()
	_, err := executeSessionsCmd(t, t.TempDir(), []string{"list", "--harness", "bogus-harness"})
	if err == nil {
		t.Fatal("expected error for invalid --harness value")
	}
	wantSubstr := "must be one of " + joinHarnesses(defaults.AllHarnesses)
	if !strings.Contains(err.Error(), wantSubstr) {
		t.Errorf("invalid --harness error should contain %q; got: %v", wantSubstr, err)
	}
}
