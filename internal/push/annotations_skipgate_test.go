package push_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/perf"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
)

// fakeVillage is a manifest-aware annotation endpoint for skip-gate/retraction
// tests. GET /api/v1/annotations/manifest returns the configured hash set (or a
// configured non-2xx status for fail-safe tests); POST /api/v1/annotations records
// the received annotations + retractions and the POST count.
type fakeVillage struct {
	mu             sync.Mutex
	manifestHashes []string
	manifestStatus int // 0 ⇒ 200 OK; otherwise this status is returned (fail-safe)
	postStatus     int // 0 ⇒ 200 OK; otherwise annotation upload fails with this status

	postCount         int
	postedAnnotations []schema.AnnotationPushItem
	postedRetractions []string
}

func (f *fakeVillage) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/annotations/manifest") {
			f.mu.Lock()
			status := f.manifestStatus
			hashes := append([]string(nil), f.manifestHashes...)
			f.mu.Unlock()
			if status != 0 && status != http.StatusOK {
				w.WriteHeader(status)
				_, _ = w.Write([]byte("manifest error"))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(schema.NewAnnotationManifestResponse(hashes))
			return
		}
		// POST /api/v1/annotations
		var req schema.AnnotationPushRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.postCount++
		status := f.postStatus
		if status == 0 || status == http.StatusOK {
			f.postedAnnotations = append(f.postedAnnotations, req.Annotations...)
			f.postedRetractions = append(f.postedRetractions, req.Retractions...)
		}
		f.mu.Unlock()
		if status != 0 && status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte("annotation upload error"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(schema.AnnotationPushResponse{Created: len(req.Annotations)})
	}
}

func (f *fakeVillage) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.postCount = 0
	f.postedAnnotations = nil
	f.postedRetractions = nil
}

func (f *fakeVillage) sentHashes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var hs []string
	for _, a := range f.postedAnnotations {
		hs = append(hs, a.ContentHash)
	}
	return hs
}

func (f *fakeVillage) snapshot() (int, []schema.AnnotationPushItem, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.postCount, append([]schema.AnnotationPushItem(nil), f.postedAnnotations...), append([]string(nil), f.postedRetractions...)
}

// TestPushAnnotations_NoChange_SendsZero verifies the steady-state win: after a
// first push, a re-push with nothing changed fetches the manifest, finds every
// hash present, and POSTs ZERO annotations. Drives the real PushAnnotationsSelected.
func TestPushAnnotations_NoChange_SendsZero(t *testing.T) {
	t.Parallel()
	fv := &fakeVillage{}
	srv := httptest.NewServer(fv.handler())
	defer srv.Close()
	client := village.NewVillageClient(srv.URL, testAPIKey, nil)
	store := &stubAnnotationStore{rows: []ingest.AnnotationPushRow{
		newSessionAnnotationRow("s1", "retry_loops", "2"),
		newSessionAnnotationRow("s2", "outcome", "success"),
	}}

	// First push: empty manifest ⇒ both sent. Capture their server-side hashes.
	first, err := push.PushAnnotationsSelected(context.Background(), client, store, push.AnnotationSelection{}, false, 4)
	if err != nil {
		t.Fatalf("first push: %v", err)
	}
	if first.Total != 2 {
		t.Fatalf("first push Total = %d, want 2", first.Total)
	}
	fv.manifestHashes = fv.sentHashes()
	fv.reset()

	// Second push: manifest now holds both ⇒ 0 sent.
	second, err := push.PushAnnotationsSelected(context.Background(), client, store, push.AnnotationSelection{}, false, 4)
	if err != nil {
		t.Fatalf("second push: %v", err)
	}
	if fv.postCount != 0 {
		t.Errorf("no-change re-push POSTed %d times, want 0", fv.postCount)
	}
	// Total is the CANDIDATE count (both still considered); all were skipped, so
	// the number actually uploaded is Total - Skipped = 0.
	if second.Total != 2 {
		t.Errorf("no-change re-push Total = %d, want 2 (candidate count)", second.Total)
	}
	if second.Skipped != 2 {
		t.Errorf("no-change re-push Skipped = %d, want 2", second.Skipped)
	}
	if second.Total-second.Skipped != 0 {
		t.Errorf("no-change re-push uploaded = Total-Skipped = %d, want 0", second.Total-second.Skipped)
	}
}

