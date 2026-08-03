package codemap

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/internal/codegraph"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/gitops"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// TestResolveProject_NotFoundPathsShareVisibilityQueryShape is the white-box
// regression guard proving ResolveProject's legacy-label zero-match ("no
// such label") and single-match-but-hidden ("label exists, session
// visibility denies it") failures both return through the SAME internal
// projectHasVisibleSession call — one querySessions round-trip plus a
// Visible() loop — rather than the zero-match branch short-circuiting
// before ever touching that seam. That parity closes a residual timing
// side-channel: the two branches already return byte-identical error text,
// but without this the hidden-match branch alone paid the extra
// querySessions round-trip, letting response latency (not just body)
// distinguish "hidden project" from "no such project."
//
// This is deliberately a white-box (package-internal) test, not a fixture
// case: "equal query shape" isn't an externally observable payload/error
// value the black-box fixture format in project_resolution_test.go can
// assert on. A future refactor that special-cases the zero-match branch
// back to an early return (skipping projectHasVisibleSession) would still
// pass every project_resolution.yaml case unchanged — only this test
// exercises the seam directly.
func TestResolveProject_NotFoundPathsShareVisibilityQueryShape(t *testing.T) {
	database := storetest.Open(t)

	hiddenHash, err := schema.NewProjectHash("2223456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("fixture project hash: %v", err)
	}
	sessionID, err := schema.NewSessionID("99999999-9999-9999-9999-999999999999")
	if err != nil {
		t.Fatalf("fixture session ID: %v", err)
	}
	metadata := &schema.UnifiedMetadata{
		SessionID:    sessionID,
		ModelHarness: defaults.HarnessClaudeCode,
		Model:        testutil.TestModel,
		HostSlug:     schema.HostSlug(testutil.TestHostSlug),
		Project:      schema.ProjectContext{Hash: hiddenHash, Name: "project", FilePath: "/work/timing-hidden"},
		Timestamp:    schema.TimestampInfo{Start: 1000, End: 1500, Ingested: int64Ptr(1501)},
		Source:       schema.SourceInfo{FilePath: "/timing.jsonl", Format: schema.SourceFormatJSONL},
	}
	if err := database.InsertSessions(context.Background(), []ingest.StoreEntry{{Metadata: metadata}}); err != nil {
		t.Fatalf("seed hidden project: %v", err)
	}

	// selected-empty: every session (including the one just seeded) is
	// hidden, exercising the same "no visible session" outcome for a
	// project that DOES exist as for one that doesn't.
	visibility, err := sessionvisibility.New(config.SelectionConfig{Mode: config.SelectionModeSelected})
	if err != nil {
		t.Fatalf("selection policy: %v", err)
	}
	service := NewService(database, func(string) gitops.Repository { return testutil.NoGitRepository() }, codegraph.NewGraphBuilder(), visibility)

	// The zero-match path: an empty/nonexistent hash. Must go through
	// projectHasVisibleSession (one querySessions round-trip that returns
	// zero rows) rather than short-circuiting, and must report "not
	// visible" — never an error — so ResolveProject's case 0 can safely
	// call it unconditionally.
	notFoundVisible, notFoundErr := service.projectHasVisibleSession(context.Background(), schema.ProjectHash(""))
	if notFoundErr != nil {
		t.Fatalf("projectHasVisibleSession(empty hash) error = %v, want nil", notFoundErr)
	}
	if notFoundVisible {
		t.Fatalf("projectHasVisibleSession(empty hash) = true, want false (no rows can match)")
	}

	// The hidden single-match path: a real, seeded, but fully-hidden
	// project. Same seam, same shape of call, same (false, nil) result —
	// proving the two ResolveProject failure branches are symmetric.
	hiddenVisible, hiddenErr := service.projectHasVisibleSession(context.Background(), hiddenHash)
	if hiddenErr != nil {
		t.Fatalf("projectHasVisibleSession(hidden hash) error = %v, want nil", hiddenErr)
	}
	if hiddenVisible {
		t.Fatalf("projectHasVisibleSession(hidden hash) = true, want false (selected-empty hides every session)")
	}

	// End-to-end and mutation-provable: count actual projectHasVisibleSession
	// invocations (via Service.onVisibilityQuery, a test-only counting hook —
	// nil/no-op in production) as ResolveProject runs its zero-match and
	// hidden-single-match legacy-label failures. Both must call the seam
	// exactly once. A future refactor that reintroduces case 0's early
	// return (skipping the query) drops its count to 0 and fails this
	// assertion — unlike asserting on ResolveProject's returned error alone,
	// which stays identical either way and would NOT catch the regression.
	var notFoundQueries int
	service.onVisibilityQuery = func() { notFoundQueries++ }
	if _, err := service.ResolveProject(context.Background(), "/work/does-not-exist-at-all"); err == nil {
		t.Fatal("ResolveProject(nonexistent label) unexpectedly succeeded")
	}
	if notFoundQueries != 1 {
		t.Fatalf("ResolveProject(nonexistent label) called projectHasVisibleSession %d times, want exactly 1 (must match the hidden-match branch's query count)", notFoundQueries)
	}

	var hiddenQueries int
	service.onVisibilityQuery = func() { hiddenQueries++ }
	if _, err := service.ResolveProject(context.Background(), "/work/timing-hidden"); err == nil {
		t.Fatal("ResolveProject(hidden label) unexpectedly succeeded")
	}
	if hiddenQueries != 1 {
		t.Fatalf("ResolveProject(hidden label) called projectHasVisibleSession %d times, want exactly 1", hiddenQueries)
	}
	if notFoundQueries != hiddenQueries {
		t.Fatalf("not-found path called projectHasVisibleSession %d time(s), hidden-match path called it %d time(s); want equal counts to close the residual timing side-channel between the two failure paths", notFoundQueries, hiddenQueries)
	}
}

func int64Ptr(v int64) *int64 { return &v }
