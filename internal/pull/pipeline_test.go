package pull_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/pull"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
)

// Compile-time guards: the shared testutil doubles satisfy the pipeline's
// consumer-side dependency interfaces (declared in internal/pull). A signature
// drift on either side breaks the build here.
var (
	_ pull.VillageReader = (*testutil.StubVillageReader)(nil)
	_ pull.PullStore     = (*testutil.StubPullStore)(nil)
	_ pull.Clock         = testutil.FixedClock{}
)

// testVillageURL is a var (not const) because testutil.TestVillageHost is now a
// fixture re-export (a package var), so a "https://"+host expression is no longer
// a constant expression.
var testVillageURL = "https://" + testutil.TestVillageHost

const (
	testPullsRoot   = "/data/village-pulls"
	testBlobContent = `{"contractVersion":"0.1.0","kind":"session_detail","sessionDetail":{}}`
)

// testCreds returns logged-in credentials whose UserID is the requester's OWN id
// (TestPullAuthorUserID) — distinct from the foreign annotation author
// (TestAuthorUserID).
func testCreds() pull.Credentials {
	return pull.Credentials{UserID: testutil.TestPullAuthorUserID, VillageURL: testVillageURL}
}

// happyMeta returns a server metadata view with a non-empty served-blob hash.
func happyMeta() *schema.PullTranscriptInfo {
	return &schema.PullTranscriptInfo{
		TranscriptID:    testutil.TestTranscriptID,
		LocalID:         testutil.TestSessionUUID,
		OwnerUserID:     testutil.TestPullAuthorUserID, // requester owns it (refresh path)
		OwnerUsername:   "self",
		Title:           "Fix the bug",
		Harness:         schema.Harness(defaults.HarnessClaudeCode),
		ProjectName:     testutil.TestProjectName,
		Visibility:      schema.VisibilityGroup,
		License:         schema.LicenseCCBYSA,
		ContentHash:     testutil.TestContentHash,
		ContractVersion: defaults.PublishSchemaVersion,
		PublishedAt:     1000,
		UpdatedAt:       2000,
		AnnotationCount: 1,
	}
}

// foreignAnnotation returns an annotation authored by someone OTHER than the
// requester (TestAuthorUserID != TestPullAuthorUserID).
func foreignAnnotation(hash string) schema.PullAnnotation {
	h := hash
	return schema.PullAnnotation{
		AnnotationSummary: schema.AnnotationSummary{
			ID:          "annot-" + hash,
			TargetKind:  schema.TargetSession,
			TypeID:      testutil.TestTypeIDSessionApproval,
			Value:       "approve",
			ContentHash: &h,
		},
		AuthorUserID:   testutil.TestAuthorUserID,
		AuthorUsername: testutil.TestAuthorUsername,
	}
}

// quotedETag mirrors the REAL village's ETag emission: the village handler sets
// fmt.Sprintf("%q", hash) ⇒ a DOUBLE-QUOTED hash string. The pull pipeline must
// strip the quotes to derive the RAW content-identity hash while echoing the
// verbatim quoted token as If-None-Match. Tests use this so the stub reflects prod
// semantics (a raw-hash stub masked the servedETag/servedBlobHash split).
func quotedETag(rawHash string) string {
	return fmt.Sprintf("%q", rawHash)
}

// happyReader returns a StubVillageReader wired for a successful single-transcript
// pull (one foreign annotation). ContentETag is QUOTED like the real village.
func happyReader() *testutil.StubVillageReader {
	return &testutil.StubVillageReader{
		Meta:        happyMeta(),
		ContentBody: []byte(testBlobContent),
		ContentETag: quotedETag(testutil.TestContentHash),
		Annotations: []schema.PullAnnotation{foreignAnnotation(testutil.TestContentHash2)},
	}
}

func newRef(t *testing.T) pull.TranscriptRef {
	t.Helper()
	ref, err := pull.ParseTranscriptRef(testutil.TestTranscriptUUID)
	if err != nil {
		t.Fatalf("ParseTranscriptRef: %v", err)
	}
	return ref
}

// --- Happy path ---

