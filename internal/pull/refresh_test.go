package pull_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/pull"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
)

// ownAnnotation returns an annotation authored by the REQUESTER themselves
// (AuthorUserID == TestPullAuthorUserID) — must be excluded from the refresh.
func ownAnnotation(hash string) schema.PullAnnotation {
	h := hash
	return schema.PullAnnotation{
		AnnotationSummary: schema.AnnotationSummary{
			ID:          "annot-own-" + hash,
			TargetKind:  schema.TargetSession,
			TypeID:      testutil.TestTypeIDSessionApproval,
			Value:       "deny",
			ContentHash: &h,
		},
		AuthorUserID:   testutil.TestPullAuthorUserID, // the requester's own id
		AuthorUsername: "self",
	}
}

// ownTranscriptListing returns a one-page listing in which the requester owns the
// transcript (OwnerUserID == TestPullAuthorUserID) plus a transcript owned by
// someone else (which the refresh must ignore).
func ownTranscriptListing() *schema.PullListResponse {
	return &schema.PullListResponse{
		Transcripts: []schema.PullTranscriptInfo{
			{
				TranscriptID: testutil.TestTranscriptID,
				LocalID:      testutil.TestSessionUUID,
				OwnerUserID:  testutil.TestPullAuthorUserID, // OWN
			},
			{
				TranscriptID: testutil.TestTranscriptID2,
				OwnerUserID:  testutil.TestAuthorUserID, // someone else — must be skipped
			},
		},
		Page:  1,
		Limit: 100,
		Total: 2,
	}
}

func TestRefreshOwnAnnotations_ExcludesOwnAuthor(t *testing.T) {
	reader := &testutil.StubVillageReader{
		ListResponses: []*schema.PullListResponse{ownTranscriptListing()},
		AnnotationsByID: map[schema.TranscriptID][]schema.PullAnnotation{
			testutil.TestTranscriptID: {
				foreignAnnotation(testutil.TestContentHash), // foreign — kept
				ownAnnotation(testutil.TestContentHash2),    // own — excluded
				foreignAnnotation(testutil.TestContentHash3),
			},
		},
	}
	st := &testutil.StubPullStore{UpsertCreated: 2}

	res, err := pull.NewPipeline(reader, testutil.NewMemFS(), st, testutil.NewFixedClock(), testCreds(), testPullsRoot).
		RefreshOwnAnnotations(context.Background(), pull.RefreshOptions{})
	if err != nil {
		t.Fatalf("RefreshOwnAnnotations: %v", err)
	}

	if res.Status != pull.PullStatusPulled {
		t.Errorf("Status = %q, want pulled", res.Status)
	}
	if res.TranscriptsScanned != 1 {
		t.Errorf("TranscriptsScanned = %d, want 1 (only the OWN transcript)", res.TranscriptsScanned)
	}
	if res.Excluded != 1 {
		t.Errorf("Excluded = %d, want 1 (the own-authored annotation)", res.Excluded)
	}
	// Exactly one upsert batch, carrying ONLY the two foreign annotations.
	if len(st.Upserts) != 1 {
		t.Fatalf("UpsertPulledAnnotations called %d times, want 1", len(st.Upserts))
	}
	rows := st.Upserts[0]
	if len(rows) != 2 {
		t.Fatalf("upsert batch size = %d, want 2 (foreign only)", len(rows))
	}
	for _, r := range rows {
		if r.AuthorUserID == testutil.TestPullAuthorUserID {
			t.Errorf("own-authored annotation %q leaked into the upsert batch", r.ContentHash)
		}
	}
	// NEGOTIATE exactly once.
	if reader.NegotiateCalls != 1 {
		t.Errorf("NegotiateCalls = %d, want 1", reader.NegotiateCalls)
	}
}