// TestPushAnnotations_Edited_ResendsOnlyChanged verifies an edited annotation
// (new hash ∉ manifest) is resent while the unchanged one is skipped.
func TestPushAnnotations_Edited_ResendsOnlyChanged(t *testing.T) {
	t.Parallel()
	fv := &fakeVillage{}
	srv := httptest.NewServer(fv.handler())
	defer srv.Close()
	client := village.NewVillageClient(srv.URL, testAPIKey, nil)
	store := &stubAnnotationStore{rows: []ingest.AnnotationPushRow{
		newSessionAnnotationRow("s1", "retry_loops", "2"),
		newSessionAnnotationRow("s2", "outcome", "success"),
	}}

	// First push captures both hashes into the manifest.
	if _, err := push.PushAnnotationsSelected(context.Background(), client, store, push.AnnotationSelection{}, false, 4); err != nil {
		t.Fatalf("first push: %v", err)
	}
	fv.manifestHashes = fv.sentHashes()
	fv.reset()

	// Edit s1's value ⇒ its hash changes ⇒ ∉ manifest ⇒ resent; s2 unchanged ⇒ skipped.
	store.rows[0] = newSessionAnnotationRow("s1", "retry_loops", "9")

	summary, err := push.PushAnnotationsSelected(context.Background(), client, store, push.AnnotationSelection{}, false, 4)
	if err != nil {
		t.Fatalf("edited push: %v", err)
	}
	if got := len(fv.postedAnnotations); got != 1 {
		t.Fatalf("edited push sent %d annotations, want 1 (only the edited one)", got)
	}
	if fv.postedAnnotations[0].TypeID != "retry_loops" || fv.postedAnnotations[0].Value != "9" {
		t.Errorf("wrong annotation resent: %+v", fv.postedAnnotations[0])
	}
	if summary.Skipped != 1 {
		t.Errorf("edited push Skipped = %d, want 1 (the unchanged one)", summary.Skipped)
	}
}

// TestPushAnnotations_FailSafe_5xx verifies that a non-2xx manifest response
// disables the skip-gate: ALL annotations are pushed (never skip on unknown).
func TestPushAnnotations_FailSafe_5xx(t *testing.T) {
	t.Parallel()
	fv := &fakeVillage{manifestStatus: http.StatusInternalServerError}
	srv := httptest.NewServer(fv.handler())
	defer srv.Close()
	client := village.NewVillageClient(srv.URL, testAPIKey, nil)
	store := &stubAnnotationStore{rows: []ingest.AnnotationPushRow{
		newSessionAnnotationRow("s1", "retry_loops", "2"),
		newSessionAnnotationRow("s2", "outcome", "success"),
	}}

	summary, err := push.PushAnnotationsSelected(context.Background(), client, store, push.AnnotationSelection{}, false, 4)
	if err != nil {
		t.Fatalf("fail-safe push: %v", err)
	}
	if got := len(fv.postedAnnotations); got != 2 {
		t.Errorf("manifest 5xx: sent %d, want 2 (fail-safe = push all)", got)
	}
	if summary.Skipped != 0 {
		t.Errorf("manifest 5xx: Skipped = %d, want 0 (never skip on unknown)", summary.Skipped)
	}
}

// failManifestRoundTripper fails the manifest GET with a transport error but
// delegates every other request to base — simulating an unreachable manifest
// endpoint while the POST path still works.
type failManifestRoundTripper struct{ base http.RoundTripper }

func (f failManifestRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasSuffix(req.URL.Path, "/annotations/manifest") {
		return nil, fmt.Errorf("simulated transport failure reaching manifest")
	}
	return f.base.RoundTrip(req)
}

// TestPushAnnotations_FailSafe_Transport verifies the DISTINCT transport/unreachable
// failure path (statusCode 0, Do() error) also disables the skip-gate ⇒ push all.
func TestPushAnnotations_FailSafe_Transport(t *testing.T) {
	t.Parallel()
	fv := &fakeVillage{}
	srv := httptest.NewServer(fv.handler())
	defer srv.Close()

	httpClient := &http.Client{Transport: failManifestRoundTripper{base: http.DefaultTransport}}
	client := village.NewVillageClient(srv.URL, testAPIKey, httpClient)
	store := &stubAnnotationStore{rows: []ingest.AnnotationPushRow{
		newSessionAnnotationRow("s1", "retry_loops", "2"),
		newSessionAnnotationRow("s2", "outcome", "success"),
	}}

	if _, err := push.PushAnnotationsSelected(context.Background(), client, store, push.AnnotationSelection{}, false, 4); err != nil {
		t.Fatalf("fail-safe (transport) push: %v", err)
	}
	if got := len(fv.postedAnnotations); got != 2 {
		t.Errorf("manifest unreachable: sent %d, want 2 (fail-safe = push all)", got)
	}
}

