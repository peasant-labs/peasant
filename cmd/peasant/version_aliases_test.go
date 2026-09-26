package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"gopkg.in/yaml.v3"
)

// versionAliasFixture is the YAML schema for
// testdata/cli/version_aliases.yaml.
type versionAliasFixture struct {
	Cases []versionAliasCase `yaml:"cases"`
}

type versionAliasCase struct {
	Name           string   `yaml:"name"`
	Args           []string `yaml:"args"`
	ExpectVersion  bool     `yaml:"expect_version"`
	SentinelError  bool     `yaml:"sentinel_error"`
	ExpectError    bool     `yaml:"expect_error"`
	ErrorContains  []string `yaml:"error_contains"`
	OutputContains []string `yaml:"output_contains"`
}

func loadVersionAliasFixture(t *testing.T) versionAliasFixture {
	t.Helper()
	data, err := os.ReadFile("testdata/cli/version_aliases.yaml")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	f, err := decodeVersionAliasFixture(data)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return f
}

func decodeVersionAliasFixture(data []byte) (versionAliasFixture, error) {
	var f versionAliasFixture
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&f); err != nil {
		return versionAliasFixture{}, fmt.Errorf("decode version alias fixture with known fields: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return versionAliasFixture{}, fmt.Errorf("version alias fixture must contain exactly one YAML document")
		}
		return versionAliasFixture{}, fmt.Errorf("decode trailing version alias fixture document: %w", err)
	}
	return f, nil
}

// TestVersionAliasFixtureIsStrict mirrors the strict-decoder guard each other
// fixture family carries: an unknown field and a trailing YAML document must
// both be rejected, so a misspelled case cannot silently stop running.
func TestVersionAliasFixtureIsStrict(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("testdata/cli/version_aliases.yaml")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	unknownField := append([]byte("unexpected_fixture_field: true\n"), data...)
	if _, err := decodeVersionAliasFixture(unknownField); err == nil || !strings.Contains(err.Error(), "field unexpected_fixture_field not found") {
		t.Fatalf("unknown field error = %v, want strict field rejection", err)
	}
	trailingDocument := append(append([]byte{}, data...), []byte("\n---\nunexpected: document\n")...)
	if _, err := decodeVersionAliasFixture(trailingDocument); err == nil || !strings.Contains(err.Error(), "exactly one YAML document") {
		t.Fatalf("trailing document error = %v, want single-document rejection", err)
	}
}

// TestVersionAliases drives every fixture case through the built production
// root command (buildRootCommand), exercising the alias's exit mapping in
// exitCodeFor together with the printed output — not only the printVersion
// helper.
func TestVersionAliases(t *testing.T) {
	t.Parallel()
	fixture := loadVersionAliasFixture(t)
	expectedLine := "peasant " + defaults.Version.String() + "\n"

	for _, tc := range fixture.Cases {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			root := buildRootCommand()

			var stdout, stderr bytes.Buffer
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			root.SetArgs(tc.Args)

			err := root.Execute()

			if tc.ExpectVersion {
				if stdout.String() != expectedLine {
					t.Errorf("stdout = %q, want the version line %q", stdout.String(), expectedLine)
				}
				if stderr.String() != "" {
					t.Errorf("stderr = %q, want empty (the version path must not leak an error line)", stderr.String())
				}
			}
			if tc.SentinelError {
				var requested *versionRequestedError
				if !errors.As(err, &requested) {
					t.Fatalf("err = %T %v, want *versionRequestedError", err, err)
				}
				if got := exitCodeFor(err); got != defaults.ExitOK {
					t.Errorf("exitCodeFor(err) = %v, want %v for the version alias", got, defaults.ExitOK)
				}
			}
			if tc.ExpectError {
				if err == nil {
					t.Fatal("expected an error, got nil")
				}
				for _, substr := range tc.ErrorContains {
					if !strings.Contains(err.Error(), substr) {
						t.Errorf("error missing %q; got %v", substr, err)
					}
				}
				if got := exitCodeFor(err); got != defaults.ExitFailure {
					t.Errorf("exitCodeFor(err) = %v, want %v", got, defaults.ExitFailure)
				}
				if stdout.String() == expectedLine {
					t.Error("failing command printed the version line; the alias must not fire on a shadowed --version")
				}
			}
			if !tc.SentinelError && !tc.ExpectError && err != nil {
				t.Errorf("unexpected error: %v; stdout: %q", err, stdout.String())
			}
			for _, substr := range tc.OutputContains {
				if !strings.Contains(stdout.String(), substr) {
					t.Errorf("stdout missing %q; got %q", substr, stdout.String())
				}
			}
		})
	}
}