func TestPullTranscript_HappyPath(t *testing.T) {
	fs := testutil.NewMemFS()
	reader := happyReader()
	st := &testutil.StubPullStore{}
	p := pull.NewPipeline(reader, fs, st, testutil.NewFixedClock(), testCreds(), testPullsRoot)

	res, err := p.PullTranscript(context.Background(), newRef(t), pull.PullOptions{})
	if err != nil {
		t.Fatalf("PullTranscript: %v", err)
	}

	// Status + report.
	if res.Status != pull.PullStatusPulled {
		t.Errorf("Status = %q, want pulled", res.Status)
	}
	if res.VillageHost != testutil.TestVillageHost {
		t.Errorf("VillageHost = %q, want %q", res.VillageHost, testutil.TestVillageHost)
	}
	if res.AnnotationCount != 1 {
		t.Errorf("AnnotationCount = %d, want 1", res.AnnotationCount)
	}
	if res.ServedBlobHash != testutil.TestContentHash {
		t.Errorf("ServedBlobHash = %q, want %q", res.ServedBlobHash, testutil.TestContentHash)
	}
	if res.License != schema.LicenseCCBYSA {
		t.Errorf("License = %q, want %q (result must carry the served license)", res.License, schema.LicenseCCBYSA)
	}

	// NEGOTIATE exactly once.
	if reader.NegotiateCalls != 1 {
		t.Errorf("NegotiateCalls = %d, want exactly 1", reader.NegotiateCalls)
	}

	wantDir := filepath.Join(testPullsRoot, testutil.TestVillageHost, testutil.TestTranscriptUUID)
	if res.PullDir != wantDir {
		t.Errorf("PullDir = %q, want %q", res.PullDir, wantDir)
	}

	// Files landed in MemFS.
	blob, err := fs.ReadFile(filepath.Join(wantDir, pull.TranscriptFilename))
	if err != nil {
		t.Fatalf("read transcript blob: %v", err)
	}
	if string(blob) != testBlobContent {
		t.Errorf("blob content = %q, want %q", string(blob), testBlobContent)
	}
	// Metadata snapshot CONTENT (not just existence): the decoded snapshot must
	// equal the village PullTranscriptInfo the pipeline fetched.
	metaBytes, err := fs.ReadFile(filepath.Join(wantDir, pull.MetadataFilename))
	if err != nil {
		t.Fatalf("metadata snapshot missing: %v", err)
	}
	var snap schema.PullTranscriptInfo
	if err := json.Unmarshal(metaBytes, &snap); err != nil {
		t.Fatalf("decode metadata snapshot: %v", err)
	}
	if snap.TranscriptID != testutil.TestTranscriptID {
		t.Errorf("snapshot TranscriptID = %q, want %q", snap.TranscriptID, testutil.TestTranscriptID)
	}
	if snap.LocalID != testutil.TestSessionUUID {
		t.Errorf("snapshot LocalID = %q, want %q", snap.LocalID, testutil.TestSessionUUID)
	}
	if snap.ContentHash != testutil.TestContentHash {
		t.Errorf("snapshot ContentHash = %q, want %q", snap.ContentHash, testutil.TestContentHash)
	}
	if snap.ContractVersion != defaults.PublishSchemaVersion {
		t.Errorf("snapshot ContractVersion = %q, want %q", snap.ContractVersion, defaults.PublishSchemaVersion)
	}

	// Manifest content.
	manifestBytes, err := fs.ReadFile(filepath.Join(wantDir, pull.ManifestFilename))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var m pull.PullManifest
	if err := json.Unmarshal(manifestBytes, &m); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if m.ManifestVersion != pull.PullManifestVersion {
		t.Errorf("manifest version = %d, want %d", m.ManifestVersion, pull.PullManifestVersion)
	}
	if m.VillageURL != testVillageURL {
		t.Errorf("manifest villageURL = %q, want %q", m.VillageURL, testVillageURL)
	}
	if m.ServedBlobHash != testutil.TestContentHash {
		t.Errorf("manifest servedBlobHash = %q, want %q", m.ServedBlobHash, testutil.TestContentHash)
	}
	if m.OwnerUserID != testutil.TestPullAuthorUserID {
		t.Errorf("manifest ownerUserId = %q", m.OwnerUserID)
	}
	if m.BlobContractVersion != defaults.PublishSchemaVersion {
		t.Errorf("manifest blobContractVersion = %q, want %q", m.BlobContractVersion, defaults.PublishSchemaVersion)
	}
	if m.PullEnvelopeVersion != defaults.PullContractVersion {
		t.Errorf("manifest pullEnvelopeVersion = %q, want %q", m.PullEnvelopeVersion, defaults.PullContractVersion)
	}
	if m.PulledAt != testutil.TestPulledAtMillis {
		t.Errorf("manifest pulledAt = %d, want %d", m.PulledAt, testutil.TestPulledAtMillis)
	}
	if len(m.Annotations) != 1 || m.Annotations[0].ContentHash != testutil.TestContentHash2 {
		t.Errorf("manifest annotations = %+v", m.Annotations)
	}
	if m.Annotations[0].AuthorUserID != testutil.TestAuthorUserID {
		t.Errorf("manifest annotation author = %q, want %q", m.Annotations[0].AuthorUserID, testutil.TestAuthorUserID)
	}

	// CommitPull payload.
	if len(st.Commits) != 1 {
		t.Fatalf("CommitPull called %d times, want 1", len(st.Commits))
	}
	commit := st.Commits[0]
	tr := commit.Transcript
	if tr.VillageHost != testutil.TestVillageHost || tr.TranscriptID != testutil.TestTranscriptID {
		t.Errorf("commit transcript identity = %+v", tr)
	}
	if tr.ContentHash != testutil.TestContentHash {
		t.Errorf("commit ContentHash = %q, want %q", tr.ContentHash, testutil.TestContentHash)
	}
	if tr.PullDir != wantDir {
		t.Errorf("commit PullDir = %q, want %q", tr.PullDir, wantDir)
	}
	if tr.License != schema.LicenseCCBYSA {
		t.Errorf("commit License = %q, want %q (buildCommit must thread meta.License)", tr.License, schema.LicenseCCBYSA)
	}
	if tr.FirstPulledAt != testutil.TestPulledAtMillis || tr.LastPulledAt != testutil.TestPulledAtMillis {
		t.Errorf("commit pulled-at = (%d,%d)", tr.FirstPulledAt, tr.LastPulledAt)
	}
	if len(commit.Annotations) != 1 {
		t.Fatalf("commit annotations = %d, want 1", len(commit.Annotations))
	}
	if commit.Annotations[0].ContentHash != testutil.TestContentHash2 {
		t.Errorf("commit annotation hash = %q", commit.Annotations[0].ContentHash)
	}
	if commit.Annotations[0].AuthorUserID != testutil.TestAuthorUserID {
		t.Errorf("commit annotation author = %q", commit.Annotations[0].AuthorUserID)
	}
}

