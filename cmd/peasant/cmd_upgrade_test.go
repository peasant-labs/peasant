package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

func TestUpgradeCommandRegisteredWithUpdateAlias(t *testing.T) {
	t.Parallel()
	root := buildRootCommand()
	if _, _, err := root.Find([]string{"upgrade"}); err != nil {
		t.Fatalf("find upgrade command: %v", err)
	}
	if _, _, err := root.Find([]string{"update"}); err != nil {
		t.Fatalf("find update alias: %v", err)
	}
}

func TestUpgradeManagedInstallAdviceFixtures(t *testing.T) {
	t.Parallel()
	fixture := loadUpgradeFixture(t)
	requireUpgradeCaseNames(t, fixture.ManagedInstallCases, map[string]struct{}{
		"dpkg":     {},
		"rpm":      {},
		"pacman":   {},
		"nix":      {},
		"homebrew": {},
	})
	for _, tc := range fixture.ManagedInstallCases {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			deps := upgradeDeps{
				Executable: func() (string, error) { return tc.Executable, nil },
				GOOS:       tc.GOOS,
				GOARCH:     tc.GOARCH,
				CommandOutput: func(_ context.Context, name string, args ...string) ([]byte, error) {
					for _, command := range tc.Commands {
						if command.Name == name && stringSlicesEqual(command.Args, args) {
							return []byte(command.Stdout), nil
						}
					}
					return nil, errors.New("not managed by this test command")
				},
				HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
					t.Fatal("managed install advice must not call the network")
					return nil, errors.New("unexpected request")
				})},
				InstallBinary: func(string, []byte, os.FileMode) error {
					t.Fatal("managed install advice must not replace the binary")
					return nil
				},
			}

			output, err := executeUpgradeCommandForTest(t, deps, tc.Args...)
			if err != nil {
				t.Fatalf("upgrade command returned error: %v\noutput:\n%s", err, output)
			}
			for _, want := range tc.OutputContains {
				if !strings.Contains(output, want) {
					t.Fatalf("managed advice output missing %q:\n%s", want, output)
				}
			}
		})
	}
}

func TestUpgradeRejectsUnsafeVersionBeforeAdvice(t *testing.T) {
	t.Parallel()
	deps := upgradeDeps{
		Executable: func() (string, error) { return "/usr/bin/peasant", nil },
		GOOS:       "linux",
		GOARCH:     "amd64",
		CommandOutput: func(_ context.Context, name string, args ...string) ([]byte, error) {
			if name == "dpkg-query" && strings.Join(args, " ") == "-S /usr/bin/peasant" {
				return []byte("peasant: /usr/bin/peasant\n"), nil
			}
			return nil, errors.New("not managed by this test command")
		},
		HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("unsafe version must fail before network use")
			return nil, errors.New("unexpected request")
		})},
		InstallBinary: func(string, []byte, os.FileMode) error {
			t.Fatal("unsafe version must fail before binary replacement")
			return nil
		},
	}

	output, err := executeUpgradeCommandForTest(t, deps, "--version", "0.5.0;rm")
	if err == nil {
		t.Fatalf("upgrade accepted unsafe --version; output:\n%s", output)
	}
	if !strings.Contains(err.Error(), "unsafe character") {
		t.Fatalf("error = %v, want unsafe character rejection", err)
	}
}

func TestUpgradeReleaseSelectionFixtures(t *testing.T) {
	t.Parallel()
	fixture := loadUpgradeFixture(t)
	requireUpgradeCaseNames(t, fixture.ReleaseSelectionCases, map[string]struct{}{
		"stable-default":               {},
		"stable-prerelease-opt-in":     {},
		"prerelease-current-default":   {},
		"stable-current-equals-target": {},
	})
	for _, tc := range fixture.ReleaseSelectionCases {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			server := newUpgradeReleaseServer(t, upgradeReleaseServerConfig{
				Latest:   tc.Latest.release(),
				Releases: upgradeFixtureReleases(tc.Releases),
			})
			defer server.Close()
			deps := upgradeTestDeps(t, server.URL, filepath.Join(t.TempDir(), "peasant"))
			deps.CurrentVersion = tc.CurrentVersion

			output, err := executeUpgradeCommandForTest(t, deps, tc.Args...)
			if err != nil {
				t.Fatalf("upgrade dry-run returned error: %v\noutput:\n%s", err, output)
			}
			for _, want := range tc.OutputContains {
				if !strings.Contains(output, want) {
					t.Fatalf("release-selection output missing %q:\n%s", want, output)
				}
			}
		})
	}
}

