package attachments_test

import (
	"bytes"
	"context"
	_ "embed"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/attachments"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/store.yaml
var attachmentStoreYAML []byte

//go:embed testdata/store.manifest.yaml
var attachmentStoreManifestYAML []byte

type attachmentStoreCase struct {
	Name      string `yaml:"name"`
	Action    string `yaml:"action"`
	MediaType string `yaml:"media_type"`
}

func TestAttachmentStore(t *testing.T) {
	var document struct {
		Cases []attachmentStoreCase `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(attachmentStoreYAML, &document); err != nil {
		t.Fatal(err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(attachmentStoreManifestYAML, "attachment store")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(document.Cases))
	for i, fixture := range document.Cases {
		names[i] = fixture.Name
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "attachment store"); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range document.Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			switch fixture.Action {
			case "dedupe":
				testAttachmentDedupe(t)
			case "missing":
				testAttachmentMissing(t)
			case "unsupported":
				testAttachmentUnsupported(t, fixture.MediaType)
			case "too_large":
				testAttachmentTooLarge(t)
			case "gc_rendezvous":
				testAttachmentGCRendezvous(t)
			case "gc_unreferenced":
				testAttachmentGCUnreferenced(t)
			case "prune":
				testAttachmentPrune(t)
			case "prune_shared":
				testAttachmentPruneKeepsSharedBlob(t)
			case "route":
				testAttachmentRoute(t)
			case "partition_key":
				testAttachmentPartitionKey(t)
			default:
				t.Fatalf("unknown attachment store action %q", fixture.Action)
			}
		})
	}
}

func testAttachmentDedupe(t *testing.T) {
	t.Helper()
	db, files := openAttachmentFixture(t)
	payload := onePixelPNG(t)
	first := putImage(t, files, testutil.TestSessionUUID, "img-a", "main", "image/png", payload)
	secondID := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	seedAttachmentSession(t, db, secondID)
	second := putImage(t, files, secondID, "img-b", "earlier", "image/png", payload)
	if first.SHA256 == "" || first.SHA256 != second.SHA256 {
		t.Fatalf("sha = %s and %s, want one shared digest", first.SHA256, second.SHA256)
	}
	matches, err := filepath.Glob(filepath.Join(files.Dir(), first.SHA256))
	if err != nil || len(matches) != 1 {
		t.Fatalf("files for %s = %v, want one", first.SHA256, matches)
	}
}

func testAttachmentMissing(t *testing.T) {
	t.Helper()
	_, files := openAttachmentFixture(t)
	row := putImage(t, files, testutil.TestSessionUUID, "img-missing", "main", "image/png", onePixelPNG(t))
	if err := os.Remove(filepath.Join(files.Dir(), row.SHA256)); err != nil {
		t.Fatal(err)
	}
	got, err := files.Integrity(context.Background(), testutil.TestSessionUUID, "img-missing")
	if err != nil {
		t.Fatal(err)
	}
	if got.OmittedReason != string(schema.OmittedReasonMissing) || got.ByteSize != 0 {
		t.Fatalf("integrity = %+v, want missing with zero size", got)
	}
}

func testAttachmentUnsupported(t *testing.T, media string) {
	t.Helper()
	_, files := openAttachmentFixture(t)
	row := putImage(t, files, testutil.TestSessionUUID, "img-"+media, "main", media, []byte("not-an-image"))
	if row.OmittedReason != string(schema.OmittedReasonUnsupported) || row.SHA256 != "" || row.MediaType != "" {
		t.Fatalf("row = %+v, want unsupported with no digest or media type", row)
	}
	entries, err := os.ReadDir(files.Dir())
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("unsupported image wrote %d files", len(entries))
	}
}

func testAttachmentTooLarge(t *testing.T) {
	t.Helper()
	_, files := openAttachmentFixture(t)
	payload := append(onePixelPNG(t), bytes.Repeat([]byte{0}, attachments.MaxImageBytes)...)
	row := putImage(t, files, testutil.TestSessionUUID, "img-large", "main", "image/png", payload)
	if row.OmittedReason != string(schema.OmittedReasonTooLarge) || row.SHA256 != "" || row.MediaType != "image/png" {
		t.Fatalf("row = %+v, want too_large png metadata and no digest", row)
	}
}

func testAttachmentGCRendezvous(t *testing.T) {
	t.Helper()
	db, _ := openAttachmentFixture(t)
	dir := db.AttachmentStore().Dir()
	fsynced := make(chan struct{})
	release := make(chan struct{})
	gcStarted := make(chan struct{})
	gcAcquired := make(chan struct{})
	files := db.AttachmentStoreWithHooks(attachments.Hooks{
		PutAfterFsync: func() {
			close(fsynced)
			<-release
		},
		GCBeforeLock: func() { close(gcStarted) },
		GCAfterLock:  func() { close(gcAcquired) },
	})
	payload := onePixelPNG(t)
	putErr := make(chan error, 1)
	go func() {
		_, err := files.Put(context.Background(), pendingImage(testutil.TestSessionUUID, "img-race", "main", "image/png", payload))
		putErr <- err
	}()
	select {
	case <-fsynced:
	case err := <-putErr:
		t.Fatalf("put finished before fsync: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("put did not reach fsync while holding the lock")
	}
	gcErr := make(chan error, 1)
	go func() {
		_, err := files.GC(context.Background())
		gcErr <- err
	}()
	select {
	case <-gcStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("GC did not signal before acquiring the lock")
	}
	select {
	case <-gcAcquired:
		t.Fatal("GC acquired the lock while the writer still held it after fsync")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-putErr; err != nil {
		t.Fatal(err)
	}
	if err := <-gcErr; err != nil {
		t.Fatal(err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "*"))
	if err != nil {
		t.Fatal(err)
	}
	kept := 0
	for _, match := range matches {
		if !strings.HasPrefix(filepath.Base(match), ".") {
			kept++
		}
	}
	if kept != 1 {
		t.Fatalf("files after rendezvous = %d, want the committed blob", kept)
	}
}

func testAttachmentGCUnreferenced(t *testing.T) {
	t.Helper()
	_, files := openAttachmentFixture(t)
	row := putImage(t, files, testutil.TestSessionUUID, "img-gone", "main", "image/png", onePixelPNG(t))
	if _, err := files.PruneSession(context.Background(), testutil.TestSessionUUID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(files.Dir(), row.SHA256)); !os.IsNotExist(err) {
		t.Fatalf("unreferenced blob still present: %v", err)
	}
}

func testAttachmentPruneKeepsSharedBlob(t *testing.T) {
	t.Helper()
	db, files := openAttachmentFixture(t)
	second := "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	seedAttachmentSession(t, db, second)
	payload := onePixelPNG(t)
	first := putImage(t, files, testutil.TestSessionUUID, "img-shared", "main", "image/png", payload)
	putImage(t, files, second, "img-shared-other", "main", "image/png", payload)
	if _, err := db.PruneSessions(context.Background(), []ingest.SessionID{schema.SessionID(testutil.TestSessionUUID)}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(files.Dir(), first.SHA256)); err != nil {
		t.Fatalf("prune removed a blob still referenced by another session: %v", err)
	}
	if db.AttachmentStore() != files {
		t.Fatal("prune and put did not share the store constructed at open")
	}
}

func testAttachmentPrune(t *testing.T) {
	t.Helper()
	db, files := openAttachmentFixture(t)
	row := putImage(t, files, testutil.TestSessionUUID, "img-prune", "main", "image/png", onePixelPNG(t))
	if _, err := db.PruneSessions(context.Background(), []ingest.SessionID{schema.SessionID(testutil.TestSessionUUID)}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(files.Dir(), row.SHA256)); !os.IsNotExist(err) {
		t.Fatalf("prune left the blob: %v", err)
	}
}

func testAttachmentRoute(t *testing.T) {
	t.Helper()
	_, files := openAttachmentFixture(t)
	row := putImage(t, files, testutil.TestSessionUUID, "img-route", "main", "image/png", onePixelPNG(t))
	handler := attachments.Handler(files)
	ok := httptest.NewRecorder()
	handler.ServeHTTP(ok, httptest.NewRequest(http.MethodGet, "/api/v1/attachments/"+row.SHA256, nil))
	if ok.Code != http.StatusOK || ok.Header().Get("Cache-Control") != attachments.CacheControl || ok.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("route status=%d cache=%q type=%q", ok.Code, ok.Header().Get("Cache-Control"), ok.Header().Get("Content-Type"))
	}
	for _, path := range []string{"/api/v1/attachments/" + strings.ToUpper(row.SHA256), "/api/v1/attachments/abcd"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), attachments.CodeRefMalformed) || strings.Contains(rec.Body.String(), strings.TrimPrefix(path, "/api/v1/attachments/")) {
			t.Fatalf("%s status = %d body = %s, want 400 %s without the rejected ref", path, rec.Code, rec.Body.String(), attachments.CodeRefMalformed)
		}
	}
	missing := httptest.NewRecorder()
	missingPath := "/api/v1/attachments/" + strings.Repeat("e", 64)
	handler.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, missingPath, nil))
	if missing.Code != http.StatusNotFound || !strings.Contains(missing.Body.String(), attachments.CodeNotFound) {
		t.Fatalf("%s status = %d body = %s, want 404 %s", missingPath, missing.Code, missing.Body.String(), attachments.CodeNotFound)
	}
}

func testAttachmentPartitionKey(t *testing.T) {
	t.Helper()
	db, files := openAttachmentFixture(t)
	second := "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	seedAttachmentSession(t, db, second)
	payload := onePixelPNG(t)
	putImage(t, files, testutil.TestSessionUUID, "same-ref", "main", "image/png", payload)
	putImage(t, files, second, "same-ref", "earlier", "image/png", payload)
	putImage(t, files, testutil.TestSessionUUID, "same-ref", "other-partition", "image/png", payload)
	first, err := files.Integrity(context.Background(), testutil.TestSessionUUID, "same-ref")
	if err != nil {
		t.Fatal(err)
	}
	if first.PartitionID != "other-partition" {
		t.Fatalf("partition = %q, want the upserted partition; the key is session and source ref, not partition", first.PartitionID)
	}
	other, err := files.Integrity(context.Background(), second, "same-ref")
	if err != nil {
		t.Fatal(err)
	}
	if other.SessionID == first.SessionID {
		t.Fatal("two sessions collapsed to one attachment row")
	}
}

func openAttachmentFixture(t *testing.T) (*store.Store, *attachments.Store) {
	t.Helper()
	dir := t.TempDir()
	// ast-grep-ignore: no-migrating-store-open-in-tests -- the attachment table is created by the migration this test exercises.
	db, err := store.Open(filepath.Join(dir, "peasant.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	seedAttachmentSession(t, db, testutil.TestSessionUUID)
	files := db.AttachmentStore()
	if err := db.EnsureAttachmentDir(); err != nil {
		t.Fatal(err)
	}
	return db, files
}

func seedAttachmentSession(t *testing.T, db *store.Store, rawID string) {
	t.Helper()
	sid, err := ingest.NewSessionID(rawID)
	if err != nil {
		t.Fatal(err)
	}
	ph, err := ingest.NewProjectHash(strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	hs, err := ingest.NewHostSlug("attachments")
	if err != nil {
		t.Fatal(err)
	}
	model, err := ingest.NewModelID("claude-opus-4-6")
	if err != nil {
		t.Fatal(err)
	}
	src, err := ingest.NewResolvedPath(testutil.PlatformAbsPath("/test/session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	ingested := int64(120000)
	entry := ingest.StoreEntry{Metadata: &ingest.UnifiedMetadata{
		SchemaVersion: ingest.CurrentSchemaVersion, SessionID: sid, ModelHarness: defaults.HarnessClaudeCode, Model: model, HostSlug: hs,
		Timestamp: ingest.TimestampInfo{Start: 1, End: 2, Ingested: &ingested},
		Source:    ingest.SourceInfo{FilePath: string(src), Format: ingest.SourceFormatJSONL},
		Project:   ingest.ProjectInfo{Hash: ph, Name: "widgets", FilePath: "/home/widgets"},
		Stats:     ingest.StatsInfo{TurnCount: 1}, Version: "1", Subagents: []ingest.SubagentRef{}, Diagnostics: ingest.DiagnosticsInfo{},
	}, Session: ingest.DiscoveredSession{SessionID: sid, Harness: defaults.HarnessClaudeCode, SourcePath: src, SourceFormat: ingest.SourceFormatJSONL}}
	if err := db.InsertSessions(context.Background(), []ingest.StoreEntry{entry}); err != nil {
		t.Fatal(err)
	}
}

func putImage(t *testing.T, files *attachments.Store, sessionID, ref, partition, media string, payload []byte) attachments.Row {
	t.Helper()
	row, err := files.Put(context.Background(), pendingImage(sessionID, ref, partition, media, payload))
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func pendingImage(sessionID, ref, partition, media string, payload []byte) attachments.Pending {
	classified, bytesKept := attachments.Classify(media, payload)
	classified.SessionID = sessionID
	classified.SourceEntryRef = ref
	classified.PartitionID = partition
	if classified.OmittedReason == string(schema.OmittedReasonTooLarge) {
		classified.ByteSize = len(payload)
		bytesKept = nil
	}
	return attachments.Pending{Row: classified, Bytes: bytesKept}
}

func onePixelPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
