package defaults

import (
	"bytes"
	_ "embed"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/app_version.yaml
var appVersionFixtureBytes []byte

type appVersionFixture struct {
	ResolveCases  []resolveVersionCase  `yaml:"resolve_cases"`
	DevBuildCases []devBuildVersionCase `yaml:"dev_build_cases"`
}

type resolveVersionCase struct {
	Name             string `yaml:"name"`
	LDFlags          string `yaml:"ldflags"`
	BuildInfoVersion string `yaml:"build_info_version"`
	BuildInfoOK      bool   `yaml:"build_info_ok"`
	BuildInfoNil     bool   `yaml:"build_info_nil"`
	Want             string `yaml:"want"`
}

type devBuildVersionCase struct {
	Name              string   `yaml:"name"`
	DescribeTag       string   `yaml:"describe_tag"`
	DescribeError     string   `yaml:"describe_error"`
	CommitCount       string   `yaml:"commit_count"`
	CommitCountError  string   `yaml:"commit_count_error"`
	ShortHash         string   `yaml:"short_hash"`
	ShortHashError    string   `yaml:"short_hash_error"`
	Want              string   `yaml:"want"`
	WantErrorContains []string `yaml:"want_error_contains"`
}

func TestResolveVersion(t *testing.T) {
	fixture := loadAppVersionFixture(t)
	requireCaseNames(t, fixture.ResolveCases, map[string]struct{}{
		"ldflags-real-version-wins-over-build-info":           {},
		"dev-placeholder-falls-back-to-module-version":        {},
		"dev-placeholder-with-devel-sentinel-stays-dev":       {},
		"dev-placeholder-with-empty-module-version-stays-dev": {},
		"dev-placeholder-with-no-build-info-stays-dev":        {},
		"dev-placeholder-with-nil-build-info-stays-dev":       {},
		"empty-ldflags-falls-back-to-module-version":          {},
	})

	buildInfo := func(mainVersion string) func() (*debug.BuildInfo, bool) {
		return func() (*debug.BuildInfo, bool) {
			return &debug.BuildInfo{Main: debug.Module{Version: mainVersion}}, true
		}
	}

	for _, tc := range fixture.ResolveCases {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			got := resolveVersion(tc.LDFlags, tc.readBuildInfo(buildInfo))
			if got != AppVersion(tc.Want) {
				t.Errorf("resolveVersion(%q, ...) = %q, want %q", tc.LDFlags, got, tc.Want)
			}
		})
	}
}

func TestDevVersionScriptFixtures(t *testing.T) {
	fixture := loadAppVersionFixture(t)
	requireCaseNames(t, fixture.DevBuildCases, map[string]struct{}{
		"normal-git-metadata":                 {},
		"exact-tag-source-build-is-still-dev": {},
		"missing-release-tag-fails":           {},
		"invalid-commit-distance-fails":       {},
		"invalid-short-hash-fails":            {},
	})

	for _, tc := range fixture.DevBuildCases {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			fakeGit := writeFakeGit(t)
			cmd := exec.Command("sh", filepath.Join("..", "..", "scripts", "dev-version.sh"))
			cmd.Env = append(os.Environ(),
				"GIT_BIN="+fakeGit,
				"FAKE_GIT_DESCRIBE_TAG="+tc.DescribeTag,
				"FAKE_GIT_DESCRIBE_ERROR="+tc.DescribeError,
				"FAKE_GIT_COMMIT_COUNT="+tc.CommitCount,
				"FAKE_GIT_COMMIT_COUNT_ERROR="+tc.CommitCountError,
				"FAKE_GIT_SHORT_HASH="+tc.ShortHash,
				"FAKE_GIT_SHORT_HASH_ERROR="+tc.ShortHashError,
			)

			output, err := cmd.CombinedOutput()
			if len(tc.WantErrorContains) == 0 {
				if err != nil {
					t.Fatalf("dev-version.sh returned error: %v\n%s", err, output)
				}
				got := strings.TrimSpace(string(output))
				if got != tc.Want {
					t.Fatalf("dev-version.sh = %q, want %q", got, tc.Want)
				}
				return
			}

			if err == nil {
				t.Fatalf("dev-version.sh succeeded, want error containing %v\n%s", tc.WantErrorContains, output)
			}
			for _, want := range tc.WantErrorContains {
				if !strings.Contains(string(output), want) {
					t.Fatalf("dev-version.sh output missing %q:\n%s", want, output)
				}
			}
		})
	}
}

