package ingest_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
)

func TestPhysicalPathResolver_UsesPhysicalDirectoryIdentity(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	first := filepath.Join(root, "clone-a")
	second := filepath.Join(root, "clone-b")
	if err := os.Mkdir(first, 0o700); err != nil {
		t.Fatalf("create first clone directory: %v", err)
	}
	if err := os.Mkdir(second, 0o700); err != nil {
		t.Fatalf("create second clone directory: %v", err)
	}
	alias := filepath.Join(root, "clone-link")
	if err := os.Symlink(first, alias); err != nil {
		t.Fatalf("create clone symbolic link: %v", err)
	}

	resolver := ingest.NewPhysicalPathResolver()
	physical, err := resolver.Resolve(first + string(filepath.Separator) + ".")
	if err != nil {
		t.Fatalf("resolve physical clone path: %v", err)
	}
	throughLink, err := resolver.Resolve(alias)
	if err != nil {
		t.Fatalf("resolve symbolic-link clone path: %v", err)
	}
	other, err := resolver.Resolve(second)
	if err != nil {
		t.Fatalf("resolve distinct clone path: %v", err)
	}
	if physical != throughLink {
		t.Fatalf("physical and symbolic-link spellings produced different identities: %q != %q", physical, throughLink)
	}
	if physical == other {
		t.Fatalf("distinct clone directories produced one identity: %q", physical)
	}
	if !filepath.IsAbs(physical.String()) || physical.String() != filepath.Clean(first) {
		t.Fatalf("physical identity = %q, want cleaned absolute path %q", physical, filepath.Clean(first))
	}
}

func TestPhysicalPathResolver_RejectsEmptyPathWithActionableError(t *testing.T) {
	t.Parallel()
	_, err := ingest.NewPhysicalPathResolver().Resolve("")
	requireActionablePathError(t, err, "path is empty")
}

func TestPhysicalPathResolver_RejectsRelativePathWithActionableError(t *testing.T) {
	t.Parallel()
	_, err := ingest.NewPhysicalPathResolver().Resolve(filepath.Join("relative", "clone"))
	requireActionablePathError(t, err, "path is relative")
}

func TestPhysicalPathResolver_RejectsMissingPathWithActionableError(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "missing-clone")
	_, err := ingest.NewPhysicalPathResolver().Resolve(missing)
	requireActionablePathError(t, err, "directory does not exist")
}

func TestPhysicalPathResolver_RejectsUnresolvableSymlinkWithActionableError(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	first := filepath.Join(root, "loop-a")
	second := filepath.Join(root, "loop-b")
	if err := os.Symlink(filepath.Base(second), first); err != nil {
		t.Fatalf("create first loop link: %v", err)
	}
	if err := os.Symlink(filepath.Base(first), second); err != nil {
		t.Fatalf("create second loop link: %v", err)
	}
	_, err := ingest.NewPhysicalPathResolver().Resolve(first)
	requireActionablePathError(t, err, "symbolic links could not be resolved")
}

func requireActionablePathError(t *testing.T, err error, reason string) {
	t.Helper()
	if err == nil {
		t.Fatalf("Resolve returned no error, want actionable error containing %q", reason)
	}
	message := err.Error()
	if !strings.Contains(message, reason) ||
		!strings.Contains(message, "what:") ||
		!strings.Contains(message, "why:") ||
		!strings.Contains(message, "where:") ||
		!strings.Contains(message, "when:") ||
		!strings.Contains(message, "meaning:") ||
		!strings.Contains(message, "fix:") {
		t.Fatalf("resolver error is not actionable:\n%s", message)
	}
}
