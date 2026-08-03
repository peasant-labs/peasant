package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/pull"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// This file drives the THREE village-contacting command bodies (transcripts pull,
// transcripts list [remote], annotations sync) to COMPLETION against a stubbed
// village (httptest), beyond the auth-gate boundary the auth-matrix tests cover.
// It reuses the repository pattern: an httptest server serving
// the pull GETs (mirroring internal/village/pull_test.go's pullTestServer endpoint
// shapes) + credentials.json pointed at the server URL (the credential-seeding
// pattern from the auth-matrix test).

// --- stub village ----------------------------------------------------------

const (
	// stubOwnerUserID is the village user the seeded creds authenticate as; the
	// listed transcripts are owned by this user so the sync path scans them.
	stubOwnerUserID = "user-1"
	// stubForeignAuthorID authors a FOREIGN annotation (different from the
	// requester) — it must LAND on sync. stubOwnerUserID authors an OWN annotation
	// that must be EXCLUDED.
	stubForeignAuthorID = "user-foreign-2"
)

// stubSchemaVersionBody is the /api/v1/schema/version response advertising a pull
// window EQUAL to this CLI's pull-contract version — the happy NegotiatePull
// preflight. A version mismatch is exercised separately (notFound/contract tests
// drive distinct endpoints).
func stubSchemaVersionBody() string {
	v := defaults.PullContractVersion.String()
	return `{"annotationSchemaVersion":"16","supportedTargetKinds":["session"],` +
		`"supportedTypeIds":["t1"],"pushContractVersion":"` + v + `",` +
		`"minPushContractVersion":"` + v + `","pullContractVersion":"` + v + `",` +
		`"minPullContractVersion":"` + v + `"}`
}

// stubBlob is a valid TranscriptContent envelope the content endpoint serves.
func stubBlob(t *testing.T, id schema.TranscriptID) []byte {
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
	b, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal stub blob: %v", err)
	}
	return b
}

// villageStub configures a stubbed village httptest server. notFound makes the
// per-transcript meta + content GETs 404 (driving the PullStatusNotFound →
// non-zero exit mapping); contractTooOld makes NegotiatePull advertise no pull
// window (driving the PullStatusContractError mapping).
type villageStub struct {
	notFound       bool
	contractTooOld bool
}

