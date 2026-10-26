package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// listFixture seeds n pullable transcripts owned by alice and shared with
// the given collectives, then points the CLI credentials at the double.
func listFixture(t *testing.T, n int, collectives ...schema.VillageUUID) (string, *testutil.CollectiveVillage) {
	t.Helper()
	dir := t.TempDir()
	server := newPullCollectiveDouble(t)
	ids := pullCollectiveTestIDs(n)
	for _, id := range ids {
		server.SetPullTranscript(id, pullCollectiveTestBlob(t, id), nil)
		for _, collective := range collectives {
			server.Share(id, collective, schema.VillageShareStatusApproved)
		}
	}
	writeCredsAt(t, dir, server.URL())
	return dir, server
}

func countListRows(out string) int {
	return strings.Count(out, "pull title for ")
}

func TestVillageListShowsFirstPageByDefault(t *testing.T) {
	t.Parallel()
	dir, server := listFixture(t, 120, pullCollectiveAID)

	out, err := runVillage(t, dir, "transcripts", "list", "--collective", pullCollectiveAName)
	if err != nil {
		t.Fatalf("list must succeed: %v\n%s", err, out)
	}
	if got := countListRows(out); got != 50 {
		t.Errorf("default listing must show one page of 50, got %d rows:\n%s", got, out)
	}
	want := "Showing 50 of 120 pullable transcripts. Use --all to page to the end."
	if !strings.Contains(out, want) {
		t.Errorf("default listing must carry the honest count note %q:\n%s", want, out)
	}
	if got := server.PullListCalls(); got != 1 {
		t.Errorf("default listing must read exactly one page, got %d list calls", got)
	}
}

func TestVillageListAllPagesToTheEnd(t *testing.T) {
	t.Parallel()
	dir, server := listFixture(t, 120, pullCollectiveAID)

	out, err := runVillage(t, dir, "transcripts", "list", "--collective", pullCollectiveAName, "--all")
	if err != nil {
		t.Fatalf("list --all must succeed: %v\n%s", err, out)
	}
	if got := countListRows(out); got != 120 {
		t.Errorf("--all must show every transcript, got %d rows", got)
	}
	if strings.Contains(out, "Showing ") {
		t.Errorf("--all must not print a partial-page note:\n%s", out)
	}
	if got := server.PullListCalls(); got != 3 {
		t.Errorf("--all over 120 rows at 50 per page must read 3 pages, got %d", got)
	}
}

func TestVillageListPlainDefaultAndAll(t *testing.T) {
	t.Parallel()
	dir, _ := listFixture(t, 120, pullCollectiveAID)

	out, err := runVillage(t, dir, "transcripts", "list")
	if err != nil {
		t.Fatalf("plain list must succeed: %v\n%s", err, out)
	}
	if got := countListRows(out); got != 50 {
		t.Errorf("plain list must show the first page by default, got %d rows", got)
	}
	if !strings.Contains(out, "Use --all to page to the end.") {
		t.Errorf("plain list must point at --all:\n%s", out)
	}

	out, err = runVillage(t, dir, "transcripts", "list", "--all")
	if err != nil {
		t.Fatalf("plain list --all must succeed: %v\n%s", err, out)
	}
	if got := countListRows(out); got != 120 {
		t.Errorf("plain list --all must show every transcript, got %d rows", got)
	}
}

func TestVillageListUserFilterPagingStaysExactWithoutAll(t *testing.T) {
	t.Parallel()
	dir, _ := listFixture(t, 120, pullCollectiveAID)

	// --user filters client-side, so a single page would make the total a lie.
	// It pages to the end even without --all and must not print a partial note.
	out, err := runVillage(t, dir, "transcripts", "list", "--user", "alice")
	if err != nil {
		t.Fatalf("list --user must succeed: %v\n%s", err, out)
	}
	if got := countListRows(out); got != 120 {
		t.Errorf("--user listing must be exact, got %d rows", got)
	}
	if strings.Contains(out, "Showing ") {
		t.Errorf("an exact --user listing must not print a partial note:\n%s", out)
	}
}