func (tc resolveVersionCase) readBuildInfo(buildInfo func(string) func() (*debug.BuildInfo, bool)) func() (*debug.BuildInfo, bool) {
	if !tc.BuildInfoOK {
		return func() (*debug.BuildInfo, bool) { return nil, false }
	}
	if tc.BuildInfoNil {
		return func() (*debug.BuildInfo, bool) { return nil, true }
	}
	return buildInfo(tc.BuildInfoVersion)
}

func loadAppVersionFixture(t *testing.T) appVersionFixture {
	t.Helper()
	var fixture appVersionFixture
	dec := yaml.NewDecoder(bytes.NewReader(appVersionFixtureBytes))
	dec.KnownFields(true)
	if err := dec.Decode(&fixture); err != nil {
		t.Fatalf("load app version fixture: %v", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		t.Fatalf("app version fixture must contain exactly one YAML document, got %v", err)
	}
	return fixture
}

func requireCaseNames[T interface{ caseName() string }](t *testing.T, cases []T, required map[string]struct{}) {
	t.Helper()
	seen := make(map[string]struct{}, len(cases))
	for _, tc := range cases {
		name := tc.caseName()
		if name == "" {
			t.Fatal("fixture case has empty name")
		}
		if _, ok := seen[name]; ok {
			t.Fatalf("duplicate fixture case name %q", name)
		}
		seen[name] = struct{}{}
	}
	for name := range required {
		if _, ok := seen[name]; !ok {
			t.Fatalf("fixture missing required case %q", name)
		}
	}
}

func (tc resolveVersionCase) caseName() string  { return tc.Name }
func (tc devBuildVersionCase) caseName() string { return tc.Name }

func writeFakeGit(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "git")
	const script = `#!/usr/bin/env sh
set -eu

if [ "$1" = "describe" ]; then
  [ "$2" = "--tags" ] && [ "$3" = "--abbrev=0" ] && [ "$4" = "--match" ] && [ "$5" = 'v[0-9]*' ] || exit 88
  if [ -n "${FAKE_GIT_DESCRIBE_ERROR:-}" ]; then
    printf '%s\n' "$FAKE_GIT_DESCRIBE_ERROR" >&2
    exit 1
  fi
  printf '%s\n' "${FAKE_GIT_DESCRIBE_TAG:-}"
  exit 0
fi

if [ "$1" = "rev-list" ]; then
  [ "$2" = "--count" ] || exit 88
  if [ -n "${FAKE_GIT_COMMIT_COUNT_ERROR:-}" ]; then
    printf '%s\n' "$FAKE_GIT_COMMIT_COUNT_ERROR" >&2
    exit 1
  fi
  printf '%s\n' "${FAKE_GIT_COMMIT_COUNT:-}"
  exit 0
fi

if [ "$1" = "rev-parse" ]; then
  [ "$2" = "--short=9" ] && [ "$3" = "HEAD" ] || exit 88
  if [ -n "${FAKE_GIT_SHORT_HASH_ERROR:-}" ]; then
    printf '%s\n' "$FAKE_GIT_SHORT_HASH_ERROR" >&2
    exit 1
  fi
  printf '%s\n' "${FAKE_GIT_SHORT_HASH:-}"
  exit 0
fi

exit 89
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake git: %v", err)
	}
	return path
}
