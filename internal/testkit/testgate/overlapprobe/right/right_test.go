package right

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestOverlapProbe rendezvous with the left package. A serialized pass cannot
// satisfy it: the first process waits until its deadline and fails.
func TestOverlapProbe(t *testing.T) {
	dir := os.Getenv("PEASANT_TESTGATE_OVERLAP_DIR")
	if dir == "" {
		t.Skip("set PEASANT_TESTGATE_OVERLAP_DIR to run the overlap probe")
	}
	if err := os.WriteFile(filepath.Join(dir, "right"), []byte("right"), 0o644); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "left")); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("left probe was not running at the same time")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