// Real-store integration: refresh classifies created/updated/skipped exactly like
// the push vocabulary (skipped = payload-identical) via the REAL store path.
func TestRefreshOwnAnnotations_RealStore_CreatedThenSkipped(t *testing.T) {
	st := storetest.Open(t)
	reader := &testutil.StubVillageReader{
		ListResponses: []*schema.PullListResponse{ownTranscriptListing()},
		AnnotationsByID: map[schema.TranscriptID][]schema.PullAnnotation{
			testutil.TestTranscriptID: {
				foreignAnnotation(testutil.TestContentHash),
				ownAnnotation(testutil.TestContentHash2), // excluded
			},
		},
	}
	p := pull.NewPipeline(reader, testutil.NewMemFS(), st, testutil.NewFixedClock(), testCreds(), testPullsRoot)

	// First refresh: the single foreign annotation is created.
	res, err := p.RefreshOwnAnnotations(context.Background(), pull.RefreshOptions{})
	if err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	if res.Created != 1 || res.Updated != 0 || res.Skipped != 0 {
		t.Errorf("first refresh counts = (c=%d,u=%d,s=%d), want (1,0,0)", res.Created, res.Updated, res.Skipped)
	}
	if res.Excluded != 1 {
		t.Errorf("Excluded = %d, want 1", res.Excluded)
	}

	// Second refresh: identical payload ⇒ skipped (payload-identical, no write).
	res2, err := p.RefreshOwnAnnotations(context.Background(), pull.RefreshOptions{})
	if err != nil {
		t.Fatalf("second refresh: %v", err)
	}
	if res2.Created != 0 || res2.Updated != 0 || res2.Skipped != 1 {
		t.Errorf("second refresh counts = (c=%d,u=%d,s=%d), want (0,0,1)", res2.Created, res2.Updated, res2.Skipped)
	}
	if res2.Status != pull.PullStatusUpToDate {
		t.Errorf("second refresh Status = %q, want up-to-date (nothing changed)", res2.Status)
	}
}

func TestRefreshOwnAnnotations_BySession(t *testing.T) {
	// Two own transcripts; --session narrows to the one whose LocalID matches.
	listing := &schema.PullListResponse{
		Transcripts: []schema.PullTranscriptInfo{
			{TranscriptID: testutil.TestTranscriptID, LocalID: testutil.TestSessionUUID, OwnerUserID: testutil.TestPullAuthorUserID},
			{TranscriptID: testutil.TestTranscriptID2, LocalID: testutil.TestSessionUUID2, OwnerUserID: testutil.TestPullAuthorUserID},
		},
		Page: 1, Limit: 100, Total: 2,
	}
	reader := &testutil.StubVillageReader{
		ListResponses: []*schema.PullListResponse{listing},
		AnnotationsByID: map[schema.TranscriptID][]schema.PullAnnotation{
			testutil.TestTranscriptID:  {foreignAnnotation(testutil.TestContentHash)},
			testutil.TestTranscriptID2: {foreignAnnotation(testutil.TestContentHash2)},
		},
	}
	st := &testutil.StubPullStore{UpsertCreated: 1}

	res, err := pull.NewPipeline(reader, testutil.NewMemFS(), st, testutil.NewFixedClock(), testCreds(), testPullsRoot).
		RefreshOwnAnnotations(context.Background(), pull.RefreshOptions{SessionID: testutil.TestSessionUUID2})
	if err != nil {
		t.Fatalf("RefreshOwnAnnotations: %v", err)
	}
	if res.TranscriptsScanned != 1 {
		t.Errorf("TranscriptsScanned = %d, want 1 (narrowed by --session)", res.TranscriptsScanned)
	}
	// Only TestTranscriptID2's annotations were fetched.
	if reader.AnnotationsCalls != 1 {
		t.Errorf("AnnotationsCalls = %d, want 1", reader.AnnotationsCalls)
	}
	if len(st.Upserts) != 1 || len(st.Upserts[0]) != 1 {
		t.Errorf("upsert batch should carry only the narrowed transcript's foreign annotation")
	}
	if st.Upserts[0][0].ContentHash != testutil.TestContentHash2 {
		t.Errorf("wrong transcript's annotation upserted: %q", st.Upserts[0][0].ContentHash)
	}
}

// --- Refresh failure paths (0tnsq) ---

