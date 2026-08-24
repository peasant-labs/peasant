package store_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/peasant-labs/peasant/internal/store/storetest"
)

// A store read that loses a shutdown race must degrade to an error, never a
// panic: Close used to nil the pool, so a late background reader (the
// annotation-create WebSocket broadcast) crashed the process with a nil
// dereference inside sqlitex.Pool.Take.
func TestClose_LateReadReturnsErrorNotPanic(t *testing.T) {
	s := storetest.Open(t)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := s.GetAnnotationsForSession(context.Background(), "sess-after-close"); err == nil {
		t.Fatal("GetAnnotationsForSession after Close: want error, got nil")
	} else if !strings.Contains(err.Error(), "pool closed") {
		t.Fatalf("GetAnnotationsForSession after Close: error %q, want a pool-closed error", err)
	}
}

// Close is idempotent: the underlying pool tolerates exactly one Close, so the
// store must swallow the second call instead of double-closing.
func TestClose_Idempotent(t *testing.T) {
	s := storetest.Open(t)
	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// Concurrent readers racing Close finish with either data or an error; the
// race detector gates the field access, and no outcome may panic.
func TestClose_ConcurrentReadersDoNotPanic(t *testing.T) {
	s := storetest.Open(t)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _ = s.GetAnnotationsForSession(context.Background(), "sess-racing-close")
		}()
	}
	close(start)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	wg.Wait()
}
