package pull_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/pull"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// endlessBlob is a served body that never ends, standing in for a hostile or
// misbehaving content endpoint. It counts what the pull consumed.
type endlessBlob struct{ read int64 }

func (b *endlessBlob) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	b.read += int64(len(p))
	return len(p), nil
}

func newCappedPipeline(reader pull.VillageReader, fs *testutil.MemFS, st pull.PullStore, capBytes int64) *pull.Pipeline {
	return pull.NewPipeline(reader, treeRenameFS{fs}, st, testutil.NewFixedClock(), testCreds(), testPullsRoot,
		pull.WithInstallSync(func(string) error { return nil }),
		pull.WithInstallLock(func(context.Context, string, schema.TranscriptID) (func() error, error) {
			return func() error { return nil }, nil
		}),
		pull.WithServedBlobCap(capBytes))
}

func assertServedBlobRefused(t *testing.T, res *pull.PullResult, err error, fs *testutil.MemFS, st *testutil.StubPullStore, reader *testutil.StubVillageReader, capBytes int64) {
	t.Helper()
	if !errors.Is(err, pull.ErrServedBlobTooLarge) {
		t.Fatalf("err = %v, want ErrServedBlobTooLarge", err)
	}
	if !strings.Contains(err.Error(), defaults.HumanByteSize(capBytes)) {
		t.Errorf("err = %q, want it to name the cap %s", err, defaults.HumanByteSize(capBytes))
	}
	if res.Status != pull.PullStatusError {
		t.Errorf("status = %q, want %q", res.Status, pull.PullStatusError)
	}
	if len(fs.Files) != 0 {
		t.Errorf("an oversized blob must leave zero files; got %d", len(fs.Files))
	}
	if len(st.Commits) != 0 {
		t.Errorf("an oversized blob must not commit; got %d commits", len(st.Commits))
	}
	if reader.AnnotationsCalls != 0 {
		t.Errorf("an oversized blob must stop before fetching annotations; got %d calls", reader.AnnotationsCalls)
	}
}

func TestPullTranscript_ServedBlobOverCapIsRefused(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	st := &testutil.StubPullStore{}
	reader := happyReader()
	capBytes := int64(len(testBlobContent) - 1)

	res, err := newCappedPipeline(reader, fs, st, capBytes).
		PullTranscript(context.Background(), newRef(t), pull.PullOptions{})
	assertServedBlobRefused(t, res, err, fs, st, reader, capBytes)
}

func TestPullTranscript_EndlessServedBlobStopsAtCap(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	st := &testutil.StubPullStore{}
	reader := happyReader()
	stream := &endlessBlob{}
	reader.ContentStream = stream
	const capBytes = 64 << 10

	res, err := newCappedPipeline(reader, fs, st, capBytes).
		PullTranscript(context.Background(), newRef(t), pull.PullOptions{})
	assertServedBlobRefused(t, res, err, fs, st, reader, capBytes)
	if limit := int64(capBytes + 1); stream.read > limit {
		t.Errorf("the pull read %d bytes of an endless body, want at most %d", stream.read, limit)
	}
}

func TestPullTranscript_ServedBlobAtCapIsPulled(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	st := &testutil.StubPullStore{}
	reader := happyReader()

	res, err := newCappedPipeline(reader, fs, st, int64(len(testBlobContent))).
		PullTranscript(context.Background(), newRef(t), pull.PullOptions{})
	if err != nil {
		t.Fatalf("PullTranscript: %v", err)
	}
	if res.Status != pull.PullStatusPulled {
		t.Fatalf("status = %q, want %q", res.Status, pull.PullStatusPulled)
	}
	if len(st.Commits) != 1 {
		t.Fatalf("commits = %d, want 1", len(st.Commits))
	}
}
