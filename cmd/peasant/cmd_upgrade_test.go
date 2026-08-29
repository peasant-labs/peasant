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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
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

func TestUpgradeManagedDpkgPrintsPackageAdviceWithoutNetwork(t *testing.T) {
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
			t.Fatal("managed install advice must not call the network")
			return nil, errors.New("unexpected request")
		})},
		InstallBinary: func(string, []byte, os.FileMode) error {
			t.Fatal("managed install advice must not replace the binary")
			return nil
		},
	}

	output, err := executeUpgradeCommandForTest(t, deps, "--version", "0.5.0-rc2")
	if err != nil {
		t.Fatalf("upgrade command returned error: %v\noutput:\n%s", err, output)
	}
	for _, want := range []string{
		"managed by dpkg package \"peasant\"",
		"No files were changed",
		"peasant_0.5.0-rc2_linux_amd64.deb",
		"sudo apt install",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("managed advice output missing %q:\n%s", want, output)
		}
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

func TestUpgradeRawDryRunSelectsNewestPrerelease(t *testing.T) {
	t.Parallel()
	server := newUpgradeReleaseServer(t, upgradeReleaseServerConfig{
		Releases: []upgradeRelease{
			newUpgradeTestRelease("v0.5.0-rc2", "peasant_0.5.0-rc2_linux_amd64.tar.gz", "checksums.txt"),
			newUpgradeTestRelease("v0.4.0", "peasant_0.4.0_linux_amd64.tar.gz", "checksums.txt"),
		},
	})
	defer server.Close()
	deps := upgradeTestDeps(t, server.URL, filepath.Join(t.TempDir(), "peasant"))

	output, err := executeUpgradeCommandForTest(t, deps, "--prerelease", "--dry-run")
	if err != nil {
		t.Fatalf("upgrade dry-run returned error: %v\noutput:\n%s", err, output)
	}
	for _, want := range []string{"target:  v0.5.0-rc2", "asset:   peasant_0.5.0-rc2_linux_amd64.tar.gz", "dry run: no files were changed"} {
		if !strings.Contains(output, want) {
			t.Fatalf("dry-run output missing %q:\n%s", want, output)
		}
	}
}

func TestUpgradeRawDryRunCurrentPrereleaseSelectsNewestPrereleaseByDefault(t *testing.T) {
	t.Parallel()
	server := newUpgradeReleaseServer(t, upgradeReleaseServerConfig{
		Latest: newUpgradeTestRelease("v0.4.0", "peasant_0.4.0_linux_amd64.tar.gz", "checksums.txt"),
		Releases: []upgradeRelease{
			newUpgradeTestRelease("v0.5.0-rc2", "peasant_0.5.0-rc2_linux_amd64.tar.gz", "checksums.txt"),
			newUpgradeTestRelease("v0.4.0", "peasant_0.4.0_linux_amd64.tar.gz", "checksums.txt"),
		},
	})
	defer server.Close()
	deps := upgradeTestDeps(t, server.URL, filepath.Join(t.TempDir(), "peasant"))
	var output bytes.Buffer

	err := runUpgradeCommand(context.Background(), &output, upgradeOptions{CurrentVersion: "0.5.0-rc1", DryRun: true}, deps)
	if err != nil {
		t.Fatalf("upgrade dry-run returned error: %v\noutput:\n%s", err, output.String())
	}
	if !strings.Contains(output.String(), "target:  v0.5.0-rc2") {
		t.Fatalf("current prerelease did not select newest prerelease:\n%s", output.String())
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
	Latest   upgradeRelease
	Releases []upgradeRelease
	Tagged   map[string]upgradeRelease
	Assets   map[string][]byte
}

func newUpgradeReleaseServer(t *testing.T, cfg upgradeReleaseServerConfig) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assetPrefix := "/assets/"
		if strings.HasPrefix(r.URL.Path, assetPrefix) {
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
		APIBaseURL: apiBaseURL + "/repos/peasant-labs/peasant",
		GOOS:       "linux",
		GOARCH:     "amd64",
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