// TestPushAnnotations_Retraction_IntersectionAndHashEquality verifies that the
// retraction set = manifest ∩ locally-superseded hashes; a foreign manifest hash
// (not locally superseded) is NOT retracted; and a superseded annotation's
// retraction hash EQUALS the hash it was originally pushed with.
func TestPushAnnotations_Retraction_IntersectionAndHashEquality(t *testing.T) {
	t.Parallel()
	fv := &fakeVillage{}
	srv := httptest.NewServer(fv.handler())
	defer srv.Close()
	client := village.NewVillageClient(srv.URL, testAPIKey, nil)

	// Step 1: push annotation S as ACTIVE to capture the EXACT hash the village
	// stored for it (this is the "originally pushed" hash).
	sRow := newSessionAnnotationRow("s-super", "retry_loops", "3")
	store := &stubAnnotationStore{rows: []ingest.AnnotationPushRow{sRow}}
	if _, err := push.PushAnnotationsSelected(context.Background(), client, store, push.AnnotationSelection{}, false, 4); err != nil {
		t.Fatalf("seed push: %v", err)
	}
	sent := fv.sentHashes()
	if len(sent) != 1 {
		t.Fatalf("seed push sent %d hashes, want 1", len(sent))
	}
	originallyPushedHash := sent[0]
	fv.reset()

	// Step 2: S is now locally superseded; an ACTIVE annotation A remains. The
	// manifest holds S's hash AND a FOREIGN hash (another machine's, not locally
	// present). A is new (∉ manifest) so it will be sent.
	const foreignHash = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef0"
	store = &stubAnnotationStore{
		rows:       []ingest.AnnotationPushRow{newSessionAnnotationRow("s-active", "outcome", "success")},
		superseded: []ingest.AnnotationPushRow{sRow},
	}
	fv.manifestHashes = []string{originallyPushedHash, foreignHash}

	summary, err := push.PushAnnotationsSelected(context.Background(), client, store, push.AnnotationSelection{}, false, 4)
	if err != nil {
		t.Fatalf("retraction push: %v", err)
	}

	// Exactly one retraction: S's originally-pushed hash. Foreign hash untouched.
	if len(fv.postedRetractions) != 1 {
		t.Fatalf("retractions = %v, want exactly 1", fv.postedRetractions)
	}
	if fv.postedRetractions[0] != originallyPushedHash {
		t.Errorf("retraction hash = %q, want the originally-pushed hash %q", fv.postedRetractions[0], originallyPushedHash)
	}
	for _, r := range fv.postedRetractions {
		if r == foreignHash {
			t.Error("foreign-machine manifest hash was retracted — must never happen")
		}
	}
	if summary.Retracted != 1 {
		t.Errorf("summary.Retracted = %d, want 1", summary.Retracted)
	}
}

// TestPushAnnotations_FailedRetractionIsNotReportedAsCompleted keeps the summary
// on observed server success. The retraction is placed in the first request, but
// a failed response does not prove the village applied it.
func TestPushAnnotations_FailedRetractionIsNotReportedAsCompleted(t *testing.T) {
	t.Parallel()
	fv := &fakeVillage{}
	srv := httptest.NewServer(fv.handler())
	defer srv.Close()
	client := village.NewVillageClient(srv.URL, testAPIKey, nil)

	row := newSessionAnnotationRow("s-retraction-failure", "outcome", "old")
	store := &stubAnnotationStore{rows: []ingest.AnnotationPushRow{row}}
	if _, err := push.PushAnnotationsSelected(context.Background(), client, store, push.AnnotationSelection{}, false, 1); err != nil {
		t.Fatalf("seed active annotation: %v", err)
	}
	hashes := fv.sentHashes()
	if len(hashes) != 1 {
		t.Fatalf("seed push hashes = %v, want one", hashes)
	}

	fv.reset()
	fv.manifestHashes = hashes
	fv.postStatus = http.StatusInternalServerError
	store.rows = nil
	store.superseded = []ingest.AnnotationPushRow{row}
	summary, err := push.PushAnnotationsSelected(context.Background(), client, store, push.AnnotationSelection{}, false, 1)
	if err == nil {
		t.Fatal("failed retraction request returned no error")
	}
	if summary.Retracted != 0 {
		t.Fatalf("summary.Retracted = %d, want 0: the village rejected the request", summary.Retracted)
	}
}

