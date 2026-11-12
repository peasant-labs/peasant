package attachments_test

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/attachments"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/image_gc.yaml
var imageGCYAML []byte

//go:embed testdata/image_gc.manifest.yaml
var imageGCManifestYAML []byte

type imageGCCase struct {
	Name            string `yaml:"name"`
	Action          string `yaml:"action"`
	Sessions        int    `yaml:"sessions"`
	WantCollections int64  `yaml:"wantCollections"`
}

func LoadImageGCFixtures(t *testing.T) []imageGCCase {
	t.Helper()
	var document struct {
		Cases []imageGCCase `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(imageGCYAML, &document); err != nil {
		t.Fatal(err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(imageGCManifestYAML, "image gc")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(document.Cases))
	for i, fixture := range document.Cases {
		names[i] = fixture.Name
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "image gc"); err != nil {
		t.Fatal(err)
	}
	return document.Cases
}

func TestImageGC(t *testing.T) {
	for _, fixture := range LoadImageGCFixtures(t) {
		t.Run(fixture.Name, func(t *testing.T) {
			switch fixture.Action {
			case "once_per_run":
				testImageGCOncePerRun(t, fixture)
			case "end_of_run":
				testImageGCEndOfRun(t)
			case "stopped_run":
				testImageGCStoppedRun(t)
			case "concurrent":
				testImageGCConcurrentPut(t)
			default:
				t.Fatalf("unknown image gc action %q", fixture.Action)
			}
		})
	}
}

// runImagePipeline is one real pipeline run over db. It discovers no session,
// so the run is its stages alone, ending in the cleanup stage.
func runImagePipeline(t *testing.T, ctx context.Context, db *store.Store) error {
	t.Helper()
	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: func(ingest.FileSystem, ingest.GitResolver, salt.Salt) ingest.SourceAdapter {
			return &testutil.StubAdapter{ProviderValue: ingest.HarnessClaudeCode}
		},
	}
	cfg := ingest.PipelineConfig{
		Sources:   map[ingest.Harness]ingest.SourceConfig{ingest.HarnessClaudeCode: {Paths: []ingest.ResolvedPath{"/sources"}, Enabled: true}},
		OutputDir: "/output",
	}
	pipeline, err := ingest.NewPipeline(testutil.NewMemFS(), testutil.DefaultGitResolver(), adapters, cfg,
		ingest.WithStore(db), ingest.WithMetricsStore(db), ingest.WithArenaSizeBytes(1<<20))
	if err != nil {
		t.Fatal(err)
	}
	_, err = pipeline.Run(ctx)
	return err
}

func coloredPNG(t *testing.T, shade int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{R: uint8(shade), G: uint8(shade >> 8), B: 7, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func countCollections(db *store.Store) *atomic.Int64 {
	var started atomic.Int64
	db.AttachmentStoreWithHooks(attachments.Hooks{GCBeforeLock: func() { started.Add(1) }})
	return &started
}

func imageFiles(t *testing.T, files *attachments.Store) int {
	t.Helper()
	entries, err := os.ReadDir(files.Dir())
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

func testImageGCOncePerRun(t *testing.T, fixture imageGCCase) {
	db, files := openAttachmentFixture(t)
	started := countCollections(db)
	for i := range fixture.Sessions {
		sessionID := fmt.Sprintf("dddddddd-dddd-4ddd-8ddd-%012d", i)
		seedAttachmentSession(t, db, sessionID)
		pending := []attachments.Pending{pendingImage(sessionID, fmt.Sprintf("img-%d", i), "main", "image/png", coloredPNG(t, i))}
		if err := db.ReplaceSessionImages(t.Context(), sessionID, pending); err != nil {
			t.Fatal(err)
		}
	}
	if got := started.Load(); got != 0 {
		t.Fatalf("collections while %d sessions installed their images = %d, want 0", fixture.Sessions, got)
	}
	if err := runImagePipeline(t, t.Context(), db); err != nil {
		t.Fatal(err)
	}
	// Each collection lists the image directory exactly once.
	if got := started.Load(); got != fixture.WantCollections {
		t.Fatalf("image directory listings in one run = %d, want %d", got, fixture.WantCollections)
	}
	if got := imageFiles(t, files); got != fixture.Sessions {
		t.Fatalf("referenced image files after the run = %d, want %d", got, fixture.Sessions)
	}
}

// leaveUnreferencedImage installs one image for the fixture session and then
// replaces the session's set with none, which leaves the file unreferenced.
func leaveUnreferencedImage(t *testing.T, db *store.Store, files *attachments.Store) string {
	t.Helper()
	pending := []attachments.Pending{pendingImage(testutil.TestSessionUUID, "img-gone", "main", "image/png", onePixelPNG(t))}
	if err := db.ReplaceSessionImages(t.Context(), testutil.TestSessionUUID, pending); err != nil {
		t.Fatal(err)
	}
	rows, err := files.SessionRows(t.Context(), testutil.TestSessionUUID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("seed image rows = %+v, %v", rows, err)
	}
	path := filepath.Join(files.Dir(), rows[0].SHA256)
	if err := db.ReplaceSessionImages(t.Context(), testutil.TestSessionUUID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("replacing a session's images collected mid-run: %v", err)
	}
	return path
}

func testImageGCEndOfRun(t *testing.T) {
	db, files := openAttachmentFixture(t)
	path := leaveUnreferencedImage(t, db, files)
	if err := runImagePipeline(t, t.Context(), db); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("unreferenced image survived the end of the run: %v", err)
	}
}

func testImageGCStoppedRun(t *testing.T) {
	db, files := openAttachmentFixture(t)
	path := leaveUnreferencedImage(t, db, files)
	started := countCollections(db)
	stopped, cancel := context.WithCancel(t.Context())
	cancel()
	_ = runImagePipeline(t, stopped, db) // the run reports its own cancellation
	if got := started.Load(); got != 0 {
		t.Fatalf("a stopped run collected %d times, want 0", got)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("a stopped run removed the unreferenced image: %v", err)
	}
	if err := runImagePipeline(t, t.Context(), db); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the next run did not collect the unreferenced image: %v", err)
	}
}

func testImageGCConcurrentPut(t *testing.T) {
	db, _ := openAttachmentFixture(t)
	fsynced := make(chan struct{})
	release := make(chan struct{})
	collectStarted := make(chan struct{})
	collectAcquired := make(chan struct{})
	files := db.AttachmentStoreWithHooks(attachments.Hooks{
		PutAfterFsync: func() {
			close(fsynced)
			<-release
		},
		GCBeforeLock: func() { close(collectStarted) },
		GCAfterLock:  func() { close(collectAcquired) },
	})
	putErr := make(chan error, 1)
	putRow := make(chan attachments.Row, 1)
	go func() {
		row, err := files.Put(context.Background(), pendingImage(testutil.TestSessionUUID, "img-race", "main", "image/png", onePixelPNG(t)))
		putRow <- row
		putErr <- err
	}()
	select {
	case <-fsynced:
	case <-time.After(5 * time.Second):
		t.Fatal("put did not reach fsync while holding the lock")
	}
	collectErr := make(chan error, 1)
	go func() { collectErr <- db.CollectUnreferencedImages(context.Background()) }()
	select {
	case <-collectStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("the run-end collection did not start")
	}
	select {
	case <-collectAcquired:
		t.Fatal("the collection acquired the lock while the writer still held it")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	row := <-putRow
	if err := <-putErr; err != nil {
		t.Fatal(err)
	}
	if err := <-collectErr; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(files.Dir(), row.SHA256)); err != nil {
		t.Fatalf("the collection removed the file a concurrent put just wrote: %v", err)
	}
}
