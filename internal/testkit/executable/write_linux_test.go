package executable_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

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