// TestRefreshOwnAnnotations_AnnotationsFetchError: a GetPullTranscriptAnnotations
// error mid-enumeration aborts the refresh with the mapped status and performs NO
// upsert (no phantom partial write). The enumerated count is still reported.
func TestRefreshOwnAnnotations_AnnotationsFetchError(t *testing.T) {
	reader := &testutil.StubVillageReader{
		ListResponses:  []*schema.PullListResponse{ownTranscriptListing()},
		AnnotationsErr: village.ErrPullNotFound, // mid-loop fetch failure (mapped status)
	}
	st := &testutil.StubPullStore{}

	res, err := pull.NewPipeline(reader, testutil.NewMemFS(), st, testutil.NewFixedClock(), testCreds(), testPullsRoot).
		RefreshOwnAnnotations(context.Background(), pull.RefreshOptions{})
	if err == nil {
		t.Fatal("expected annotation-fetch error")
	}
	// Status mapped via the village sentinel (not a bare error).
	if res.Status != pull.PullStatusNotFound {
		t.Errorf("Status = %q, want not-found (mapped from village sentinel)", res.Status)
	}
	// Partial progress reported correctly: the OWN transcript WAS enumerated...
	if res.TranscriptsScanned != 1 {
		t.Errorf("TranscriptsScanned = %d, want 1", res.TranscriptsScanned)
	}
	// ...but NO upsert happened (no phantom write on a mid-loop fetch failure).
	if len(st.Upserts) != 0 {
		t.Errorf("annotation-fetch failure must not upsert; got %d batch(es)", len(st.Upserts))
	}
}

// TestRefreshOwnAnnotations_UpsertError: an UpsertPulledAnnotations error surfaces
// as PullStatusError with the persist-error message (StubPullStore.UpsertErr).
func TestRefreshOwnAnnotations_UpsertError(t *testing.T) {
	reader := &testutil.StubVillageReader{
		ListResponses: []*schema.PullListResponse{ownTranscriptListing()},
		AnnotationsByID: map[schema.TranscriptID][]schema.PullAnnotation{
			testutil.TestTranscriptID: {foreignAnnotation(testutil.TestContentHash)},
		},
	}
	st := &testutil.StubPullStore{UpsertErr: errors.New("upsert boom")}

	res, err := pull.NewPipeline(reader, testutil.NewMemFS(), st, testutil.NewFixedClock(), testCreds(), testPullsRoot).
		RefreshOwnAnnotations(context.Background(), pull.RefreshOptions{})
	if err == nil {
		t.Fatal("expected upsert error")
	}
	if res.Status != pull.PullStatusError {
		t.Errorf("Status = %q, want error", res.Status)
	}
	if res.TranscriptsScanned != 1 {
		t.Errorf("TranscriptsScanned = %d, want 1", res.TranscriptsScanned)
	}
	if !strings.Contains(err.Error(), "persist foreign annotations") {
		t.Errorf("error should name the persist failure; got: %v", err)
	}
	// The upsert WAS attempted (one batch) — the failure is in the store, not before.
	if len(st.Upserts) != 1 {
		t.Errorf("upsert should have been attempted once; got %d", len(st.Upserts))
	}
}

// --- Multi-page pagination (n78if) ---