// TestPullTranscript_InvalidAssociationAnnotationStopsBeforeLocalMutation
// verifies an invalid association-target annotation is rejected after fetch but
// before the pull pipeline writes files or commits its local DB projection.
func TestPullTranscript_InvalidAssociationAnnotationStopsBeforeLocalMutation(t *testing.T) {
	fs := testutil.NewMemFS()
	reader := happyReader()
	reader.Annotations = []schema.PullAnnotation{{
		AnnotationSummary: schema.AnnotationSummary{
			ID:         "invalid-association-target",
			TargetKind: schema.TargetAssociation,
		},
		AuthorUserID:   testutil.TestAuthorUserID,
		AuthorUsername: testutil.TestAuthorUsername,
	}}
	store := &testutil.StubPullStore{}
	pipeline := pull.NewPipeline(reader, fs, store, testutil.NewFixedClock(), testCreds(), testPullsRoot)

	result, err := pipeline.PullTranscript(context.Background(), newRef(t), pull.PullOptions{})
	if err == nil {
		t.Fatal("PullTranscript with missing association target ID succeeded, want schema validation error")
	}
	if result == nil || result.Status != pull.PullStatusError {
		t.Errorf("PullTranscript result = %+v, want error status", result)
	}
	if len(store.Commits) != 0 {
		t.Errorf("invalid association annotation committed %d local store rows, want 0", len(store.Commits))
	}
	pullDir := filepath.Join(testPullsRoot, testutil.TestVillageHost, testutil.TestTranscriptUUID)
	if _, readErr := fs.ReadFile(filepath.Join(pullDir, pull.TranscriptFilename)); readErr == nil {
		t.Error("invalid association annotation wrote transcript bytes, want no local files")
	}
}

func TestPullTranscript_ByURL(t *testing.T) {
	ref, err := pull.ParseTranscriptRef(testVillageURL + "/transcripts/" + testutil.TestTranscriptUUID)
	if err != nil {
		t.Fatalf("ParseTranscriptRef: %v", err)
	}
	if !ref.FromURL {
		t.Errorf("FromURL = false, want true")
	}

	fs := testutil.NewMemFS()
	st := &testutil.StubPullStore{}
	p := pull.NewPipeline(happyReader(), fs, st, testutil.NewFixedClock(), testCreds(), testPullsRoot)

	res, err := p.PullTranscript(context.Background(), ref, pull.PullOptions{})
	if err != nil {
		t.Fatalf("PullTranscript: %v", err)
	}
	if res.Status != pull.PullStatusPulled {
		t.Errorf("Status = %q, want pulled", res.Status)
	}
	if len(st.Commits) != 1 {
		t.Errorf("CommitPull calls = %d, want 1", len(st.Commits))
	}
}

// --- Up-to-date paths ---

func TestPullTranscript_UpToDate_HashMatch(t *testing.T) {
	fs := testutil.NewMemFS()
	st := &testutil.StubPullStore{}
	clock := testutil.NewFixedClock()
	ref := newRef(t)

	// First pull lands files + a manifest recording the served-blob hash.
	p := pull.NewPipeline(happyReader(), fs, st, clock, testCreds(), testPullsRoot)
	if _, err := p.PullTranscript(context.Background(), ref, pull.PullOptions{}); err != nil {
		t.Fatalf("first pull: %v", err)
	}

	// Second pull: server reports the SAME content hash ⇒ up-to-date via the
	// manifest-hash DIFF, no DOWNLOAD, no new CommitPull.
	reader2 := happyReader()
	p2 := pull.NewPipeline(reader2, fs, st, clock, testCreds(), testPullsRoot)
	res, err := p2.PullTranscript(context.Background(), ref, pull.PullOptions{})
	if err != nil {
		t.Fatalf("second pull: %v", err)
	}
	if res.Status != pull.PullStatusUpToDate {
		t.Errorf("Status = %q, want up-to-date", res.Status)
	}
	if res.License != schema.LicenseCCBYSA {
		t.Errorf("License = %q, want %q (up-to-date result must carry the served license)", res.License, schema.LicenseCCBYSA)
	}
	if reader2.ContentCalls != 0 {
		t.Errorf("ContentCalls = %d, want 0 (no re-download when hash matches)", reader2.ContentCalls)
	}
	if len(st.Commits) != 1 {
		t.Errorf("total CommitPull calls = %d, want 1 (no commit on up-to-date)", len(st.Commits))
	}
}

func TestPullTranscript_UpToDate_Via304(t *testing.T) {
	fs := testutil.NewMemFS()
	st := &testutil.StubPullStore{}
	clock := testutil.NewFixedClock()
	ref := newRef(t)

	// First pull lands a manifest with the served-blob hash.
	if _, err := pull.NewPipeline(happyReader(), fs, st, clock, testCreds(), testPullsRoot).
		PullTranscript(context.Background(), ref, pull.PullOptions{}); err != nil {
		t.Fatalf("first pull: %v", err)
	}

	// Second pull: server omits its content hash in metadata (forces the
	// conditional GET), and the content endpoint answers 304 Not Modified.
	metaNoHash := happyMeta()
	metaNoHash.ContentHash = ""
	reader2 := &testutil.StubVillageReader{
		Meta:       metaNoHash,
		ContentErr: village.ErrNotModified,
	}
	res, err := pull.NewPipeline(reader2, fs, st, clock, testCreds(), testPullsRoot).
		PullTranscript(context.Background(), ref, pull.PullOptions{})
	if err != nil {
		t.Fatalf("second pull: %v", err)
	}
	if res.Status != pull.PullStatusUpToDate {
		t.Errorf("Status = %q, want up-to-date (304)", res.Status)
	}
	if res.License != schema.LicenseCCBYSA {
		t.Errorf("License = %q, want %q (304 result must carry the served license)", res.License, schema.LicenseCCBYSA)
	}
	// The pipeline must have echoed the stored VERBATIM (quoted) ETag as
	// If-None-Match — not the raw hash. This is the transport-token half of the
	// servedETag/servedBlobHash split.
	if reader2.LastIfNoneMatch != quotedETag(testutil.TestContentHash) {
		t.Errorf("If-None-Match = %q, want quoted %q", reader2.LastIfNoneMatch, quotedETag(testutil.TestContentHash))
	}
}

