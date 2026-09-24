package store

import (
	"context"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/schema"
)

// TestSessionSnapshotReleasesPoolConnection proves the snapshot returns its
// pool connection before invoking the callback while retaining the shared OS
// lock, so a callback needing another store lookup cannot deadlock at pool
// size one.
func TestSessionSnapshotReleasesPoolConnection(t *testing.T) {
	dir := t.TempDir()
	root := dir + "/artifacts"
	artifacts, err := NewOSGenerationArtifactStore(root)
	if err != nil {
		t.Fatal(err)
	}
	locker, err := NewFileSessionLocker(root)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir+"/pool.db", WithPoolSize(1), WithIndexFormats(generationIndexFormat{}), WithGenerationArtifacts(artifacts, locker))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	sid, err := schema.NewSessionID("cccccccc-cccc-4ccc-8ccc-cccccccccccc")
	if err != nil {
		t.Fatal(err)
	}
	seedGenerationSession(t, s, string(sid))
	v2, blobs := buildTestGeneration(t, sid, "gen_pool_g1", "pool text", "pool input", "pool output")
	if _, err := s.ActivateGeneration(context.Background(), GenerationActivation{Generation: v2, Blobs: blobs}); err != nil {
		t.Fatalf("activate: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- s.WithSessionSnapshot(context.Background(), sid, func(snapshot indexformat.ReadSnapshot) error {
			// This lookup needs the pool connection the snapshot used. At
			// pool size one it succeeds only when the snapshot returned its
			// connection before the callback.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := s.ReadIndexState(ctx, sid); err != nil {
				return err
			}
			if snapshot.GenerationID != "gen_pool_g1" {
				return errorPoolf("generation = %q", snapshot.GenerationID)
			}
			return nil
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("snapshot with nested lookup: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("snapshot callback deadlocked at pool size one; the connection was retained through the callback")
	}
}

func errorPoolf(format, arg string) error {
	return poolError(format + ": " + arg)
}

type poolError string

func (e poolError) Error() string { return string(e) }