// TestRefreshOwnAnnotations_MultiPage: two FULL pages of own transcripts exercise
// the listOwnTranscripts page loop — both own transcripts (across both pages) are
// scanned, and ListCalls reflects exactly the page count (no over/under-fetch).
func TestRefreshOwnAnnotations_MultiPage(t *testing.T) {
	// refreshPageLimit (100) full-page entries on page 1, one entry on page 2.
	const fullPage = 100
	page1 := make([]schema.PullTranscriptInfo, fullPage)
	for i := range page1 {
		page1[i] = schema.PullTranscriptInfo{
			TranscriptID: testutil.TestTranscriptID,
			LocalID:      testutil.TestSessionUUID,
			OwnerUserID:  testutil.TestPullAuthorUserID,
		}
	}
	page2 := []schema.PullTranscriptInfo{
		{TranscriptID: testutil.TestTranscriptID2, LocalID: testutil.TestSessionUUID2, OwnerUserID: testutil.TestPullAuthorUserID},
	}
	reader := &testutil.StubVillageReader{
		ListResponses: []*schema.PullListResponse{
			{Transcripts: page1, Page: 1, Limit: fullPage, Total: fullPage + 1},
			{Transcripts: page2, Page: 2, Limit: fullPage, Total: fullPage + 1},
		},
		AnnotationsByID: map[schema.TranscriptID][]schema.PullAnnotation{
			testutil.TestTranscriptID:  {foreignAnnotation(testutil.TestContentHash)},
			testutil.TestTranscriptID2: {foreignAnnotation(testutil.TestContentHash2)},
		},
	}
	st := &testutil.StubPullStore{UpsertCreated: fullPage + 1}

	res, err := pull.NewPipeline(reader, testutil.NewMemFS(), st, testutil.NewFixedClock(), testCreds(), testPullsRoot).
		RefreshOwnAnnotations(context.Background(), pull.RefreshOptions{})
	if err != nil {
		t.Fatalf("RefreshOwnAnnotations: %v", err)
	}
	// All own transcripts across BOTH pages scanned (100 + 1).
	if res.TranscriptsScanned != fullPage+1 {
		t.Errorf("TranscriptsScanned = %d, want %d (both pages)", res.TranscriptsScanned, fullPage+1)
	}
	// Pagination stopped after EXACTLY two list calls (page*limit >= Total on p2);
	// no infinite re-fetch, no early stop.
	if reader.ListCalls != 2 {
		t.Errorf("ListCalls = %d, want 2 (two-page listing terminates)", reader.ListCalls)
	}
}

// TestRefreshOwnAnnotations_ShortPageTermination: a final page shorter than the
// page limit terminates the loop via the len(page)<limit break (the second break
// condition), distinct from the Total-based break above.
func TestRefreshOwnAnnotations_ShortPageTermination(t *testing.T) {
	// A single SHORT page (1 entry < refreshPageLimit) with Total=0 (unknown total)
	// must terminate via len(resp.Transcripts) < limit, not loop forever.
	reader := &testutil.StubVillageReader{
		ListResponses: []*schema.PullListResponse{
			{
				Transcripts: []schema.PullTranscriptInfo{
					{TranscriptID: testutil.TestTranscriptID, LocalID: testutil.TestSessionUUID, OwnerUserID: testutil.TestPullAuthorUserID},
				},
				Page: 1, Limit: 100, Total: 0, // Total unknown ⇒ rely on short-page break
			},
		},
		AnnotationsByID: map[schema.TranscriptID][]schema.PullAnnotation{
			testutil.TestTranscriptID: {foreignAnnotation(testutil.TestContentHash)},
		},
	}
	st := &testutil.StubPullStore{UpsertCreated: 1}

	res, err := pull.NewPipeline(reader, testutil.NewMemFS(), st, testutil.NewFixedClock(), testCreds(), testPullsRoot).
		RefreshOwnAnnotations(context.Background(), pull.RefreshOptions{})
	if err != nil {
		t.Fatalf("RefreshOwnAnnotations: %v", err)
	}
	if res.TranscriptsScanned != 1 {
		t.Errorf("TranscriptsScanned = %d, want 1", res.TranscriptsScanned)
	}
	// Short page ⇒ exactly ONE list call (no needless second fetch).
	if reader.ListCalls != 1 {
		t.Errorf("ListCalls = %d, want 1 (short page terminates)", reader.ListCalls)
	}
}

func TestRefreshOwnAnnotations_NotLoggedIn(t *testing.T) {
	reader := &testutil.StubVillageReader{}
	st := &testutil.StubPullStore{}
	res, err := pull.NewPipeline(reader, testutil.NewMemFS(), st, testutil.NewFixedClock(), pull.Credentials{}, testPullsRoot).
		RefreshOwnAnnotations(context.Background(), pull.RefreshOptions{})
	if err == nil {
		t.Fatal("expected not-logged-in error")
	}
	if res.Status != pull.PullStatusNotLoggedIn {
		t.Errorf("Status = %q, want not-logged-in", res.Status)
	}
	if reader.NegotiateCalls != 0 || len(st.Upserts) != 0 {
		t.Errorf("logged-out refresh must not contact the village or write")
	}
}
