//go:build e2e

package e2e

import (
	_ "embed"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/fingerprint.yaml
var fingerprintYAML []byte

func TestDeveloperStateFingerprintDetectsTailChanges(t *testing.T) {
	var fixtures struct {
		Required []string `yaml:"required_names"`
		Cases    []struct {
			Name     string `yaml:"name"`
			Bytes    int64  `yaml:"bytes"`
			Excluded bool   `yaml:"excluded"`
		} `yaml:"cases"`
	}
	if err := yaml.Unmarshal(fingerprintYAML, &fixtures); err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, fixture := range fixtures.Cases {
		if fixture.Name == "" || seen[fixture.Name] || fixture.Bytes < 1 {
			t.Fatal("invalid fingerprint fixture")
		}
		seen[fixture.Name] = true
		t.Run(fixture.Name, func(t *testing.T) {
			root := t.TempDir()
			excluded := filepath.Join(root, "sandbox")
			if err := os.Mkdir(excluded, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "state.bin")
			if fixture.Excluded {
				path = filepath.Join(excluded, "state.bin")
			}
			file, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			if err := file.Truncate(fixture.Bytes); err != nil {
				t.Fatal(err)
			}
			info, err := file.Stat()
			if err != nil {
				t.Fatal(err)
			}
			before := fingerprintPaths(t, []string{root}, excluded)
			if _, err := file.WriteAt([]byte{1}, fixture.Bytes-1); err != nil {
				t.Fatal(err)
			}
			// Keep size and timestamps identical: only reading the complete
			// content can detect the last-byte change in the tracked file.
			if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
			after := fingerprintPaths(t, []string{root}, excluded)
			if unchanged := before.digest == after.digest; unchanged != fixture.Excluded {
				t.Fatalf("fingerprint unchanged=%t, excluded=%t", unchanged, fixture.Excluded)
			}
		})
	}
	for _, name := range fixtures.Required {
		if !seen[name] {
			t.Fatalf("missing required fixture %s", name)
		}
	}
}