func TestVillageListAllDedupesAcrossCollectives(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	server := newPullCollectiveDouble(t)
	ids := pullCollectiveTestIDs(5)
	// ids[0:3] live in A, ids[2:5] in B; ids[2] is in both.
	for _, id := range ids[:3] {
		server.SetPullTranscript(id, pullCollectiveTestBlob(t, id), nil)
		server.Share(id, pullCollectiveAID, schema.VillageShareStatusApproved)
	}
	for _, id := range ids[2:] {
		server.SetPullTranscript(id, pullCollectiveTestBlob(t, id), nil)
		server.Share(id, pullCollectiveBID, schema.VillageShareStatusApproved)
	}
	writeCredsAt(t, dir, server.URL())

	out, err := runVillage(t, dir, "transcripts", "list", "--collective", pullCollectiveAName, "--collective", pullCollectiveBName, "--all")
	if err != nil {
		t.Fatalf("list across two collectives must succeed: %v\n%s", err, out)
	}
	if got := countListRows(out); got != 5 {
		t.Errorf("two collectives with one overlap must list 5 unique transcripts, got %d:\n%s", got, out)
	}
}

func TestVillageListJSONCarriesPageAndTotal(t *testing.T) {
	t.Parallel()
	dir, _ := listFixture(t, 120, pullCollectiveAID)

	out, err := runVillage(t, dir, "transcripts", "list", "--collective", pullCollectiveAName, "--json")
	if err != nil {
		t.Fatalf("list --json must succeed: %v\n%s", err, out)
	}
	var resp schema.PullListResponse
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("decode list json: %v\n%s", err, out)
	}
	if len(resp.Transcripts) != 50 || resp.Total != 120 {
		t.Errorf("json must carry one page with the true total, got %d rows total=%d", len(resp.Transcripts), resp.Total)
	}

	out, err = runVillage(t, dir, "transcripts", "list", "--collective", pullCollectiveAName, "--json", "--all")
	if err != nil {
		t.Fatalf("list --json --all must succeed: %v\n%s", err, out)
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("decode list json: %v\n%s", err, out)
	}
	if len(resp.Transcripts) != 120 {
		t.Errorf("json --all must carry every transcript, got %d", len(resp.Transcripts))
	}
}

func TestVillageListRejectsAllWithLocal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	out, err := runVillage(t, dir, "transcripts", "list", "--local", "--all")
	if err == nil {
		t.Fatalf("--local --all must fail; out=%q", out)
	}
	if !strings.Contains(err.Error(), "--all applies to the remote listing") {
		t.Errorf("error must name why --all is rejected: %v", err)
	}
}

func TestVillageListShortPagePrintsNoNote(t *testing.T) {
	t.Parallel()
	dir, _ := listFixture(t, 5, pullCollectiveAID)

	out, err := runVillage(t, dir, "transcripts", "list", "--collective", pullCollectiveAName)
	if err != nil {
		t.Fatalf("short listing must succeed: %v\n%s", err, out)
	}
	if got := countListRows(out); got != 5 {
		t.Errorf("a short page is the whole set, got %d rows", got)
	}
	if strings.Contains(out, "Showing ") {
		t.Errorf("a complete short page must not print a partial note:\n%s", out)
	}
}

func TestVillageListEmptyListingPrintsNoNote(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	server := newPullCollectiveDouble(t)
	writeCredsAt(t, dir, server.URL())

	out, err := runVillage(t, dir, "transcripts", "list")
	if err != nil {
		t.Fatalf("empty listing must succeed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "No pullable transcripts available") {
		t.Errorf("empty listing must say so:\n%s", out)
	}
	if strings.Contains(out, "Showing ") {
		t.Errorf("empty listing must not print a partial note:\n%s", out)
	}
}
