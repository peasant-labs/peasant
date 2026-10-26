package config_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
)

// TestSaveAtomicYAML_WritesTheDocumentOrNothing saves an edited document: a
// valid one lands byte for byte, comments and unnamed keys included, and one
// Parse refuses leaves the file and its directory as they were.
func TestSaveAtomicYAML_WritesTheDocumentOrNothing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	document := []byte("# kept by hand\nversion: 1\npush:\n  license: CC0-1.0 # chosen once\n")
	if err := config.SaveAtomicYAML(path, document); err != nil {
		t.Fatalf("save a valid document: %v", err)
	}
	written, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(written, document) {
		t.Fatalf("saved file = %q (err %v), want the document byte for byte %q", written, err, document)
	}

	if err := config.SaveAtomicYAML(path, []byte("version: 1\npush:\n  method: sideways\n")); err == nil {
		t.Fatal("SaveAtomicYAML accepted a document Parse refuses")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, document) {
		t.Fatalf("a refused document changed the file: %q (err %v)", after, err)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(dir, ".config-*.yaml.tmp")); len(leftovers) != 0 {
		t.Fatalf("a refused document left temporary files: %v", leftovers)
	}
}
