package left

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestOverlapProbe rendezvous with the right package. A serialized pass cannot
// satisfy it: the first process waits until its deadline and fails.
func TestOverlapProbe(t *testing.T) {
	dir := os.Getenv("PEASANT_TESTGATE_OVERLAP_DIR")
	if dir == "" {
		t.Skip("set PEASANT_TESTGATE_OVERLAP_DIR to run the overlap probe")
	}
	if err := os.WriteFile(filepath.Join(dir, "left"), []byte("left"), 0o644); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "right")); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("right probe was not running at the same time")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