// TestPullTranscript_ETagHashSplit asserts the servedETag/servedBlobHash split
// against a prod-faithful QUOTED ETag: the manifest persists the RAW (unquoted)
// hash as ServedBlobHash, the VERBATIM quoted token as ServedETag, the DB row's
// ContentHash is RAW, and PullResult.ServedBlobHash is RAW.
func TestPullTranscript_ETagHashSplit(t *testing.T) {
	fs := testutil.NewMemFS()
	st := &testutil.StubPullStore{}
	ref := newRef(t)

	res, err := pull.NewPipeline(happyReader(), fs, st, testutil.NewFixedClock(), testCreds(), testPullsRoot).
		PullTranscript(context.Background(), ref, pull.PullOptions{})
	if err != nil {
		t.Fatalf("PullTranscript: %v", err)
	}

	// Result carries the RAW hash, never the quoted ETag.
	if res.ServedBlobHash != testutil.TestContentHash {
		t.Errorf("result ServedBlobHash = %q, want RAW %q", res.ServedBlobHash, testutil.TestContentHash)
	}

	wantDir := filepath.Join(testPullsRoot, testutil.TestVillageHost, testutil.TestTranscriptUUID)
	manifestBytes, err := fs.ReadFile(filepath.Join(wantDir, pull.ManifestFilename))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var m pull.PullManifest
	if err := json.Unmarshal(manifestBytes, &m); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	// servedBlobHash = RAW; servedETag = verbatim QUOTED.
	if m.ServedBlobHash != testutil.TestContentHash {
		t.Errorf("manifest servedBlobHash = %q, want RAW %q", m.ServedBlobHash, testutil.TestContentHash)
	}
	if strings.Contains(m.ServedBlobHash, `"`) {
		t.Errorf("manifest servedBlobHash %q must not carry quote chars", m.ServedBlobHash)
	}
	if m.ServedETag != quotedETag(testutil.TestContentHash) {
		t.Errorf("manifest servedETag = %q, want verbatim quoted %q", m.ServedETag, quotedETag(testutil.TestContentHash))
	}

	// DB row content_hash is RAW (the consistent identity key downstream compares).
	if len(st.Commits) != 1 {
		t.Fatalf("CommitPull calls = %d, want 1", len(st.Commits))
	}
	if st.Commits[0].Transcript.ContentHash != testutil.TestContentHash {
		t.Errorf("commit ContentHash = %q, want RAW %q", st.Commits[0].Transcript.ContentHash, testutil.TestContentHash)
	}
}

// --- DryRun (resolve→negotiate→fetch-meta→diff, then short-circuit) ---

func TestPullTranscript_DryRun_WouldPull(t *testing.T) {
	fs := testutil.NewMemFS()
	st := &testutil.StubPullStore{}
	reader := happyReader()

	res, err := pull.NewPipeline(reader, fs, st, testutil.NewFixedClock(), testCreds(), testPullsRoot).
		PullTranscript(context.Background(), newRef(t), pull.PullOptions{DryRun: true})
	if err != nil {
		t.Fatalf("dry-run pull: %v", err)
	}

	// Would-pull outcome reported, marked as a dry run.
	if res.Status != pull.PullStatusPulled {
		t.Errorf("Status = %q, want pulled (would-pull)", res.Status)
	}
	if !res.DryRun {
		t.Errorf("DryRun = false, want true")
	}
	wantDir := filepath.Join(testPullsRoot, testutil.TestVillageHost, testutil.TestTranscriptUUID)
	if res.PullDir != wantDir {
		t.Errorf("PullDir = %q, want would-be %q", res.PullDir, wantDir)
	}

	// NEGOTIATE + FETCH-META ran exactly once; NO download, NO write, NO commit.
	if reader.NegotiateCalls != 1 {
		t.Errorf("NegotiateCalls = %d, want 1", reader.NegotiateCalls)
	}
	if reader.MetaCalls != 1 {
		t.Errorf("MetaCalls = %d, want 1", reader.MetaCalls)
	}
	if reader.ContentCalls != 0 {
		t.Errorf("ContentCalls = %d, want 0 (dry-run downloads nothing)", reader.ContentCalls)
	}
	if res.License != schema.LicenseCCBYSA {
		t.Errorf("License = %q, want %q (dry-run result must carry the served license)", res.License, schema.LicenseCCBYSA)
	}
	if reader.AnnotationsCalls != 0 {
		t.Errorf("AnnotationsCalls = %d, want 0 (dry-run fetches no annotations)", reader.AnnotationsCalls)
	}
	if len(fs.Files) != 0 {
		t.Errorf("dry-run must leave zero files; got %d", len(fs.Files))
	}
	if len(st.Commits) != 0 {
		t.Errorf("dry-run must not commit; got %d", len(st.Commits))
	}
}