func TestUpgradeVersionOrderFixtures(t *testing.T) {
	t.Parallel()
	fixture := loadUpgradeFixture(t)
	requireUpgradeCaseNames(t, fixture.VersionOrderCases, map[string]struct{}{
		"stable-release-ordering":                 {},
		"release-candidate-ordering":              {},
		"dev-build-orders-after-its-base-rc":      {},
		"dev-build-orders-before-newer-rc":        {},
		"final-base-dev-build-orders-after-final": {},
		"equal-release-versions":                  {},
		"malformed-current-version-fails-safe":    {},
		"malformed-target-version-fails-safe":     {},
		"observed-dev-placeholder-fails-safe":     {},
	})
	for _, tc := range fixture.VersionOrderCases {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			got, err := compareUpgradeVersions(tc.CurrentVersion, tc.TargetVersion)
			if len(tc.ErrorContains) > 0 {
				if err == nil {
					t.Fatalf("compareUpgradeVersions succeeded, want error containing %v", tc.ErrorContains)
				}
				for _, want := range tc.ErrorContains {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("compareUpgradeVersions error missing %q:\n%s", want, err)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("compareUpgradeVersions returned error: %v", err)
			}
			if got.String() != tc.WantOrder {
				t.Fatalf("compareUpgradeVersions(%q, %q) = %s, want %s", tc.CurrentVersion, tc.TargetVersion, got, tc.WantOrder)
			}
		})
	}
}

func TestUpgradeRefusesDowngradeTargetsBeforeDownloadFixtures(t *testing.T) {
	t.Parallel()
	fixture := loadUpgradeFixture(t)
	requireUpgradeCaseNames(t, fixture.DowngradeRefusalCases, map[string]struct{}{
		"dev-build-refuses-older-stable":      {},
		"observed-dev-placeholder-fails-safe": {},
		"malformed-target-version-fails-safe": {},
	})
	for _, tc := range fixture.DowngradeRefusalCases {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			archive := makeUpgradeArchive(t, []byte("new binary"))
			assetName := "peasant_" + strings.TrimPrefix(tc.TargetVersion, "v") + "_linux_amd64.tar.gz"
			var assetRequests atomic.Int64
			server := newUpgradeReleaseServer(t, upgradeReleaseServerConfig{
				Tagged: map[string]upgradeRelease{
					normalizeUpgradeTag(tc.TargetVersion): newUpgradeTestRelease(normalizeUpgradeTag(tc.TargetVersion), assetName, "checksums.txt"),
				},
				Assets: map[string][]byte{
					assetName:       archive,
					"checksums.txt": []byte(fmt.Sprintf("%x  %s\n", sha256.Sum256(archive), assetName)),
				},
				AssetRequests: &assetRequests,
			})
			defer server.Close()
			deps := upgradeTestDeps(t, server.URL, filepath.Join(t.TempDir(), "peasant"))
			deps.CurrentVersion = tc.CurrentVersion
			deps.InstallBinary = func(string, []byte, os.FileMode) error {
				t.Fatal("downgrade refusal must not replace the binary")
				return nil
			}

			output, err := executeUpgradeCommandForTest(t, deps, "--version", tc.TargetVersion)
			if err == nil {
				t.Fatalf("upgrade accepted blocked target; output:\n%s", output)
			}
			if strings.Contains(output, "current:") || strings.Contains(output, "target:") || strings.Contains(output, "asset:") || strings.Contains(output, "path:") || strings.Contains(output, "dry run:") {
				t.Fatalf("blocked target wrote plan output before refusal:\n%s", output)
			}
			for _, want := range tc.ErrorContains {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("blocked target error missing %q:\n%s", want, err)
				}
			}
			if assetRequests.Load() != 0 {
				t.Fatalf("blocked target downloaded %d asset(s), want 0", assetRequests.Load())
			}
		})
	}
}

