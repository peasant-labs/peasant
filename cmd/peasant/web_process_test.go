package main

import (
	_ "embed"
	"os"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/web_process_ownership.yaml
var webProcessOwnershipYAML []byte

func TestIsWebServerCommandLine(t *testing.T) {
	t.Parallel()
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name string   `yaml:"name"`
			Args []string `yaml:"args"`
			Want bool     `yaml:"want"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeNamedFixtureYAML(webProcessOwnershipYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			if got := isWebServerCommandLine(tc.Args, 18693); got != tc.Want {
				t.Fatalf("server command match = %v, want %v", got, tc.Want)
			}
		})
	}
}

func TestWebProcessRequiresExactExecutablePath(t *testing.T) {
	t.Parallel()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if !sameExecutablePath(self) {
		t.Fatal("current executable was not recognized")
	}
	other := filepath.Join(t.TempDir(), filepath.Base(self))
	if sameExecutablePath(other) {
		t.Fatal("another absolute path with the same executable name was accepted")
	}
	if sameExecutablePath(filepath.Base(self)) {
		t.Fatal("a basename without executable identity was accepted")
	}
}
