package ingest

import (
	"bytes"
	"context"
	"io/fs"
	"log/slog"
	"strings"
	"testing"
)

// schedulingLookupFaultStore is a narrow dependency double: it returns the
// configured fault from the one store method the scheduling-parent lookup
// reads. No other SessionStore method is called by this test, so the embedded
// interface stays nil on purpose.
type schedulingLookupFaultStore struct {
	SessionStore
	err error
}

func (s schedulingLookupFaultStore) BulkLookupSessionLocations(context.Context, []SessionID) (map[SessionID]SessionLocation, error) {
	return nil, s.err
}

// TestSchedulingLookupWarningRefusesRawDependencyDetail proves the stored
// scheduling-parent lookup fault is reported with a bounded reason code and
// never reproduces the raw dependency error. The fault carries a private
// filesystem path; the production warning must not contain it.
func TestSchedulingLookupWarningRefusesRawDependencyDetail(t *testing.T) {
	const privatePath = "/home/private-operator/secret-workspace"
	child := SessionID("11111111-1111-4111-8111-111111111111")
	parent := SessionID("22222222-2222-4222-8222-222222222222")
	fault := &fs.PathError{Op: "stat", Path: privatePath, Err: fs.ErrNotExist}
	p := &Pipeline{
		store:         schedulingLookupFaultStore{err: fault},
		locationCache: map[SessionID]SessionLocation{},
	}
	entries := []DiffEntry{{Session: DiscoveredSession{SessionID: child, ParentUUID: &parent}}}

	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	got := p.storedParentSet(context.Background(), entries)
	if !got[parent] {
		t.Fatalf("a lookup failure must keep the legacy available-parent edge; got %v", got)
	}
	logged := buf.String()
	if !strings.Contains(logged, "resolve stored scheduling parents") {
		t.Fatalf("expected the scheduling-parent warning, got %q", logged)
	}
	if strings.Contains(logged, privatePath) {
		t.Fatalf("the warning leaked the private dependency detail %q in %q", privatePath, logged)
	}
	if !strings.Contains(logged, `"reason":"not_found"`) {
		t.Fatalf("the warning did not carry the bounded reason code, got %q", logged)
	}
}