func TestPullTranscript_DryRun_UpToDate(t *testing.T) {
	fs := testutil.NewMemFS()
	st := &testutil.StubPullStore{}
	clock := testutil.NewFixedClock()
	ref := newRef(t)

	// First REAL pull lands the manifest (raw stored hash).
	if _, err := pull.NewPipeline(happyReader(), fs, st, clock, testCreds(), testPullsRoot).
		PullTranscript(context.Background(), ref, pull.PullOptions{}); err != nil {
		t.Fatalf("first pull: %v", err)
	}

	// Dry-run re-pull: metadata fast-path DIFF (raw stored == raw server) ⇒
	// up-to-date, marked dry-run, and STILL no mutation beyond the first pull.
	reader2 := happyReader()
	res, err := pull.NewPipeline(reader2, fs, st, clock, testCreds(), testPullsRoot).
		PullTranscript(context.Background(), ref, pull.PullOptions{DryRun: true})
	if err != nil {
		t.Fatalf("dry-run pull: %v", err)
	}
	if res.Status != pull.PullStatusUpToDate {
		t.Errorf("Status = %q, want up-to-date", res.Status)
	}
	if res.License != schema.LicenseCCBYSA {
		t.Errorf("License = %q, want %q (up-to-date dry-run result must carry the served license)", res.License, schema.LicenseCCBYSA)
	}
	if !res.DryRun {
		t.Errorf("DryRun = false, want true")
	}
	if reader2.ContentCalls != 0 {
		t.Errorf("ContentCalls = %d, want 0", reader2.ContentCalls)
	}
	if len(st.Commits) != 1 {
		t.Errorf("total CommitPull calls = %d, want 1 (dry-run adds none)", len(st.Commits))
	}
}

// --- Force ---

func TestPullTranscript_Force_BypassesDiff(t *testing.T) {
	fs := testutil.NewMemFS()
	st := &testutil.StubPullStore{}
	clock := testutil.NewFixedClock()
	ref := newRef(t)

	if _, err := pull.NewPipeline(happyReader(), fs, st, clock, testCreds(), testPullsRoot).
		PullTranscript(context.Background(), ref, pull.PullOptions{}); err != nil {
		t.Fatalf("first pull: %v", err)
	}

	// --force: even though the hash matches, re-download + re-commit.
	reader2 := happyReader()
	res, err := pull.NewPipeline(reader2, fs, st, clock, testCreds(), testPullsRoot).
		PullTranscript(context.Background(), ref, pull.PullOptions{Force: true})
	if err != nil {
		t.Fatalf("force pull: %v", err)
	}
	if res.Status != pull.PullStatusPulled {
		t.Errorf("Status = %q, want pulled (force re-downloads)", res.Status)
	}
	if reader2.ContentCalls != 1 {
		t.Errorf("ContentCalls = %d, want 1 (force re-downloads)", reader2.ContentCalls)
	}
	if reader2.LastIfNoneMatch != "" {
		t.Errorf("If-None-Match = %q, want empty under --force", reader2.LastIfNoneMatch)
	}
	if len(st.Commits) != 2 {
		t.Errorf("total CommitPull calls = %d, want 2 (force re-commits)", len(st.Commits))
	}
}

// --- Status mapping for village-contacting failures ---

func TestPullTranscript_NotLoggedIn(t *testing.T) {
	fs := testutil.NewMemFS()
	st := &testutil.StubPullStore{}
	reader := happyReader()
	p := pull.NewPipeline(reader, fs, st, testutil.NewFixedClock(), pull.Credentials{}, testPullsRoot)

	res, err := p.PullTranscript(context.Background(), newRef(t), pull.PullOptions{})
	if err == nil {
		t.Fatal("expected not-logged-in error")
	}
	if res.Status != pull.PullStatusNotLoggedIn {
		t.Errorf("Status = %q, want not-logged-in", res.Status)
	}
	if !strings.Contains(err.Error(), "peasant village login") {
		t.Errorf("error should name `peasant village login`; got: %v", err)
	}
	// Zero village contact, zero mutation.
	if reader.NegotiateCalls != 0 || reader.MetaCalls != 0 {
		t.Errorf("logged-out pull must not contact the village")
	}
	if len(fs.Files) != 0 || len(st.Commits) != 0 {
		t.Errorf("logged-out pull must not write anything")
	}
}

func TestPullTranscript_NotFound(t *testing.T) {
	fs := testutil.NewMemFS()
	st := &testutil.StubPullStore{}
	reader := happyReader()
	reader.MetaErr = village.ErrPullNotFound // 404 from FETCH-META

	res, err := pull.NewPipeline(reader, fs, st, testutil.NewFixedClock(), testCreds(), testPullsRoot).
		PullTranscript(context.Background(), newRef(t), pull.PullOptions{})
	if err == nil {
		t.Fatal("expected not-found error")
	}
	if res.Status != pull.PullStatusNotFound {
		t.Errorf("Status = %q, want not-found", res.Status)
	}
	if len(fs.Files) != 0 || len(st.Commits) != 0 {
		t.Errorf("not-found pull must not write anything")
	}
}

