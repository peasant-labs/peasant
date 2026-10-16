package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/pull"
	"github.com/peasant-labs/peasant/internal/testkit/testwait"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/gates"
	"github.com/peasant-labs/peasant/internal/tui/theme"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
	"github.com/spf13/cobra"
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
			case "progress renders on a terminal with done over total":
				testVillagePullCollectiveProgressTTY(t)
			case "progress stays silent when stderr is not a terminal":
				testVillagePullCollectiveProgressSuppressedOffTerminal(t)
			case "progress stays silent under json and the aggregate is unchanged":
				testVillagePullCollectiveProgressSuppressedUnderJSON(t)
			case "cancel reports after N of M and exits non-zero":
				testVillagePullCollectiveCancelReports(t)
			case "cancel paints the canceling footer while a transcript is in flight":
				testVillagePullCollectiveCancelPaintsWhileInFlight(t)
			case "cancel leaves no partial copy and earlier transcripts stay landed":
				testVillagePullCollectiveCancelNoPartial(t)
			case "resume after cancel downloads only what was missing":
				testVillagePullCollectiveResumeAfterCancel(t)
			case "progress composes through the kit primitives only":
				testVillagePullCollectiveKitGate(t)
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

// progressTestClock is a stepped clock steering the progress throttle
// deterministically: the reported run time only moves when the test moves
// it, so rapid advances and floor refreshes never depend on wall timing.
type progressTestClock struct {
	now time.Time
}

// Now reports the clock's current time for the progress reporter.
func (c *progressTestClock) Now() time.Time { return c.now }