func TestUpgradeRawInstallDownloadsVerifiesAndReplacesBinary(t *testing.T) {
	t.Parallel()
	currentPath := filepath.Join(t.TempDir(), "peasant")
	if err := os.WriteFile(currentPath, []byte("old binary"), 0o755); err != nil {
		t.Fatalf("seed current binary: %v", err)
	}
	newBinary := []byte("new binary")
	archive := makeUpgradeArchive(t, newBinary)
	assetName := "peasant_9.9.9_linux_amd64.tar.gz"
	server := newUpgradeReleaseServer(t, upgradeReleaseServerConfig{
		Tagged: map[string]upgradeRelease{
			"v9.9.9": newUpgradeTestRelease("v9.9.9", assetName, "checksums.txt"),
		},
		Assets: map[string][]byte{
			assetName:       archive,
			"checksums.txt": []byte(fmt.Sprintf("%x  %s\n", sha256.Sum256(archive), assetName)),
		},
	})
	defer server.Close()
	deps := upgradeTestDeps(t, server.URL, currentPath)

	output, err := executeUpgradeCommandForTest(t, deps, "--version", "9.9.9")
	if err != nil {
		t.Fatalf("upgrade install returned error: %v\noutput:\n%s", err, output)
	}
	got, err := os.ReadFile(currentPath)
	if err != nil {
		t.Fatalf("read replaced binary: %v", err)
	}
	if !bytes.Equal(got, newBinary) {
		t.Fatalf("binary content = %q, want %q", got, newBinary)
	}
	info, err := os.Stat(currentPath)
	if err != nil {
		t.Fatalf("stat replaced binary: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("binary mode = %v, want 0755", info.Mode().Perm())
	}
	if !strings.Contains(output, "installed v9.9.9") {
		t.Fatalf("upgrade output did not report success:\n%s", output)
	}
}

func TestUpgradeRawInstallRejectsChecksumMismatchAndPreservesBinary(t *testing.T) {
	t.Parallel()
	currentPath := filepath.Join(t.TempDir(), "peasant")
	original := []byte("old binary")
	if err := os.WriteFile(currentPath, original, 0o755); err != nil {
		t.Fatalf("seed current binary: %v", err)
	}
	archive := makeUpgradeArchive(t, []byte("new binary"))
	assetName := "peasant_9.9.9_linux_amd64.tar.gz"
	server := newUpgradeReleaseServer(t, upgradeReleaseServerConfig{
		Tagged: map[string]upgradeRelease{
			"v9.9.9": newUpgradeTestRelease("v9.9.9", assetName, "checksums.txt"),
		},
		Assets: map[string][]byte{
			assetName:       archive,
			"checksums.txt": []byte(strings.Repeat("0", sha256.Size*2) + "  " + assetName + "\n"),
		},
	})
	defer server.Close()
	deps := upgradeTestDeps(t, server.URL, currentPath)

	output, err := executeUpgradeCommandForTest(t, deps, "--version", "9.9.9")
	if err == nil {
		t.Fatalf("upgrade install succeeded with a checksum mismatch; output:\n%s", output)
	}
	if !strings.Contains(err.Error(), "checksum did not match") {
		t.Fatalf("error = %v, want checksum mismatch", err)
	}
	got, readErr := os.ReadFile(currentPath)
	if readErr != nil {
		t.Fatalf("read preserved binary: %v", readErr)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("binary changed after checksum failure: got %q want %q", got, original)
	}
}

type upgradeReleaseServerConfig struct {
	Latest        upgradeRelease
	Releases      []upgradeRelease
	Tagged        map[string]upgradeRelease
	Assets        map[string][]byte
	AssetRequests *atomic.Int64
}

type upgradeFixture struct {
	ManagedInstallCases   []upgradeManagedInstallCase   `yaml:"managed_install_cases"`
	ReleaseSelectionCases []upgradeReleaseSelectionCase `yaml:"release_selection_cases"`
	VersionOrderCases     []upgradeVersionOrderCase     `yaml:"version_order_cases"`
	DowngradeRefusalCases []upgradeDowngradeRefusalCase `yaml:"downgrade_refusal_cases"`
}

type upgradeManagedInstallCase struct {
	Name           string                  `yaml:"name"`
	Executable     string                  `yaml:"executable"`
	GOOS           string                  `yaml:"goos"`
	GOARCH         string                  `yaml:"goarch"`
	Args           []string                `yaml:"args"`
	Commands       []upgradeCommandFixture `yaml:"commands"`
	OutputContains []string                `yaml:"output_contains"`
}

type upgradeReleaseSelectionCase struct {
	Name           string                  `yaml:"name"`
	CurrentVersion string                  `yaml:"current_version"`
	Args           []string                `yaml:"args"`
	Latest         upgradeReleaseFixture   `yaml:"latest"`
	Releases       []upgradeReleaseFixture `yaml:"releases"`
	OutputContains []string                `yaml:"output_contains"`
}

type upgradeVersionOrderCase struct {
	Name           string   `yaml:"name"`
	CurrentVersion string   `yaml:"current_version"`
	TargetVersion  string   `yaml:"target_version"`
	WantOrder      string   `yaml:"want_order"`
	ErrorContains  []string `yaml:"error_contains"`
}

type upgradeDowngradeRefusalCase struct {
	Name           string   `yaml:"name"`
	CurrentVersion string   `yaml:"current_version"`
	TargetVersion  string   `yaml:"target_version"`
	ErrorContains  []string `yaml:"error_contains"`
}

type upgradeReleaseFixture struct {
	Tag    string   `yaml:"tag"`
	Draft  bool     `yaml:"draft"`
	Assets []string `yaml:"assets"`
}

type upgradeCommandFixture struct {
	Name   string   `yaml:"name"`
	Args   []string `yaml:"args"`
	Stdout string   `yaml:"stdout"`
}

func loadUpgradeFixture(t *testing.T) upgradeFixture {
	t.Helper()
	data, err := os.ReadFile("testdata/upgrade.yaml")
	if err != nil {
		t.Fatalf("read upgrade fixture: %v", err)
	}
	var fixture upgradeFixture
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode upgrade fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			t.Fatal("upgrade fixture must contain exactly one YAML document")
		}
		t.Fatalf("decode trailing upgrade fixture document: %v", err)
	}
	return fixture
}

