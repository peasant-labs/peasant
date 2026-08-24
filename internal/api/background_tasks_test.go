package api

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// Shutdown must not return while a tracked background task is still running:
// the caller closes the store right after Shutdown, so an undrained broadcast
// would read a closed store (the crash this primitive exists to prevent).
func TestShutdown_WaitsForSpawnedBackgroundTasks(t *testing.T) {
	s := NewServer(ServerConfig{})
	release := make(chan struct{})
	var finished atomic.Bool
	s.spawnBackground(context.Background(), func(context.Context) {
		<-release
		finished.Store(true)
	})

	done := make(chan struct{})
	go func() {
		if err := s.Shutdown(context.Background()); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("Shutdown returned while a tracked background task was still running")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown did not return after the tracked task finished")
	}
	if !finished.Load() {
		t.Fatal("tracked task did not run to completion before Shutdown returned")
	}
}

// The task receives the context it was spawned with, so serve-context
// cancellation interrupts its store reads and shutdown stays prompt.
func TestSpawnBackground_PropagatesContext(t *testing.T) {
	s := NewServer(ServerConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan error, 1)
	s.spawnBackground(ctx, func(ctx context.Context) {
		<-ctx.Done()
		got <- ctx.Err()
	})
	cancel()
	select {
	case err := <-got:
		if err == nil {
			t.Fatal("spawned task saw a nil ctx.Err after cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("spawned task never observed the spawn context's cancellation")
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}
