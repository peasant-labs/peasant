package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/village_pull_collectives.yaml
var villagePullCollectivesYAML []byte

// TestVillagePullCollectivesFixtures drives the bulk collective pull driver
// against the collective village double's pull surface. Each fixture case names
// one driver behaviour; the test sets up the double's pullable transcripts and
// shares for that behaviour, runs the production
// `village transcripts pull --collective ...` command, and asserts the
// observable outcome (pulled rows, exit code, and which village reads
// happened). The double answers the pull listing, the skip-gate, and the
// per-transcript GETs, so no new doubles are invented here.
func TestVillagePullCollectivesFixtures(t *testing.T) {
	t.Parallel()
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name string `yaml:"name"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeNamedFixtureYAML(villagePullCollectivesYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			switch c.Name {
			case "paging follows a multi-page listing to the end and pulls a transcript held by two collectives once":
				testVillagePullCollectivePaging(t)
			case "continue-on-error pulls the others when one transcript fails and reports non-zero":
				testVillagePullCollectiveContinueOnError(t)
			case "skip-gate pruning does not pull a transcript the gate answers as current":
				testVillagePullCollectiveSkipGatePruning(t)
			case "skip-gate fallback pulls every transcript individually when the gate does not answer":
				testVillagePullCollectiveSkipGateFallback(t)
			default:
				t.Fatalf("unknown bulk pull case %q", c.Name)
			}
		})
	}
}

const (
	pullCollectiveAID   = schema.VillageUUID("11111111-1111-4111-8111-111111111111")
	pullCollectiveBID   = schema.VillageUUID("22222222-2222-4222-8222-222222222222")
	pullCollectiveAName = "platform-team"
	pullCollectiveBName = "oss-mirror"
)

// pullCollectiveTestIDs returns n deterministic transcript IDs for bulk pull
// tests. Generated, not listed, so a multi-page test can ask for more than the
// driver's fixed page size without an inline table.
func pullCollectiveTestIDs(n int) []schema.TranscriptID {
	ids := make([]schema.TranscriptID, 0, n)
	for i := 0; i < n; i++ {
		raw := fmt.Sprintf("aaaaaaaa-bbbb-cccc-dddd-%012x", i)
		id, err := schema.NewTranscriptID(raw)
		if err != nil {
			panic("pullCollectiveTestIDs: " + err.Error())
		}
		ids = append(ids, id)
	}
	return ids
}

// pullCollectiveTestBlob returns a valid TranscriptContent envelope blob for
// the transcript, so the pull pipeline's write and commit succeed.
func pullCollectiveTestBlob(t *testing.T, id schema.TranscriptID) []byte {
	t.Helper()
	envelope := schema.TranscriptContent{
		ContractVersion: defaults.PublishSchemaVersion,
		Kind:            schema.ContentKindSessionDetail,
		SessionDetail: &schema.SessionDetailPayload{
			ID:      id.String(),
			Harness: defaults.HarnessClaudeCode,
			Turns:   foldInvariantTurns(),
		},
	}
	blob, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal pull blob: %v", err)
	}
	return blob
}

// newPullCollectiveDouble starts a collective village knowing two member
// collectives, for bulk pull tests to resolve `--collective` names against.
func newPullCollectiveDouble(t *testing.T) *testutil.CollectiveVillage {
	t.Helper()
	return testutil.NewCollectiveVillage(t,
		testutil.VillageCollective{ID: pullCollectiveAID, Name: pullCollectiveAName, Member: true},
		testutil.VillageCollective{ID: pullCollectiveBID, Name: pullCollectiveBName, Member: true},
	)
}

// seedPullTranscripts stores pull data for each id and shares it approved with
// the given collectives, so a filtered pull listing admits it.
func seedPullTranscripts(t *testing.T, server *testutil.CollectiveVillage, ids []schema.TranscriptID, collectives ...schema.VillageUUID) {
	t.Helper()
	for _, id := range ids {
		server.SetPullTranscript(id, pullCollectiveTestBlob(t, id), nil)
		for _, collective := range collectives {
			server.Share(id, collective, schema.VillageShareStatusApproved)
		}
	}
}

// testVillagePullCollectivePaging proves a listing spanning several pages is
// followed to the end and de-duplicated: 102 transcripts need two pages at the
// driver's fixed page size, and the one held by both listed collectives is
// pulled once, not twice.
func testVillagePullCollectivePaging(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	server := newPullCollectiveDouble(t)
	ids := pullCollectiveTestIDs(102)
	// All but the last live in the first collective; the last is approved in
	// both, so a per-collective loop would pull it twice.
	for _, id := range ids[:101] {
		server.SetPullTranscript(id, pullCollectiveTestBlob(t, id), nil)
		server.Share(id, pullCollectiveAID, schema.VillageShareStatusApproved)
	}
	shared := ids[101]
	server.SetPullTranscript(shared, pullCollectiveTestBlob(t, shared), nil)
	server.Share(shared, pullCollectiveAID, schema.VillageShareStatusApproved)
	server.Share(shared, pullCollectiveBID, schema.VillageShareStatusApproved)
	writeCredsAt(t, dir, server.URL())

	out, err := runVillage(t, dir, "transcripts", "pull", "--collective", pullCollectiveAName, "--collective", pullCollectiveBName)
	if err != nil {
		t.Fatalf("paging pull must exit 0, got err=%v out=%q", err, out)
	}
	if n := countPulledRows(t, dir); n != len(ids) {
		t.Errorf("paging pull must pull all %d transcripts once, got %d rows", len(ids), n)
	}
	if got := server.PullListCalls(); got != 2 {
		t.Errorf("paging pull must read two listing pages (100 + 2), got %d list calls", got)
	}
	if !strings.Contains(out, "pulled 102") || !strings.Contains(out, "failed 0") {
		t.Errorf("paging summary must report 102 pulled and 0 failed; out=%q", out)
	}
}

// testVillagePullCollectiveContinueOnError proves one transcript failing does
// not stop the others, and the run reports a non-zero result.
func testVillagePullCollectiveContinueOnError(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	server := newPullCollectiveDouble(t)
	ids := pullCollectiveTestIDs(3)
	seedPullTranscripts(t, server, ids, pullCollectiveAID)
	server.FailPullContent(ids[1], true)
	writeCredsAt(t, dir, server.URL())

	out, err := runVillage(t, dir, "transcripts", "pull", "--collective", pullCollectiveAName)
	if err == nil {
		t.Fatalf("a bulk pull with one failure must exit non-zero; out=%q", out)
	}
	if !strings.Contains(err.Error(), "did not pull") || !strings.Contains(err.Error(), "1 of 3") {
		t.Errorf("continue-on-error must report 1 of 3 did not pull; got: %v (out=%q)", err, out)
	}
	if n := countPulledRows(t, dir); n != 2 {
		t.Errorf("continue-on-error must keep the 2 successful transcripts, got %d rows (out=%q)", n, out)
	}
	for _, id := range ids {
		if got := server.ContentCalls(id); got != 1 {
			t.Errorf("transcript %s must be attempted once, got %d content calls", id, got)
		}
	}
}

// testVillagePullCollectiveSkipGatePruning proves a transcript the gate answers
// as current is NOT pulled: after one bulk pull populates the local index, a
// second run asks the gate once and reports both up to date without
// re-downloading either blob.
func testVillagePullCollectiveSkipGatePruning(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	server := newPullCollectiveDouble(t)
	ids := pullCollectiveTestIDs(2)
	seedPullTranscripts(t, server, ids, pullCollectiveAID)
	writeCredsAt(t, dir, server.URL())

	if out, err := runVillage(t, dir, "transcripts", "pull", "--collective", pullCollectiveAName); err != nil {
		t.Fatalf("first pull must exit 0, got err=%v out=%q", err, out)
	}
	if got := server.SkipGateCalls(); got != 0 {
		t.Errorf("first pull with an empty local index must not ask the gate, got %d gate calls", got)
	}

	out, err := runVillage(t, dir, "transcripts", "pull", "--collective", pullCollectiveAName)
	if err != nil {
		t.Fatalf("pruned pull must exit 0, got err=%v out=%q", err, out)
	}
	if got := server.SkipGateCalls(); got != 1 {
		t.Errorf("pruned pull must ask the gate once, got %d gate calls", got)
	}
	for _, id := range ids {
		if got := server.ContentCalls(id); got != 1 {
			t.Errorf("pruned transcript %s must never be re-downloaded, got %d content calls", id, got)
		}
	}
	if !strings.Contains(out, "up to date 2") || !strings.Contains(out, "failed 0") {
		t.Errorf("pruned summary must report 2 up to date and 0 failed; out=%q", out)
	}
	if n := countPulledRows(t, dir); n != 2 {
		t.Errorf("pruned pull must keep 2 rows, got %d", n)
	}
}

// testVillagePullCollectiveSkipGateFallback proves a gate answering non-2xx
// warns and every transcript is pulled individually.
func testVillagePullCollectiveSkipGateFallback(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	server := newPullCollectiveDouble(t)
	ids := pullCollectiveTestIDs(2)
	seedPullTranscripts(t, server, ids, pullCollectiveAID)
	writeCredsAt(t, dir, server.URL())
	// Populate the local index first, so the second run would prune if the gate
	// answered. With the gate failing it must instead check both individually:
	// the run still exits 0 and reports both up to date (the pipeline's DIFF
	// settles them without re-downloading), proving the gate failure did not
	// block the pull.
	if out, err := runVillage(t, dir, "transcripts", "pull", "--collective", pullCollectiveAName); err != nil {
		t.Fatalf("first pull must exit 0, got err=%v out=%q", err, out)
	}
	server.AnswerSkipGate(500)
	out, err := runVillage(t, dir, "transcripts", "pull", "--collective", pullCollectiveAName)
	if err != nil {
		t.Fatalf("fallback pull must exit 0, got err=%v out=%q", err, out)
	}
	if got := server.SkipGateCalls(); got != 1 {
		t.Errorf("fallback pull must attempt the gate once, got %d gate calls", got)
	}
	if !strings.Contains(out, "skip-gate did not answer") || !strings.Contains(out, "checked individually") {
		t.Errorf("fallback must warn the gate did not answer and every transcript is checked individually; out=%q", out)
	}
	for _, id := range ids {
		if got := server.ContentCalls(id); got != 1 {
			t.Errorf("fallback must not re-download an up-to-date transcript %s, got %d content calls (want 1: first pull only)", id, got)
		}
	}
	if !strings.Contains(out, "up to date 2") || !strings.Contains(out, "failed 0") {
		t.Errorf("fallback summary must report 2 up to date and 0 failed; out=%q", out)
	}
	if n := countPulledRows(t, dir); n != 2 {
		t.Errorf("fallback pull must keep 2 rows, got %d", n)
	}
}