type namedUpgradeCase interface {
	upgradeCaseName() string
}

func (c upgradeManagedInstallCase) upgradeCaseName() string   { return c.Name }
func (c upgradeReleaseSelectionCase) upgradeCaseName() string { return c.Name }
func (c upgradeVersionOrderCase) upgradeCaseName() string     { return c.Name }
func (c upgradeDowngradeRefusalCase) upgradeCaseName() string { return c.Name }

func requireUpgradeCaseNames[T namedUpgradeCase](t *testing.T, cases []T, required map[string]struct{}) {
	t.Helper()
	seen := make(map[string]struct{}, len(cases))
	for _, tc := range cases {
		seen[tc.upgradeCaseName()] = struct{}{}
	}
	for name := range required {
		if _, ok := seen[name]; !ok {
			t.Fatalf("upgrade fixture is missing required case %q", name)
		}
	}
}

func (f upgradeReleaseFixture) release() upgradeRelease {
	release := upgradeRelease{TagName: f.Tag, Draft: f.Draft}
	for _, asset := range f.Assets {
		release.Assets = append(release.Assets, upgradeAsset{Name: asset})
	}
	return release
}

func upgradeFixtureReleases(fixtures []upgradeReleaseFixture) []upgradeRelease {
	releases := make([]upgradeRelease, len(fixtures))
	for i, fixture := range fixtures {
		releases[i] = fixture.release()
	}
	return releases
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func newUpgradeReleaseServer(t *testing.T, cfg upgradeReleaseServerConfig) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assetPrefix := "/assets/"
		if strings.HasPrefix(r.URL.Path, assetPrefix) {
			if cfg.AssetRequests != nil {
				cfg.AssetRequests.Add(1)
			}
			name := strings.TrimPrefix(r.URL.Path, assetPrefix)
			data, ok := cfg.Assets[name]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(data)
			return
		}

		tagPrefix := "/repos/peasant-labs/peasant/releases/tags/"
		if strings.HasPrefix(r.URL.Path, tagPrefix) {
			tag := strings.TrimPrefix(r.URL.Path, tagPrefix)
			release, ok := cfg.Tagged[tag]
			if !ok {
				http.NotFound(w, r)
				return
			}
			writeUpgradeJSON(t, w, withUpgradeAssetURLs(release, r.Host))
			return
		}

		switch r.URL.Path {
		case "/repos/peasant-labs/peasant/releases/latest":
			writeUpgradeJSON(t, w, withUpgradeAssetURLs(cfg.Latest, r.Host))
		case "/repos/peasant-labs/peasant/releases":
			releases := make([]upgradeRelease, len(cfg.Releases))
			for i, release := range cfg.Releases {
				releases[i] = withUpgradeAssetURLs(release, r.Host)
			}
			writeUpgradeJSON(t, w, releases)
		default:
			http.NotFound(w, r)
		}
	}))
	return server
}

func writeUpgradeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}

func withUpgradeAssetURLs(release upgradeRelease, host string) upgradeRelease {
	if release.HTMLURL == "" && release.TagName != "" {
		release.HTMLURL = "http://" + host + "/releases/tag/" + release.TagName
	}
	for i := range release.Assets {
		if release.Assets[i].BrowserDownloadURL == "" {
			release.Assets[i].BrowserDownloadURL = "http://" + host + "/assets/" + release.Assets[i].Name
		}
	}
	return release
}

func newUpgradeTestRelease(tag string, assets ...string) upgradeRelease {
	release := upgradeRelease{TagName: tag, HTMLURL: "https://example.invalid/releases/tag/" + tag}
	for _, name := range assets {
		release.Assets = append(release.Assets, upgradeAsset{Name: name})
	}
	return release
}

func upgradeTestDeps(t *testing.T, apiBaseURL, executablePath string) upgradeDeps {
	t.Helper()
	return upgradeDeps{
		Executable: func() (string, error) { return executablePath, nil },
		CommandOutput: func(context.Context, string, ...string) ([]byte, error) {
			return nil, errors.New("not managed by this test command")
		},
		APIBaseURL:     apiBaseURL + "/repos/peasant-labs/peasant",
		GOOS:           "linux",
		GOARCH:         "amd64",
		CurrentVersion: "v0.0.0",
	}
}

func executeUpgradeCommandForTest(t *testing.T, deps upgradeDeps, args ...string) (string, error) {
	t.Helper()
	root := &cobra.Command{Use: "peasant"}
	cmd := buildUpgradeCommand(deps)
	root.AddCommand(cmd)
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs(append([]string{cmd.Name()}, args...))
	err := root.Execute()
	return output.String(), err
}

func makeUpgradeArchive(t *testing.T, binary []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "peasant", Mode: 0o755, Size: int64(len(binary))}); err != nil {
		t.Fatalf("write tar header: %v", err)
	}
	if _, err := tw.Write(binary); err != nil {
		t.Fatalf("write tar binary: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
