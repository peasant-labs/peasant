package kickstart_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// readMovedDiscoveryFixture reads a capture that now lives with the neutral
// discovery package. The terminal tests keep asserting the same bytes.
func readMovedDiscoveryFixture(t *testing.T, name string) []byte {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("locate moved discovery fixture %s: the caller path is unavailable", name)
	}
	path := filepath.Join(filepath.Dir(file), "..", "..", "discovery", "testdata", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read moved discovery fixture %s at %s: %v\nwhat: the pre-move capture could not be read.\nwhy: %v.\nwhere: %s.\nwhen: loading a terminal discovery fixture.\nmeans: the capture was not checked.\nfix: restore the named file under internal/discovery/testdata.", name, path, err, err, path)
	}
	if len(data) == 0 {
		t.Fatalf("moved discovery fixture %s at %s is empty", name, path)
	}
	return data
}
