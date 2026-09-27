package storetest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestConcurrentGoldenCopiesAreIsolated protects the property that makes this
// package safe to use from several test processes at once: every user gets its
// own copy of the shared template inside its own t.TempDir, so two concurrent
// opens can never share a database file. The template itself lives under the
// process temp dir, so a second process builds its own and the copies never
// cross a process boundary.
func TestConcurrentGoldenCopiesAreIsolated(t *testing.T) {
	t.Parallel()

	const workers = 8
	var mu sync.Mutex
	copies := map[string]int{}
	for i := 0; i < workers; i++ {
		t.Run(fmt.Sprintf("worker-%d", i), func(t *testing.T) {
			t.Parallel()
			path := CopyGoldenDB(t)
			goldenMu.Lock()
			template := goldenPath
			goldenMu.Unlock()
			if template == "" {
				t.Fatal("the golden template vanished while a user still holds a reference")
			}
			if !strings.HasPrefix(template, filepath.Clean(os.TempDir())+string(os.PathSeparator)) {
				t.Fatalf("golden template %s is not under the process temp dir %s", template, os.TempDir())
			}
			if err := os.WriteFile(filepath.Join(filepath.Dir(path), "isolation-marker"), []byte("private"), 0o600); err != nil {
				t.Fatalf("write a sibling of the copy: %v", err)
			}
			mu.Lock()
			copies[path]++
			mu.Unlock()
		})
	}

	t.Cleanup(func() {
		if len(copies) != workers {
			t.Fatalf("concurrent copies produced %d distinct paths, want one per worker (%d): %v", len(copies), workers, copies)
		}
		for path, count := range copies {
			if count != 1 {
				t.Fatalf("copy %s was handed out %d times", path, count)
			}
		}
	})
}