// TestPushAnnotations_RetractionUsesStoredHashAfterTargetLoss covers a legacy
// orphan created by the old re-index path. The annotation may have been valid
// and published before its local target row disappeared; its stored hash remains
// the exact remote identity and must still be retractable after supersession.
func TestPushAnnotations_RetractionUsesStoredHashAfterTargetLoss(t *testing.T) {
	t.Parallel()
	const publishedHash = "3b41883f5fb7b5b94c0503caa0b7e80bafed64f85196d6f1bdcabdd584b27d91"
	publishedHashValue := publishedHash
	fv := &fakeVillage{manifestHashes: []string{publishedHash}}
	server := httptest.NewServer(fv.handler())
	defer server.Close()

	store := &stubAnnotationStore{superseded: []ingest.AnnotationPushRow{{
		ID:            "superseded-orphan",
		TargetKind:    schema.TargetEntry,
		TypeID:        "quality.turn_outcome",
		Value:         "resolved",
		AnnotatorName: "outcome-classifier",
		ContentHash:   &publishedHashValue,
	}}}
	client := village.NewVillageClient(server.URL, testAPIKey, nil)
	summary, err := push.PushAnnotationsSelected(context.Background(), client, store, push.AnnotationSelection{}, false, 1)
	if err != nil {
		t.Fatalf("retract superseded orphan by stored hash: %v", err)
	}
	_, posted, retractions := fv.snapshot()
	if summary.Retracted != 1 || len(posted) != 0 || len(retractions) != 1 || retractions[0] != publishedHash {
		t.Fatalf("summary=%+v posted=%d retractions=%v, want one retraction of the stored published hash", summary, len(posted), retractions)
	}
}

