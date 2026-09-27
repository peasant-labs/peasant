package testutil

import (
	"errors"
	"io/fs"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/testkit/fsdecorator"
)

func TestGatedFS_HoldsAndReleasesOneOperation(t *testing.T) {
	t.Parallel()
	mem := NewMemFS()
	const path = "/out/session--transcript.jsonl"
	if err := mem.WriteFile(path, []byte("first"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	gated := NewGatedFS(mem)
	if gated.Reached() != nil {
		t.Fatal("Reached must be nil before a gate is armed")
	}
	gated.Arm(fsdecorator.Gate{Op: fsdecorator.OpWriteFile, Path: path})

	done := make(chan error, 1)
	go func() {
		done <- gated.WriteFile(path, []byte("second"), 0o600)
	}()

	select {
	case <-gated.Reached():
	case <-time.After(5 * time.Second):
		t.Fatal("the armed write never reached the gate")
	}
	// The hold is before the inner filesystem, so the old content is still there.
	if got, err := mem.ReadFile(path); err != nil || string(got) != "first" {
		t.Fatalf("held write touched the inner filesystem: %q, %v", got, err)
	}
	gated.Release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("released write: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Release did not unblock the held write")
	}
	if got, err := mem.ReadFile(path); err != nil || string(got) != "second" {
		t.Fatalf("released write did not land: %q, %v", got, err)
	}
	// Release is idempotent, and a consumed gate lets the next call through.
	gated.Release()
	if err := gated.WriteFile(path, []byte("third"), 0o600); err != nil {
		t.Fatalf("post-release write: %v", err)
	}
	if got, _ := mem.ReadFile(path); string(got) != "third" {
		t.Fatalf("post-release content = %q, want third", got)
	}
}

func TestGatedFS_NonMatchingOperationPassesThrough(t *testing.T) {
	t.Parallel()
	mem := NewMemFS()
	gated := NewGatedFS(mem)
	gated.Arm(fsdecorator.Gate{Op: fsdecorator.OpWriteFile, Path: "/gated"})
	if err := gated.WriteFile("/other", []byte("ok"), 0o600); err != nil {
		t.Fatalf("non-matching write must not block: %v", err)
	}
}

func TestBoundedFS_ReadOnlyRefusesEveryMutation(t *testing.T) {
	t.Parallel()
	mem := NewMemFS()
	if err := mem.WriteFile("/seed", []byte("seed"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	bounded := NewBoundedFS(mem)
	bounded.ReadOnly()

	mutations := []struct {
		name string
		run  func() error
	}{
		{"WriteFile", func() error { return bounded.WriteFile("/new", []byte("x"), 0o600) }},
		{"MkdirAll", func() error { return bounded.MkdirAll("/dir", 0o700) }},
		{"Rename", func() error { return bounded.Rename("/seed", "/moved") }},
		{"Remove", func() error { return bounded.Remove("/seed") }},
		{"RemoveAll", func() error { return bounded.RemoveAll("/seed") }},
		{"CopyFile", func() error { return bounded.CopyFile("/seed", "/copy", 0o600) }},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			if err := mutation.run(); !errors.Is(err, fs.ErrPermission) {
				t.Fatalf("%s on a read-only filesystem = %v, want fs.ErrPermission", mutation.name, err)
			}
		})
	}
	if _, err := bounded.ReadFile("/seed"); err != nil {
		t.Fatalf("read-only must not refuse a read: %v", err)
	}
}

func TestBoundedFS_MaxCallsRefusesTheCallPastTheBound(t *testing.T) {
	t.Parallel()
	mem := NewMemFS()
	const path = "/out/metadata.json"
	if err := mem.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	bounded := NewBoundedFS(mem)
	bounded.Limit(fsdecorator.Bound{Op: fsdecorator.OpReadFile, Path: path, MaxCalls: 1})

	if _, err := bounded.ReadFile(path); err != nil {
		t.Fatalf("first read must pass: %v", err)
	}
	if _, err := bounded.ReadFile(path); !errors.Is(err, ErrBoundExceeded) {
		t.Fatalf("second read = %v, want ErrBoundExceeded", err)
	}
}

func TestBoundedFS_MaxBytesBoundsTheReadPayload(t *testing.T) {
	t.Parallel()
	mem := NewMemFS()
	const path = "/out/transcript.jsonl"
	if err := mem.WriteFile(path, []byte("0123456789"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	bounded := NewBoundedFS(mem)
	bounded.Limit(fsdecorator.Bound{Op: fsdecorator.OpReadFile, Path: path, MaxBytes: 4})

	got, err := bounded.ReadFile(path)
	if err != nil {
		t.Fatalf("bounded read: %v", err)
	}
	if string(got) != "0123" {
		t.Fatalf("bounded read = %q, want 0123", got)
	}
}