func TestPullTranscript_ContractError(t *testing.T) {
	fs := testutil.NewMemFS()
	st := &testutil.StubPullStore{}
	reader := happyReader()
	reader.NegotiateErr = village.ErrPullContractIncompatible

	res, err := pull.NewPipeline(reader, fs, st, testutil.NewFixedClock(), testCreds(), testPullsRoot).
		PullTranscript(context.Background(), newRef(t), pull.PullOptions{})
	if err == nil {
		t.Fatal("expected contract error")
	}
	if res.Status != pull.PullStatusContractError {
		t.Errorf("Status = %q, want contract-error", res.Status)
	}
	if reader.MetaCalls != 0 {
		t.Errorf("contract failure must abort before FETCH-META")
	}
	if len(fs.Files) != 0 || len(st.Commits) != 0 {
		t.Errorf("contract-error pull must not write anything")
	}
}

// --- Pre-WRITE failure ⇒ zero mutation ---

func TestPullTranscript_PreWriteFailure_ZeroMutation(t *testing.T) {
	fs := testutil.NewMemFS()
	st := &testutil.StubPullStore{}
	reader := happyReader()
	reader.AnnotationsErr = errors.New("annotation fetch boom") // fails in FETCH-ANNOTATIONS (pre-WRITE)

	res, err := pull.NewPipeline(reader, fs, st, testutil.NewFixedClock(), testCreds(), testPullsRoot).
		PullTranscript(context.Background(), newRef(t), pull.PullOptions{})
	if err == nil {
		t.Fatal("expected error")
	}
	if res.Status != pull.PullStatusError {
		t.Errorf("Status = %q, want error", res.Status)
	}
	if res.Status == pull.PullStatusUpToDate {
		t.Errorf("an error must NEVER be reported as up-to-date")
	}
	if len(fs.Files) != 0 {
		t.Errorf("pre-WRITE failure must leave zero files; got %d", len(fs.Files))
	}
	if len(st.Commits) != 0 {
		t.Errorf("pre-WRITE failure must not commit")
	}
}

// --- DB-TX failure ⇒ compensating dir removal ---

func TestPullTranscript_DBTxFailure_CompensatingRemoval(t *testing.T) {
	fs := testutil.NewMemFS()
	st := &testutil.StubPullStore{CommitErr: errors.New("db commit boom")}
	reader := happyReader()

	res, err := pull.NewPipeline(reader, fs, st, testutil.NewFixedClock(), testCreds(), testPullsRoot).
		PullTranscript(context.Background(), newRef(t), pull.PullOptions{})
	if err == nil {
		t.Fatal("expected DB-TX error")
	}
	if res.Status != pull.PullStatusError {
		t.Errorf("Status = %q, want error", res.Status)
	}
	// Compensation: the renamed pull dir must be removed (pre-pull state restored).
	wantDir := filepath.Join(testPullsRoot, testutil.TestVillageHost, testutil.TestTranscriptUUID)
	for path := range fs.Files {
		if strings.HasPrefix(path, wantDir) {
			t.Errorf("compensation failed: file %q still under pull dir after DB-TX failure", path)
		}
	}
	if !strings.Contains(err.Error(), "rolled back") {
		t.Errorf("error should note the rollback; got: %v", err)
	}
}

// --- DB-TX failure AND compensation failure ⇒ actionable orphan error ---

func TestPullTranscript_CompensationFailure_NamesOrphan(t *testing.T) {
	mem := testutil.NewMemFS()
	wantDir := filepath.Join(testPullsRoot, testutil.TestVillageHost, testutil.TestTranscriptUUID)

	// FailingFS: the compensating RemoveAll(pullDir) fails. The pipeline also
	// clears the (absent) pull dir BEFORE the rename — that first matching call is
	// harmless and must succeed, so skip the first match and fail only the LATER
	// compensating RemoveAll of the same path.
	fs := testutil.NewFailingFS(mem)
	fs.RemoveAllErr = errors.New("removeall boom")
	fs.RemoveAllOnPaths = map[string]bool{wantDir: true}
	fs.RemoveAllSkipFirstN = 1

	st := &testutil.StubPullStore{CommitErr: errors.New("db commit boom")}
	reader := happyReader()

	res, err := pull.NewPipeline(reader, fs, st, testutil.NewFixedClock(), testCreds(), testPullsRoot).
		PullTranscript(context.Background(), newRef(t), pull.PullOptions{})
	if err == nil {
		t.Fatal("expected compensation-failure error")
	}
	if res.Status != pull.PullStatusError {
		t.Errorf("Status = %q, want error", res.Status)
	}
	// Actionable error: names the orphan dir + instructs --force repair.
	if !strings.Contains(err.Error(), wantDir) {
		t.Errorf("error must name the orphan dir %q; got: %v", wantDir, err)
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("error must instruct --force repair; got: %v", err)
	}
	// The pipeline DID attempt the compensating RemoveAll of the pull dir.
	saw := false
	for _, p := range fs.RemoveAllCalls {
		if p == wantDir {
			saw = true
		}
	}
	if !saw {
		t.Errorf("pipeline did not attempt compensating RemoveAll(%q)", wantDir)
	}
}

// --- WRITE-stage cleanup branches ⇒ zero mutation (FailingFS injections) ---

// assertNoMutationUnder fails if any MemFS file path is under the given pull dir,
// or if the temp staging dir lingers — i.e. the WRITE stage left local state.
func assertZeroPullState(t *testing.T, mem *testutil.MemFS, pullDir, tmpDir string) {
	t.Helper()
	for path := range mem.Files {
		if strings.HasPrefix(path, pullDir) {
			t.Errorf("WRITE failure left a file under the pull dir: %q", path)
		}
		if strings.HasPrefix(path, tmpDir) {
			t.Errorf("WRITE failure left a file under the temp dir: %q", path)
		}
	}
}

