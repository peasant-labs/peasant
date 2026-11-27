package hookguard

import (
	_ "embed"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/probes.yaml
var probesYAML []byte

// TestHookGuardProbes runs the guard over each probe package and requires
// exactly the listed findings, so the guard is shown to catch each violation
// it claims to.
func TestHookGuardProbes(t *testing.T) {
	var fixture struct {
		Hooks  []Hook `yaml:"hooks"`
		Probes []struct {
			Name     string   `yaml:"name"`
			Source   string   `yaml:"source"`
			Findings []string `yaml:"findings"`
		} `yaml:"probes"`
	}
	if err := yaml.Unmarshal(probesYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Probes) == 0 || len(fixture.Hooks) == 0 {
		t.Fatal("the probe fixture names no probes or no hooks")
	}
	for _, probe := range fixture.Probes {
		t.Run(probe.Name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "probe.go"), []byte(probe.Source), 0o600); err != nil {
				t.Fatal(err)
			}
			// A test file may set a hook; the guard never reads it.
			if err := os.WriteFile(filepath.Join(dir, "probe_test.go"), []byte("package probe\nfunc set() { mutationHook = nil }\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := Check(dir, fixture.Hooks)
			if err != nil {
				t.Fatal(err)
			}
			unmatched := append([]string(nil), got...)
			for _, want := range probe.Findings {
				found := -1
				for i, finding := range unmatched {
					if strings.Contains(finding, want) {
						found = i
						break
					}
				}
				if found < 0 {
					t.Fatalf("missing finding %q in %q", want, got)
				}
				unmatched = append(unmatched[:found], unmatched[found+1:]...)
			}
			if len(unmatched) != 0 {
				t.Fatalf("unexpected findings %q", unmatched)
			}
		})
	}
}