// Advance moves the clock forward by d.
func (c *progressTestClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

// testVillagePullCollectiveProgressTTY proves the stderr bar renders
// done/total on a terminal: the reporter is enabled on a PTY writer and
// the bytes it writes carry the count, the bar cells, and the elapsed run
// time. The clock steps past the refresh floor so the advance is a
// periodic refresh, not a throttled-away write.
func testVillagePullCollectiveProgressTTY(t *testing.T) {
	t.Helper()
	master, terminal := openTestTerminal(t)
	setTestTerminalSize(t, terminal, 24, 80)
	clock := &progressTestClock{now: time.Now()}
	progress := newCollectivePullProgressAt(terminal, false, theme.New(theme.ModeDark), clock.Now)
	if !progress.Enabled() {
		t.Fatal("progress must be enabled on a terminal stderr without --json")
	}
	out := newSignalWriter("1/4")
	copied := make(chan struct{})
	go func() { _, _ = io.Copy(out, master); close(copied) }()
	t.Cleanup(func() { _ = terminal.Close(); _ = master.Close(); <-copied })
	progress.SetTotal(4)
	clock.Advance(collectivePullProgressFloor)
	progress.Advance(1)
	testwait.Receive(t, out.seen, "the progress bar rendered done/total on a terminal")
	rendered := out.String()
	if !strings.Contains(rendered, "1/4") {
		t.Errorf("terminal bar must show done/total 1/4; got %q", rendered)
	}
	if !strings.Contains(rendered, "elapsed:") {
		t.Errorf("terminal bar must carry the elapsed run time; got %q", rendered)
	}
	if !strings.Contains(rendered, "█") && !strings.Contains(rendered, "░") {
		t.Errorf("terminal bar must carry bar cells; got %q", rendered)
	}
}

// TestCollectivePullProgressThrottlesRapidAdvances proves a burst of
// completions writes one periodic refresh instead of one panel per
// transcript: with the clock frozen past the initial bar, intermediate
// advances move only the counts, and the floor passing renders once with
// the reached count and the elapsed run time.
func TestCollectivePullProgressThrottlesRapidAdvances(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	clock := &progressTestClock{now: time.Now()}
	progress := newCollectivePullProgressAt(&stderr, false, theme.New(theme.ModeDark), clock.Now)
	progress.enabled = true
	progress.SetTotal(100)
	initial := stderr.String()
	if !strings.Contains(initial, "0/100") {
		t.Fatalf("initial bar must show 0/100; got %q", initial)
	}
	progress.Advance(1)
	progress.Advance(2)
	progress.Advance(50)
	if got := stderr.String(); got != initial {
		t.Errorf("rapid advances must not write between floor refreshes; initial %d bytes grew to %d", len(initial), len(got))
	}
	clock.Advance(collectivePullProgressFloor)
	progress.Advance(51)
	refreshed := stderr.String()
	if !strings.Contains(refreshed, "51/100") {
		t.Errorf("floor refresh must show the reached 51/100; got %q", refreshed)
	}
	if !strings.Contains(refreshed, "elapsed: 5s") {
		t.Errorf("floor refresh must carry the elapsed run time; got %q", refreshed)
	}
	if n := strings.Count(refreshed, "collective pull progress"); n != 2 {
		t.Errorf("one initial plus one refresh must render exactly 2 panels, got %d", n)
	}
}

// TestCollectivePullProgressFinalAdvanceAlwaysRenders proves a fast run
// still leaves its completed bar on stderr: the final advance renders even
// though the floor interval never passed.
func TestCollectivePullProgressFinalAdvanceAlwaysRenders(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	clock := &progressTestClock{now: time.Now()}
	progress := newCollectivePullProgressAt(&stderr, false, theme.New(theme.ModeDark), clock.Now)
	progress.enabled = true
	progress.SetTotal(4)
	progress.Advance(1)
	progress.Advance(2)
	progress.Advance(3)
	before := stderr.String()
	if n := strings.Count(before, "collective pull progress"); n != 1 {
		t.Fatalf("throttled advances must leave only the initial panel, got %d", n)
	}
	progress.Advance(4)
	final := stderr.String()
	if !strings.Contains(final, "4/4") {
		t.Errorf("final advance must show the completed 4/4; got %q", final)
	}
	if n := strings.Count(final, "collective pull progress"); n != 2 {
		t.Errorf("initial plus final must render exactly 2 panels, got %d", n)
	}
}

// TestCollectivePullProgressCancelAlwaysRenders proves an interrupt renders
// its final state even though the floor interval never passed: the
// terminal keeps the reached counts beside the cancellation report.
func TestCollectivePullProgressCancelAlwaysRenders(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	clock := &progressTestClock{now: time.Now()}
	progress := newCollectivePullProgressAt(&stderr, false, theme.New(theme.ModeDark), clock.Now)
	progress.enabled = true
	progress.SetTotal(4)
	progress.Advance(1)
	clock.Advance(2 * time.Second)
	progress.Cancel()
	rendered := stderr.String()
	if !strings.Contains(rendered, "pull canceled") {
		t.Errorf("cancel must render the canceled footer; got %q", rendered)
	}
	if !strings.Contains(rendered, "1/4") {
		t.Errorf("cancel must retain the reached 1/4; got %q", rendered)
	}
	if !strings.Contains(rendered, "elapsed: 2s") {
		t.Errorf("cancel must retain the elapsed run time; got %q", rendered)
	}
}

// testVillagePullCollectiveProgressSuppressedOffTerminal proves no bar is
// written when stderr is not a terminal. The per-transcript lines and the
// closing summary keep printing because they travel on stdout.
func testVillagePullCollectiveProgressSuppressedOffTerminal(t *testing.T) {
	t.Helper()
	var stderr bytes.Buffer
	progress := newCollectivePullProgress(&stderr, false, theme.New(theme.ModeDark))
	if progress.Enabled() {
		t.Error("progress must stay silent when stderr is not a terminal")
	}
	progress.SetTotal(3)
	progress.Advance(1)
	progress.Advance(2)
	if stderr.Len() != 0 {
		t.Errorf("non-terminal stderr must receive no bar bytes, got %q", stderr.String())
	}

	dir := t.TempDir()
	server := newPullCollectiveDouble(t)
	ids := pullCollectiveTestIDs(2)
	seedPullTranscripts(t, server, ids, pullCollectiveAID)
	writeCredsAt(t, dir, server.URL())

	out, err := runVillage(t, dir, "transcripts", "pull", "--collective", pullCollectiveAName)
	if err != nil {
		t.Fatalf("suppressed-progress pull must exit 0, got err=%v out=%q", err, out)
	}
	if strings.Contains(out, "█") || strings.Contains(out, "░") {
		t.Errorf("non-terminal run must print no bar cells; out=%q", out)
	}
	if !strings.Contains(out, "pulled 2") || !strings.Contains(out, "failed 0") {
		t.Errorf("per-transcript lines and summary must keep printing without a terminal; out=%q", out)
	}
}

// testVillagePullCollectiveProgressSuppressedUnderJSON proves --json forces
// the bar off even on a terminal, and the aggregate stays machine-parseable.
func testVillagePullCollectiveProgressSuppressedUnderJSON(t *testing.T) {
	t.Helper()
	_, terminal := openTestTerminal(t)
	progress := newCollectivePullProgress(terminal, true, theme.New(theme.ModeDark))
	if progress.Enabled() {
		t.Error("progress must stay silent under --json even on a terminal")
	}

	dir := t.TempDir()
	server := newPullCollectiveDouble(t)
	ids := pullCollectiveTestIDs(2)
	seedPullTranscripts(t, server, ids, pullCollectiveAID)
	writeCredsAt(t, dir, server.URL())

	out, err := runVillage(t, dir, "transcripts", "pull", "--collective", pullCollectiveAName, "--json")
	if err != nil {
		t.Fatalf("json pull must exit 0, got err=%v out=%q", err, out)
	}
	var parsed jsonPullCollective
	if jErr := json.Unmarshal([]byte(out), &parsed); jErr != nil {
		t.Fatalf("json pull must stay machine-parseable with progress suppressed: %v\nout=%q", jErr, out)
	}
	if parsed.Totals.Pulled != 2 || parsed.Totals.Total != 2 || len(parsed.Results) != 2 {
		t.Errorf("json aggregate must report 2 pulled of 2; got %+v", parsed.Totals)
	}
	if strings.Contains(out, "█") || strings.Contains(out, "░") {
		t.Errorf("json run must print no bar cells; out=%q", out)
	}
}

// cancelAfterFirstPuller wraps the real pipeline and cancels the run context
// after the first transcript lands, so cancellation is deterministic with no
// timing: the second transcript never starts.
type cancelAfterFirstPuller struct {
	inner  collectivePuller
	cancel context.CancelFunc
	calls  int
}

func (s *cancelAfterFirstPuller) PullTranscript(ctx context.Context, ref pull.TranscriptRef, opts pull.PullOptions) (*pull.PullResult, error) {
	s.calls++
	result, err := s.inner.PullTranscript(ctx, ref, opts)
	if s.calls == 1 {
		s.cancel()
	}
	return result, err
}

// runCancelOnce seeds three transcripts, pulls with a pipeline that cancels
// after the first lands, and returns the sandbox with the interrupted run's
// stdout, stderr, and error. Each cancel case starts from this one
// deterministic interruption.
func runCancelOnce(t *testing.T) (string, *testutil.CollectiveVillage, []schema.TranscriptID, string, string, error) {
	t.Helper()
	dir := t.TempDir()
	server := newPullCollectiveDouble(t)
	ids := pullCollectiveTestIDs(3)
	seedPullTranscripts(t, server, ids, pullCollectiveAID)

	db, err := openPreparedStore(t, string(defaults.ResolveDBFilePathWith(dir)))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	client := village.NewVillageClient(server.URL(), "test-api-key", nil)
	pullsRoot := string(defaults.ResolveVillagePullsDirPathWith(dir))
	realPipeline := pull.NewPipeline(client, &ingest.OSFileSystem{}, db,
		pull.SystemClock{},
		pull.Credentials{UserID: testutil.TestOwnerUserID, VillageURL: server.URL()},
		pullsRoot)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stub := &cancelAfterFirstPuller{inner: realPipeline, cancel: cancel}
	cmd := &cobra.Command{Use: "pull"}
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetContext(ctx)
	runErr := runVillageTranscriptsPullCollectives(cmd, db, client, stub,
		[]string{pullCollectiveAName}, collectivePullFlags{})
	return dir, server, ids, stdout.String(), stderr.String(), runErr
}

// testVillagePullCollectiveCancelReports proves an interrupt stops the run
// cleanly, reports cancelled after N of M, and exits non-zero.
func testVillagePullCollectiveCancelReports(t *testing.T) {
	t.Helper()
	_, _, _, _, _, runErr := runCancelOnce(t)
	if runErr == nil {
		t.Fatal("a cancelled bulk pull must exit non-zero")
	}
	if !strings.Contains(runErr.Error(), "cancelled after 1 of 3") {
		t.Errorf("cancellation must report cancelled after 1 of 3; got: %v", runErr)
	}
	if got := exitCodeFor(runErr); got != defaults.ExitFailure {
		t.Errorf("cancellation exit code = %v, want %v", got, defaults.ExitFailure)
	}
}

// testVillagePullCollectiveCancelNoPartial proves cancellation leaves no
// partial transcript on disk: the interrupted transcript has no pull
// directory and no staging directory, while the earlier transcript stays
// landed. The pipeline removes its staging directory on any failure before
// the write stage; this asserts that mechanism, it does not re-implement it.
func testVillagePullCollectiveCancelNoPartial(t *testing.T) {
	t.Helper()
	dir, server, ids, _, _, runErr := runCancelOnce(t)
	if runErr == nil {
		t.Fatal("a cancelled bulk pull must exit non-zero")
	}
	if n := countPulledRows(t, dir); n != 1 {
		t.Errorf("cancelled run must keep exactly the 1 landed transcript, got %d rows", n)
	}
	landed := pullDirFor(t, dir, server.URL(), ids[0])
	if _, statErr := os.Stat(landed); statErr != nil {
		t.Errorf("the transcript that landed before the interrupt must stay on disk (%s): %v", landed, statErr)
	}
	for _, id := range ids[1:] {
		if _, statErr := os.Stat(pullDirFor(t, dir, server.URL(), id)); !os.IsNotExist(statErr) {
			t.Errorf("interrupted transcript %s must leave no pull directory (stat err=%v)", id, statErr)
		}
	}
	host := strings.TrimPrefix(server.URL(), "http://")
	staging, globErr := filepath.Glob(filepath.Join(string(defaults.ResolveVillagePullsDirPathWith(dir)), host, defaults.TempDirPrefix+"*"))
	if globErr != nil {
		t.Fatalf("list staging directories: %v", globErr)
	}
	if len(staging) != 0 {
		t.Errorf("cancelled run must leave no staging directory, found %v", staging)
	}
}

// testVillagePullCollectiveResumeAfterCancel proves a re-run resumes: the
// current transcript is skipped, so only what is missing repeats.
func testVillagePullCollectiveResumeAfterCancel(t *testing.T) {
	t.Helper()
	dir, server, ids, _, _, runErr := runCancelOnce(t)
	if runErr == nil {
		t.Fatal("a cancelled bulk pull must exit non-zero")
	}
	writeCredsAt(t, dir, server.URL())
	out, err := runVillage(t, dir, "transcripts", "pull", "--collective", pullCollectiveAName)
	if err != nil {
		t.Fatalf("resume pull must exit 0, got err=%v out=%q", err, out)
	}
	if n := countPulledRows(t, dir); n != 3 {
		t.Errorf("resume must land all 3 transcripts, got %d rows (out=%q)", n, out)
	}
	if got := server.ContentCalls(ids[0]); got != 1 {
		t.Errorf("resume must skip the current transcript %s, got %d content calls (want 1: first run only)", ids[0], got)
	}
	for _, id := range ids[1:] {
		if got := server.ContentCalls(id); got != 1 {
			t.Errorf("resume must download missing transcript %s once, got %d content calls", id, got)
		}
	}
	if !strings.Contains(out, "failed 0") {
		t.Errorf("resume summary must report 0 failed; out=%q", out)
	}
}

// testVillagePullCollectiveKitGate proves the progress surface composes
// through the kit primitives only: no file under
// internal/tui/pullprogress pads, places, aligns, or paints a background
// itself.
func testVillagePullCollectiveKitGate(t *testing.T) {
	t.Helper()
	matches, err := gates.ScanForHandRolledLayout(testutil.ModuleRoot(t))
	if err != nil {
		t.Fatalf("scan for hand-rolled layout: %v", err)
	}
	for _, m := range matches {
		if strings.HasPrefix(m.Path, "internal/tui/pullprogress/") {
			t.Errorf("pullprogress surface pads or paints by hand: %+v", m)
		}
	}
}