// TestPullTranscript_StageFilesFailure_ZeroMutation: a WriteFile error during
// staging must RemoveAll(tmpDir) and leave zero pullDir mutation + no commit.
func TestPullTranscript_StageFilesFailure_ZeroMutation(t *testing.T) {
	mem := testutil.NewMemFS()
	fs := testutil.NewFailingFS(mem)
	fs.WriteFileErr = errors.New("stage writefile boom")
	st := &testutil.StubPullStore{}

	res, err := pull.NewPipeline(happyReader(), fs, st, testutil.NewFixedClock(), testCreds(), testPullsRoot).
		PullTranscript(context.Background(), newRef(t), pull.PullOptions{})
	if err == nil {
		t.Fatal("expected stageFiles error")
	}
	if res.Status != pull.PullStatusError {
		t.Errorf("Status = %q, want error", res.Status)
	}
	pullDir := filepath.Join(testPullsRoot, testutil.TestVillageHost, testutil.TestTranscriptUUID)
	tmpDir := filepath.Join(testPullsRoot, testutil.TestVillageHost, defaults.TempDirPrefix+testutil.TestTranscriptUUID)
	assertZeroPullState(t, mem, pullDir, tmpDir)
	if len(st.Commits) != 0 {
		t.Errorf("stageFiles failure must not commit")
	}
}

// TestPullTranscript_StageMkdirFailure_ZeroMutation: a MkdirAll error during
// staging (cannot even create the temp dir) ⇒ error + zero mutation + no commit.
func TestPullTranscript_StageMkdirFailure_ZeroMutation(t *testing.T) {
	mem := testutil.NewMemFS()
	fs := testutil.NewFailingFS(mem)
	fs.MkdirAllErr = errors.New("stage mkdir boom")
	st := &testutil.StubPullStore{}

	res, err := pull.NewPipeline(happyReader(), fs, st, testutil.NewFixedClock(), testCreds(), testPullsRoot).
		PullTranscript(context.Background(), newRef(t), pull.PullOptions{})
	if err == nil {
		t.Fatal("expected stage mkdir error")
	}
	if res.Status != pull.PullStatusError {
		t.Errorf("Status = %q, want error", res.Status)
	}
	pullDir := filepath.Join(testPullsRoot, testutil.TestVillageHost, testutil.TestTranscriptUUID)
	tmpDir := filepath.Join(testPullsRoot, testutil.TestVillageHost, defaults.TempDirPrefix+testutil.TestTranscriptUUID)
	assertZeroPullState(t, mem, pullDir, tmpDir)
	if len(st.Commits) != 0 {
		t.Errorf("stage mkdir failure must not commit")
	}
}

// TestPullTranscript_StaleTempClearFailure: the pre-staging RemoveAll(tmpDir)
// clear of a crashed prior run's temp dir fails ⇒ actionable error, no commit.
func TestPullTranscript_StaleTempClearFailure(t *testing.T) {
	mem := testutil.NewMemFS()
	tmpDir := filepath.Join(testPullsRoot, testutil.TestVillageHost, defaults.TempDirPrefix+testutil.TestTranscriptUUID)
	fs := testutil.NewFailingFS(mem)
	fs.RemoveAllErr = errors.New("clear stale temp boom")
	fs.RemoveAllOnPaths = map[string]bool{tmpDir: true} // fail ONLY the stale-temp clear
	st := &testutil.StubPullStore{}

	res, err := pull.NewPipeline(happyReader(), fs, st, testutil.NewFixedClock(), testCreds(), testPullsRoot).
		PullTranscript(context.Background(), newRef(t), pull.PullOptions{})
	if err == nil {
		t.Fatal("expected stale-temp clear error")
	}
	if res.Status != pull.PullStatusError {
		t.Errorf("Status = %q, want error", res.Status)
	}
	if !strings.Contains(err.Error(), "stale temp dir") {
		t.Errorf("error should name the stale temp dir; got: %v", err)
	}
	pullDir := filepath.Join(testPullsRoot, testutil.TestVillageHost, testutil.TestTranscriptUUID)
	assertZeroPullState(t, mem, pullDir, tmpDir)
	if len(st.Commits) != 0 {
		t.Errorf("stale-temp clear failure must not commit")
	}
}