// start spins the httptest server serving the pull GETs the pipeline drives:
// NegotiatePull (/schema/version), the listing, per-transcript meta + content +
// annotations. The listing returns ONE own transcript (id), and the annotations
// endpoint returns a mix of one FOREIGN and one OWN annotation so the sync path's
// own-author exclusion is observable.
func (s villageStub) start(t *testing.T, id schema.TranscriptID) *httptest.Server {
	t.Helper()
	blob := stubBlob(t, id)
	blobHash := schema.ComputeTranscriptHash(blob)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/schema/version":
			if s.contractTooOld {
				// Omit the pull window entirely ⇒ NegotiatePull aborts with a
				// contract error (the village predates the pull surface).
				_, _ = w.Write([]byte(`{"annotationSchemaVersion":"16",` +
					`"supportedTargetKinds":["session"],"supportedTypeIds":["t1"],` +
					`"pushContractVersion":"0.1.0","minPushContractVersion":"0.1.0"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(stubSchemaVersionBody()))

		case r.URL.Path == "/api/v1/pull/transcripts":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(schema.PullListResponse{
				Transcripts: []schema.PullTranscriptInfo{{
					TranscriptID:    id,
					OwnerUserID:     stubOwnerUserID,
					OwnerUsername:   "owneruser",
					Title:           "Build the thing",
					Harness:         defaults.HarnessClaudeCode,
					Visibility:      schema.VisibilityGroup,
					License:         schema.LicenseCCBY,
					AnnotationCount: 2,
				}},
				Page: 1, Limit: 50, Total: 1,
			})

		case r.URL.Path == "/api/v1/pull/transcripts/"+id.String():
			if s.notFound {
				http.Error(w, "not pullable", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(schema.PullTranscriptInfo{
				TranscriptID:  id,
				OwnerUserID:   stubOwnerUserID,
				OwnerUsername: "owneruser",
				Title:         "Build the thing",
				Harness:       defaults.HarnessClaudeCode,
				Visibility:    schema.VisibilityGroup,
				License:       schema.LicenseCCBY,
				ContentHash:   blobHash,
			})

		case r.URL.Path == "/api/v1/pull/transcripts/"+id.String()+"/content":
			if s.notFound {
				http.Error(w, "gone", http.StatusNotFound)
				return
			}
			w.Header().Set("ETag", `"`+blobHash+`"`)
			_, _ = w.Write(blob)

		case r.URL.Path == "/api/v1/pull/transcripts/"+id.String()+"/annotations":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(stubAnnotations())

		default:
			t.Errorf("unexpected request path %q", r.URL.Path)
			http.Error(w, "unexpected path", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// stubAnnotations returns a mix of one FOREIGN annotation (author != requester,
// must LAND) and one OWN annotation (author == requester, must be EXCLUDED) so the
// sync path's own-author exclusion is observable.
func stubAnnotations() []schema.PullAnnotation {
	foreignHash := testutil.TestContentHash
	ownHash := testutil.TestContentHash2
	return []schema.PullAnnotation{
		{
			AnnotationSummary: schema.AnnotationSummary{ID: "ann-foreign", ContentHash: &foreignHash},
			AuthorUserID:      stubForeignAuthorID,
			AuthorUsername:    "bob",
		},
		{
			AnnotationSummary: schema.AnnotationSummary{ID: "ann-own", ContentHash: &ownHash},
			AuthorUserID:      stubOwnerUserID,
			AuthorUsername:    "owneruser",
		},
	}
}

// writeCredsAt writes a valid credentials.json whose village_url points at
// villageURL and whose user_id is stubOwnerUserID (so sync scans the listed own
// transcripts). This is the credential-seeding pattern from the auth-matrix test,
// parameterized on the (reachable) stub URL.
func writeCredsAt(t *testing.T, dir, villageURL string) {
	t.Helper()
	peasantDir := string(defaults.ResolveConfigDirPathWith(dir))
	if err := os.MkdirAll(peasantDir, 0o700); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	creds := `{
		"api_key": "test-api-key",
		"key_id": "test-key-id",
		"user_id": "` + stubOwnerUserID + `",
		"username": "owneruser",
		"village_url": "` + villageURL + `",
		"linked_at": "2025-01-01T00:00:00Z"
	}`
	if err := os.WriteFile(filepath.Join(peasantDir, string(defaults.CredentialsFile)), []byte(creds), 0o600); err != nil {
		t.Fatalf("write creds: %v", err)
	}
}

// pullDirFor returns the on-disk pull directory a transcript lands in under dir,
// mirroring the pipeline's {pullsRoot}/{villageHost}/{id} layout. The host is the
// httptest server URL's host:port.
func pullDirFor(t *testing.T, dir, villageURL string, id schema.TranscriptID) string {
	t.Helper()
	host := strings.TrimPrefix(villageURL, "http://")
	pullsRoot := string(defaults.ResolveVillagePullsDirPathWith(dir))
	return filepath.Join(pullsRoot, host, id.String())
}

// countPulledRows opens the store under dir and returns the number of
// pulled_transcripts rows — used to assert ZERO DB mutation in the dry-run test.
func countPulledRows(t *testing.T, dir string) int {
	t.Helper()
	db, err := store.Open(string(defaults.ResolveDBFilePathWith(dir)))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	rows, err := db.ListPulledTranscripts(t.Context())
	if err != nil {
		t.Fatalf("list pulled transcripts: %v", err)
	}
	return len(rows)
}

// --- (a) pull happy path ---------------------------------------------------

// TestVillagePull_HappyPath_StubVillage drives `transcripts pull` to completion
// against the stub village: exit 0, the blob + manifest land on disk, a
// pulled_transcripts row is written, and the human summary reports the pull.
func TestVillagePull_HappyPath_StubVillage(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	id := schema.TranscriptID(testutil.TestSessionUUID)
	srv := villageStub{}.start(t, id)
	writeCredsAt(t, dir, srv.URL)

	out, err := runVillage(t, dir, "transcripts", "pull", id.String())
	if err != nil {
		t.Fatalf("pull happy path must exit 0, got err=%v out=%q", err, out)
	}

	// Summary on stdout names the pulled transcript.
	if !strings.Contains(out, "pulled") || !strings.Contains(out, id.String()) {
		t.Errorf("summary must report the pulled transcript; out=%q", out)
	}

	// Files landed on disk.
	pullDir := pullDirFor(t, dir, srv.URL, id)
	for _, name := range []string{pull.TranscriptFilename, pull.ManifestFilename} {
		if _, statErr := os.Stat(filepath.Join(pullDir, name)); statErr != nil {
			t.Errorf("expected %s in pull dir %s; stat err=%v", name, pullDir, statErr)
		}
	}

	// A pulled_transcripts row landed.
	if n := countPulledRows(t, dir); n != 1 {
		t.Errorf("expected exactly 1 pulled_transcripts row, got %d", n)
	}
}

// --- (b) pull --dry-run ⇒ zero mutation -------------------------------------

// TestVillagePull_DryRun_ZeroMutation proves `pull --dry-run` against a
// reachable stub village exits 0, reports a would-pull, and writes NOTHING — no
// files, no DB rows.
func TestVillagePull_DryRun_ZeroMutation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	id := schema.TranscriptID(testutil.TestSessionUUID)
	srv := villageStub{}.start(t, id)
	writeCredsAt(t, dir, srv.URL)

	out, err := runVillage(t, dir, "transcripts", "pull", id.String(), "--dry-run")
	if err != nil {
		t.Fatalf("pull --dry-run must exit 0, got err=%v out=%q", err, out)
	}

	// Would-pull verb in the human summary.
	if !strings.Contains(out, "would pull") {
		t.Errorf("dry-run must report 'would pull'; out=%q", out)
	}

	// ZERO file mutation: the pull dir must not exist.
	pullDir := pullDirFor(t, dir, srv.URL, id)
	if _, statErr := os.Stat(pullDir); !os.IsNotExist(statErr) {
		t.Errorf("dry-run must write NO files; pull dir %s exists (stat err=%v)", pullDir, statErr)
	}

	// ZERO DB mutation: no pulled_transcripts rows.
	if n := countPulledRows(t, dir); n != 0 {
		t.Errorf("dry-run must write NO DB rows, got %d pulled_transcripts rows", n)
	}
}

// --- (c) pull --json + sync --json parseable + dryRun marker ----------------

// TestVillagePull_JSON_Parseable asserts `pull --json` emits parseable output
// surfacing the typed status.
func TestVillagePull_JSON_Parseable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	id := schema.TranscriptID(testutil.TestSessionUUID)
	srv := villageStub{}.start(t, id)
	writeCredsAt(t, dir, srv.URL)

	out, err := runVillage(t, dir, "transcripts", "pull", id.String(), "--json")
	if err != nil {
		t.Fatalf("pull --json must exit 0, got err=%v out=%q", err, out)
	}
	var parsed jsonPullResult
	if jErr := json.Unmarshal([]byte(out), &parsed); jErr != nil {
		t.Fatalf("pull --json must be parseable: %v\nout=%q", jErr, out)
	}
	if parsed.TranscriptID != id.String() {
		t.Errorf("json transcriptId = %q, want %q", parsed.TranscriptID, id)
	}
	if parsed.Status != pull.PullStatusPulled.String() {
		t.Errorf("json status = %q, want %q", parsed.Status, pull.PullStatusPulled)
	}
	// A real pull is not marked dry-run.
	if parsed.DryRun {
		t.Errorf("real pull --json must not set dryRun=true; out=%q", out)
	}
	if parsed.License != string(schema.LicenseCCBY) {
		t.Errorf("json license = %q, want %q (served license must reach the receipt)", parsed.License, schema.LicenseCCBY)
	}
}

// TestVillagePull_DryRunJSON_HasDryRunMarker asserts `pull --dry-run --json`
// surfaces the dryRun marker so a machine consumer can distinguish would-pull from
// an actual pull. It also re-confirms zero mutation.
func TestVillagePull_DryRunJSON_HasDryRunMarker(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	id := schema.TranscriptID(testutil.TestSessionUUID)
	srv := villageStub{}.start(t, id)
	writeCredsAt(t, dir, srv.URL)

	out, err := runVillage(t, dir, "transcripts", "pull", id.String(), "--dry-run", "--json")
	if err != nil {
		t.Fatalf("pull --dry-run --json must exit 0, got err=%v out=%q", err, out)
	}
	var parsed jsonPullResult
	if jErr := json.Unmarshal([]byte(out), &parsed); jErr != nil {
		t.Fatalf("pull --dry-run --json must be parseable: %v\nout=%q", jErr, out)
	}
	if !parsed.DryRun {
		t.Errorf("dry-run --json must set dryRun=true so it is distinguishable from a real pull; out=%q", out)
	}

	// Still zero mutation under --json.
	if n := countPulledRows(t, dir); n != 0 {
		t.Errorf("dry-run --json must write NO DB rows, got %d", n)
	}
}

// --- (d) sync happy path: foreign lands, own excluded -----------------------

// TestVillageSync_HappyPath_ForeignLandsOwnExcluded drives `annotations sync` to
// completion against the stub: exit 0, the FOREIGN annotation is created, the OWN
// annotation is excluded, and the summary reports the counts.
func TestVillageSync_HappyPath_ForeignLandsOwnExcluded(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	id := schema.TranscriptID(testutil.TestSessionUUID)
	srv := villageStub{}.start(t, id)
	writeCredsAt(t, dir, srv.URL)

	out, err := runVillage(t, dir, "annotations", "sync", "--verbose")
	if err != nil {
		t.Fatalf("sync happy path must exit 0, got err=%v out=%q", err, out)
	}
	// One foreign annotation created; the verbose line reports one own-authored
	// exclusion. The stub returns exactly one of each.
	if !strings.Contains(out, "1 created") {
		t.Errorf("sync must report 1 created (the foreign annotation); out=%q", out)
	}
	if !strings.Contains(out, "own-authored excluded: 1") {
		t.Errorf("sync --verbose must report 1 own-authored exclusion; out=%q", out)
	}
}

// TestVillageSync_JSON_Parseable asserts `sync --json` emits parseable output with
// the typed status and the created/excluded counts.
func TestVillageSync_JSON_Parseable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	id := schema.TranscriptID(testutil.TestSessionUUID)
	srv := villageStub{}.start(t, id)
	writeCredsAt(t, dir, srv.URL)

	out, err := runVillage(t, dir, "annotations", "sync", "--json")
	if err != nil {
		t.Fatalf("sync --json must exit 0, got err=%v out=%q", err, out)
	}
	var parsed jsonSyncResult
	if jErr := json.Unmarshal([]byte(out), &parsed); jErr != nil {
		t.Fatalf("sync --json must be parseable: %v\nout=%q", jErr, out)
	}
	if parsed.Created != 1 {
		t.Errorf("sync --json created = %d, want 1 (foreign annotation)", parsed.Created)
	}
	if parsed.Excluded != 1 {
		t.Errorf("sync --json excluded = %d, want 1 (own-authored)", parsed.Excluded)
	}
	if parsed.Status != pull.PullStatusPulled.String() {
		t.Errorf("sync --json status = %q, want %q", parsed.Status, pull.PullStatusPulled)
	}
}

// --- list remote happy path -------------------------------------------------

// TestVillageListRemote_HappyPath drives `transcripts list` (remote) to completion
// against the stub: exit 0 and the listed transcript appears in the table.
func TestVillageListRemote_HappyPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	id := schema.TranscriptID(testutil.TestSessionUUID)
	srv := villageStub{}.start(t, id)
	writeCredsAt(t, dir, srv.URL)

	out, err := runVillage(t, dir, "transcripts", "list")
	if err != nil {
		t.Fatalf("list remote happy path must exit 0, got err=%v out=%q", err, out)
	}
	if !strings.Contains(out, id.String()) {
		t.Errorf("remote list must show the listed transcript; out=%q", out)
	}
	if !strings.Contains(out, "LICENSE") || !strings.Contains(out, string(schema.LicenseCCBY)) {
		t.Errorf("remote list must show the LICENSE column with the served license; out=%q", out)
	}
}

// --- (e) PullStatus → exit-code mapping (distinct non-zero messages) --------

// TestVillagePull_NotFound_NonZeroExit asserts a 404 from the stub maps to a
// non-zero exit with the actionable not-found message (PullStatusNotFound).
func TestVillagePull_NotFound_NonZeroExit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	id := schema.TranscriptID(testutil.TestSessionUUID)
	srv := villageStub{notFound: true}.start(t, id)
	writeCredsAt(t, dir, srv.URL)

	out, err := runVillage(t, dir, "transcripts", "pull", id.String())
	if err == nil {
		t.Fatalf("not-found pull must exit non-zero; out=%q", out)
	}
	if !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "not pullable") {
		t.Errorf("not-found error must be actionable (404 / not pullable); got: %v", err)
	}
	// No mutation on a failed pull.
	if n := countPulledRows(t, dir); n != 0 {
		t.Errorf("failed pull must write NO DB rows, got %d", n)
	}
}

// TestVillagePull_ContractError_NonZeroExit asserts an old village (no pull
// window) maps to a non-zero exit with a DISTINCT contract-error message
// (PullStatusContractError) — different from the not-found message.
func TestVillagePull_ContractError_NonZeroExit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	id := schema.TranscriptID(testutil.TestSessionUUID)
	srv := villageStub{contractTooOld: true}.start(t, id)
	writeCredsAt(t, dir, srv.URL)

	out, err := runVillage(t, dir, "transcripts", "pull", id.String())
	if err == nil {
		t.Fatalf("contract-too-old pull must exit non-zero; out=%q", out)
	}
	// Distinct from not-found: names the pull-contract incompatibility, not the
	// semantic marker used by the 404 response. Do not search for the digits
	// "404" alone because the random test-server port can contain them.
	if !strings.Contains(err.Error(), "pull contract") {
		t.Errorf("contract error must name the pull contract; got: %v", err)
	}
	if strings.Contains(err.Error(), "not pullable") {
		t.Errorf("contract error must be distinct from the not-found message; got: %v", err)
	}
	if n := countPulledRows(t, dir); n != 0 {
		t.Errorf("failed pull must write NO DB rows, got %d", n)
	}
}
