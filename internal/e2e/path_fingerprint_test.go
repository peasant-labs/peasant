package e2e

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

type pathFingerprint struct {
	paths  []string
	digest string
}

// fingerprintPaths is shared with the mounted hook E2E isolation guard. Keep
// this helper untagged so its memory and byte-change regressions run in make check.
// File contents are streamed; a large developer database must not be allocated
// in full merely to verify that the test left it unchanged.
func fingerprintPaths(t *testing.T, paths []string, excludedSubtree string) pathFingerprint {
	t.Helper()
	h := sha256.New()
	buffer := make([]byte, 32*1024)
	for _, root := range paths {
		_, _ = h.Write([]byte(root))
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				_, _ = h.Write([]byte(err.Error()))
				return nil
			}
			if path == excludedSubtree || strings.HasPrefix(path, excludedSubtree+string(os.PathSeparator)) {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			info, infoErr := d.Info()
			if infoErr != nil {
				return nil
			}
			_, _ = h.Write([]byte(path + info.Mode().String() + fmt.Sprint(info.Size(), info.ModTime().UnixNano())))
			if !d.IsDir() {
				file, openErr := os.Open(path)
				if openErr != nil {
					t.Fatalf("fingerprint developer state: cannot open %s: %v; isolation cannot be verified; check the file's readability", path, openErr)
				}
				_, readErr := io.CopyBuffer(h, file, buffer)
				closeErr := file.Close()
				if readErr != nil || closeErr != nil {
					t.Fatalf("fingerprint developer state: cannot finish reading %s: read=%v close=%v; isolation cannot be verified; check the file and retry", path, readErr, closeErr)
				}
			}
			return nil
		})
	}
	return pathFingerprint{paths: paths, digest: hex.EncodeToString(h.Sum(nil))}
}

//go:embed testdata/path-fingerprint.yaml
var pathFingerprintYAML []byte

type pathFingerprintFixtures struct {
	RequiredCaseNames []string `yaml:"required_case_names"`
	Cases             []struct {
		Name              string `yaml:"name"`
		File              string `yaml:"file"`
		FileBytes         int64  `yaml:"file_bytes"`
		Excluded          string `yaml:"excluded"`
		MaxAllocatedBytes uint64 `yaml:"max_allocated_bytes"`
		WantContentChange bool   `yaml:"want_content_change"`
	} `yaml:"cases"`
}

func loadPathFingerprintFixtures(t *testing.T) pathFingerprintFixtures {
	t.Helper()
	var fixture pathFingerprintFixtures
	if err := yaml.Unmarshal(pathFingerprintYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	present := make(map[string]bool)
	for _, tc := range fixture.Cases {
		if present[tc.Name] {
			t.Fatalf("duplicate fingerprint case %q", tc.Name)
		}
		present[tc.Name] = true
		if !filepath.IsLocal(tc.File) || !filepath.IsLocal(tc.Excluded) || tc.FileBytes < 1 || tc.FileBytes > 64*1024*1024 || tc.MaxAllocatedBytes == 0 {
			t.Fatalf("invalid synthetic fingerprint case %q", tc.Name)
		}
	}
	if err := testutil.RequireFixtureNames("path fingerprint", "case", fixture.RequiredCaseNames, present); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// Keep this test serial: TotalAlloc measures the process, and simultaneous
// fixture work would add allocations unrelated to the isolation guard.
func TestPathFingerprintBoundedMemory(t *testing.T) {
	for _, tc := range loadPathFingerprintFixtures(t).Cases {
		t.Run(tc.Name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, tc.File)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			file, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			// A sparse file models a large local database without allocating its
			// contents in the test or committing a large fixture to the repository.
			if err := file.Truncate(tc.FileBytes); err != nil {
				_ = file.Close()
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			roots := []string{root}
			excluded := filepath.Join(root, tc.Excluded)
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			first := fingerprintPaths(t, roots, excluded)
			runtime.ReadMemStats(&after)
			allocated := after.TotalAlloc - before.TotalAlloc
			t.Logf("fingerprinted %d-byte synthetic file with %d allocated bytes", tc.FileBytes, allocated)
			if allocated > tc.MaxAllocatedBytes {
				t.Errorf("fingerprinting a %d-byte file allocated %d bytes, limit %d", tc.FileBytes, allocated, tc.MaxAllocatedBytes)
			}
			if unchanged := fingerprintPaths(t, roots, excluded); unchanged.digest != first.digest {
				t.Fatal("unchanged file tree produced a different fingerprint")
			}
			file, err = os.OpenFile(path, os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, writeErr := file.WriteAt([]byte{1}, tc.FileBytes-1)
			closeErr := file.Close()
			if writeErr != nil || closeErr != nil {
				t.Fatalf("mutate synthetic file: write=%v close=%v", writeErr, closeErr)
			}
			// Restore size/mtime/mode evidence: only the last content byte differs.
			if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
			changed := fingerprintPaths(t, roots, excluded)
			if got := changed.digest != first.digest; got != tc.WantContentChange {
				t.Fatalf("content-only mutation changed fingerprint=%t, want %t", got, tc.WantContentChange)
			}
		})
	}
}