// TestPushAnnotations_EntryTargetExclusiveArmLifecycle verifies that an entry
// annotation uses only its nested entry target throughout publish, skip, and
// retraction. The fixture supplies the annotation's target and content fields.
func TestPushAnnotations_EntryTargetExclusiveArmLifecycle(t *testing.T) {
	t.Parallel()

	fixtures, err := schema.LoadAnnotationFixtures()
	if err != nil {
		t.Fatalf("LoadAnnotationFixtures: %v", err)
	}
	fixture := fixtures.FindSummary("rule_frustration_signal_entry")
	if fixture == nil {
		t.Fatal("annotation fixture rule_frustration_signal_entry not found")
	}
	if fixture.TargetSessionID == "" || fixture.TargetEntryIndex == nil {
		t.Fatalf("annotation fixture rule_frustration_signal_entry has incomplete entry target: %+v", fixture)
	}

	entryIndex := *fixture.TargetEntryIndex
	entryEndIndex := entryIndex + 1
	sessionID := fixture.TargetSessionID
	row := ingest.AnnotationPushRow{
		ID:            fixture.ID,
		TargetKind:    schema.TargetKind(fixture.TargetKind),
		SessionID:     &sessionID,
		EntryIndex:    &entryIndex,
		EntryEndIndex: &entryEndIndex,
		TypeID:        fixture.TypeID,
		Value:         fixture.Value,
		IsPrimary:     fixture.IsPrimary,
		AnnotatorName: fixture.AnnotatorName,
	}
	store := &stubAnnotationStore{rows: []ingest.AnnotationPushRow{row}}
	fv := &fakeVillage{}
	srv := httptest.NewServer(fv.handler())
	defer srv.Close()
	client := village.NewVillageClient(srv.URL, testAPIKey, nil)

	first, err := push.PushAnnotationsSelected(context.Background(), client, store, push.AnnotationSelection{}, false, 1)
	if err != nil {
		t.Fatalf("first entry annotation push: %v", err)
	}
	if first.Total != 1 || first.Created != 1 {
		t.Fatalf("first entry annotation push summary = %+v, want total=1 created=1", first)
	}
	postCount, posted, _ := fv.snapshot()
	if postCount != 1 || len(posted) != 1 {
		t.Fatalf("first entry annotation push requests=%d items=%d, want one request with one item", postCount, len(posted))
	}
	item := posted[0]
	if err := item.Validate(); err != nil {
		t.Fatalf("first entry annotation item failed schema validation: %v", err)
	}
	if item.TargetKind != schema.TargetEntry || item.EntryTarget == nil {
		t.Fatalf("first entry annotation target = %+v, want targetKind entry with entryTarget", item)
	}
	if item.SessionID != nil || item.TargetAssociationID != nil || item.AnnotationID != nil || item.ProjectHash != nil {
		t.Fatalf("first entry annotation populated another target arm: %+v", item)
	}
	if item.EntryTarget.SessionID != sessionID || item.EntryTarget.EntryIndex != entryIndex || item.EntryTarget.EndIndex != entryEndIndex {
		t.Fatalf("first entry annotation nested target = %+v, want session=%q index=%d end=%d", item.EntryTarget, sessionID, entryIndex, entryEndIndex)
	}
	if item.ContentHash == "" || item.ContentHash != item.ComputeContentHash() {
		t.Fatalf("first entry annotation content hash = %q, want recomputed canonical hash", item.ContentHash)
	}
	activeHash := item.ContentHash

	fv.mu.Lock()
	fv.manifestHashes = []string{activeHash}
	fv.mu.Unlock()
	fv.reset()
	second, err := push.PushAnnotationsSelected(context.Background(), client, store, push.AnnotationSelection{}, false, 1)
	if err != nil {
		t.Fatalf("entry annotation skip push: %v", err)
	}
	postCount, posted, retractions := fv.snapshot()
	if second.Total != 1 || second.Skipped != 1 || postCount != 0 || len(posted) != 0 || len(retractions) != 0 {
		t.Fatalf("entry annotation skip summary=%+v requests=%d items=%d retractions=%v, want one skipped item and no POST", second, postCount, len(posted), retractions)
	}

	store.rows = nil
	store.superseded = []ingest.AnnotationPushRow{row}
	fv.reset()
	third, err := push.PushAnnotationsSelected(context.Background(), client, store, push.AnnotationSelection{}, false, 1)
	if err != nil {
		t.Fatalf("entry annotation retraction push: %v", err)
	}
	postCount, posted, retractions = fv.snapshot()
	if third.Retracted != 1 || postCount != 1 || len(posted) != 0 || len(retractions) != 1 || retractions[0] != activeHash {
		t.Fatalf("entry annotation retraction summary=%+v requests=%d items=%d retractions=%v, want exactly %q", third, postCount, len(posted), retractions, activeHash)
	}
}

// TestUploadAnnotations_ConnectionReuse verifies the pooled transport reuses the
// connection on a second annotation upload (Reused=true), the steady-state goal
// of the HTTP-pool sizing (not "exactly N connections").
func TestUploadAnnotations_ConnectionReuse(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(schema.AnnotationPushResponse{Created: 0})
	}))
	defer srv.Close()

	// Pooled client (CLI path), sized to 4.
	client := village.NewVillageClientWithConcurrency(srv.URL, testAPIKey, 4)
	sessionID := testutil.TestSessionUUID
	req := schema.AnnotationPushRequest{Annotations: []schema.AnnotationPushItem{{
		ContentHash: "h",
		TargetKind:  schema.TargetSession,
		SessionID:   &sessionID,
		TypeID:      "t",
		Value:       "v",
	}}}

	upload := func() perf.UploadSample {
		ctx, trace := perf.ContextWithUploadTrace(context.Background())
		if _, _, err := client.UploadAnnotations(ctx, req); err != nil {
			t.Fatalf("upload annotations: %v", err)
		}
		return trace.Sample("x")
	}

	first := upload()
	if !first.Connected {
		t.Fatal("first upload not Connected — httptrace not attached?")
	}
	if first.Reused {
		t.Error("first upload Reused=true, want false")
	}
	second := upload()
	if !second.Reused {
		t.Error("second upload Reused=false, want true (pooled connection reuse)")
	}
}
