package kickstart_test

import (
	"os"
	"path/filepath"
	"testing"
)

// readMovedDiscoveryFixture reads a capture that now lives with the neutral
// discovery package. The terminal tests keep asserting the same bytes.
//
// The path is package-relative, like the rest of this package's fixtures:
// `go test` runs each test binary in its package directory, which keeps the
// read working under -trimpath where runtime.Caller reports a module-relative
// path that no filesystem resolves.
func readMovedDiscoveryFixture(t *testing.T, name string) []byte {
	t.Helper()
	path := filepath.Join("..", "..", "discovery", "testdata", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read moved discovery fixture %s at %s: %v\nwhat: the pre-move capture could not be read.\nwhy: %v.\nwhere: %s.\nwhen: loading a terminal discovery fixture.\nmeans: the capture was not checked.\nfix: restore the named file under internal/discovery/testdata.", name, path, err, err, path)
	}
	if len(data) == 0 {
		t.Fatalf("moved discovery fixture %s at %s is empty", name, path)
	}
	return data
}