// TestPullTranscript_PreRenameClearFailure: on a re-pull, clearing the existing
// pull dir BEFORE the publish move fails ⇒ error names the pull dir + tmp cleaned.
func TestPullTranscript_PreRenameClearFailure(t *testing.T) {
	mem := testutil.NewMemFS()
	clock := testutil.NewFixedClock()
	ref := newRef(t)
	st := &testutil.StubPullStore{}

	// First REAL pull creates an existing pull dir to clear on the re-pull.
	if _, err := pull.NewPipeline(happyReader(), mem, st, clock, testCreds(), testPullsRoot).
		PullTranscript(context.Background(), ref, pull.PullOptions{}); err != nil {
		t.Fatalf("first pull: %v", err)
	}

	pullDir := filepath.Join(testPullsRoot, testutil.TestVillageHost, testutil.TestTranscriptUUID)
	tmpDir := filepath.Join(testPullsRoot, testutil.TestVillageHost, defaults.TempDirPrefix+testutil.TestTranscriptUUID)

	// Re-pull --force (skip DIFF) so we reach the WRITE stage; fail ONLY the
	// pre-rename RemoveAll(pullDir).
	fs := testutil.NewFailingFS(mem)
	fs.RemoveAllErr = errors.New("clear pull dir boom")
	fs.RemoveAllOnPaths = map[string]bool{pullDir: true}

	res, err := pull.NewPipeline(happyReader(), fs, st, clock, testCreds(), testPullsRoot).
		PullTranscript(context.Background(), ref, pull.PullOptions{Force: true})
	if err == nil {
		t.Fatal("expected pre-rename clear error")
	}
	if res.Status != pull.PullStatusError {
		t.Errorf("Status = %q, want error", res.Status)
	}
	if !strings.Contains(err.Error(), "before rename") {
		t.Errorf("error should name the pre-rename clear; got: %v", err)
	}
	// The temp dir staged for this re-pull must be cleaned up.
	for path := range mem.Files {
		if strings.HasPrefix(path, tmpDir) {
			t.Errorf("pre-rename failure left a temp file: %q", path)
		}
	}
	// No NEW commit beyond the first pull.
	if len(st.Commits) != 1 {
		t.Errorf("CommitPull calls = %d, want 1 (re-pull aborted before DB-TX)", len(st.Commits))
	}
}

// TestPullTranscript_RenameDirFailure_Cleanup: a CopyFile error inside renameDir
// (the publish move) ⇒ RemoveAll(tmpDir)+RemoveAll(pullDir) cleanup + actionable
// "atomic rename" error, zero mutation, no commit.
func TestPullTranscript_RenameDirFailure_Cleanup(t *testing.T) {
	mem := testutil.NewMemFS()
	fs := testutil.NewFailingFS(mem)
	fs.CopyFileErr = errors.New("rename copyfile boom")
	st := &testutil.StubPullStore{}

	res, err := pull.NewPipeline(happyReader(), fs, st, testutil.NewFixedClock(), testCreds(), testPullsRoot).
		PullTranscript(context.Background(), newRef(t), pull.PullOptions{})
	if err == nil {
		t.Fatal("expected renameDir error")
	}
	if res.Status != pull.PullStatusError {
		t.Errorf("Status = %q, want error", res.Status)
	}
	if !strings.Contains(err.Error(), "rename") {
		t.Errorf("error should name the rename failure; got: %v", err)
	}
	pullDir := filepath.Join(testPullsRoot, testutil.TestVillageHost, testutil.TestTranscriptUUID)
	tmpDir := filepath.Join(testPullsRoot, testutil.TestVillageHost, defaults.TempDirPrefix+testutil.TestTranscriptUUID)
	assertZeroPullState(t, mem, pullDir, tmpDir)
	if len(st.Commits) != 0 {
		t.Errorf("renameDir failure must not commit")
	}
}

// --- Served-blob hash fallback when the village computes none ---

func TestPullTranscript_NoServerHash_LocalRecompute(t *testing.T) {
	fs := testutil.NewMemFS()
	st := &testutil.StubPullStore{}
	meta := happyMeta()
	meta.ContentHash = "" // village computed no hash
	reader := &testutil.StubVillageReader{
		Meta:        meta,
		ContentBody: []byte(testBlobContent),
		ContentETag: "", // no ETag either
		Annotations: nil,
	}

	res, err := pull.NewPipeline(reader, fs, st, testutil.NewFixedClock(), testCreds(), testPullsRoot).
		PullTranscript(context.Background(), newRef(t), pull.PullOptions{})
	if err != nil {
		t.Fatalf("PullTranscript: %v", err)
	}
	if res.Status != pull.PullStatusPulled {
		t.Errorf("Status = %q, want pulled", res.Status)
	}
	want := schema.ComputeTranscriptHash([]byte(testBlobContent))
	if res.ServedBlobHash != want {
		t.Errorf("ServedBlobHash = %q, want locally-recomputed %q", res.ServedBlobHash, want)
	}
	if len(st.Commits) != 1 || st.Commits[0].Transcript.ContentHash != want {
		t.Errorf("commit ContentHash should be the recomputed hash")
	}

	// SECOND pull, same hashless server: the manifest now stores the
	// recomputed hash, and the post-download local-hash DIFF fallback is the
	// ONLY path that can classify this as up-to-date (no meta hash to
	// fast-path on, no ETag to 304 on). This is that fallback's sole
	// exercise in the suite — it must re-download, then decline to rewrite,
	// and still carry the served license.
	reader2 := &testutil.StubVillageReader{
		Meta:        meta,
		ContentBody: []byte(testBlobContent),
		ContentETag: "",
		Annotations: nil,
	}
	res2, err := pull.NewPipeline(reader2, fs, st, testutil.NewFixedClock(), testCreds(), testPullsRoot).
		PullTranscript(context.Background(), newRef(t), pull.PullOptions{})
	if err != nil {
		t.Fatalf("second PullTranscript: %v", err)
	}
	if res2.Status != pull.PullStatusUpToDate {
		t.Errorf("second pull Status = %q, want up-to-date (post-download hash fallback)", res2.Status)
	}
	if res2.License != schema.LicenseCCBYSA {
		t.Errorf("second pull License = %q, want %q (fallback result must carry the served license)", res2.License, schema.LicenseCCBYSA)
	}
	if len(st.Commits) != 1 {
		t.Errorf("second pull committed %d extra row(s); want none (up-to-date)", len(st.Commits)-1)
	}
}

// --- ensure the store row types remain referenced by the consumer interface ---

var _ = store.PullCommit{}
