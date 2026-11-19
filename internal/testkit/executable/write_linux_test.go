package executable_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/testkit/executable"
)

func TestWriteReplacesWriteOpenExecutable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture")
	old, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	if _, err := old.WriteString("#!/bin/sh\nexit 0\n"); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command(path).Run(); !errors.Is(err, syscall.ETXTBSY) {
		t.Fatalf("execute write-open original: got %v, want ETXTBSY", err)
	}
	if err := executable.Write(path, []byte("#!/bin/sh\nprintf safe"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(path).CombinedOutput()
	if err != nil || string(out) != "safe" {
		t.Fatalf("execute replacement: output %q, error %v", out, err)
	}
}

func TestWriteUnderForkPressure(t *testing.T) {
	dir := t.TempDir()
	truePath, err := exec.LookPath("true")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	var wg sync.WaitGroup
	failures := make(chan error, 16)
	for range 16 {
		wg.Go(func() {
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				if err := exec.CommandContext(ctx, truePath).Run(); err != nil && ctx.Err() == nil {
					failures <- err
					return
				}
			}
		})
	}
	t.Cleanup(func() { cancel(); wg.Wait() })
	script := []byte("#!/bin/sh\nexit 0\n#" + strings.Repeat("x", 1<<20) + "\n")
	for i := range 200 {
		if err := ctx.Err(); err != nil {
			t.Fatalf("fork pressure exceeded deadline: %v", err)
		}
		select {
		case err := <-failures:
			t.Fatalf("fork pressure child: %v", err)
		default:
		}
		path := filepath.Join(dir, fmt.Sprintf("fixture-%d", i))
		if err := executable.Write(path, script, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := exec.CommandContext(ctx, path).Run(); err != nil {
			t.Fatalf("execute fixture %d: %v", i, err)
		}
	}
	cancel()
	wg.Wait()
	select {
	case err := <-failures:
		t.Fatalf("fork pressure child: %v", err)
	default:
	}
}
