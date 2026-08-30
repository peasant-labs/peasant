package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

const (
	upgradeAPIBaseURL     = "https://api.github.com/repos/peasant-labs/peasant"
	upgradeReleasePageURL = "https://github.com/peasant-labs/peasant/releases"
	upgradeHTTPTimeout    = 90 * time.Second
	upgradeProbeTimeout   = 2 * time.Second
	upgradeArchiveLimit   = 256 << 20
	upgradeBinaryLimit    = 128 << 20
)

type upgradeInstallKind int

const (
	upgradeInstallRaw upgradeInstallKind = iota
	upgradeInstallDPKG
	upgradeInstallRPM
	upgradeInstallPacman
	upgradeInstallNix
	upgradeInstallHomebrew
)

func (k upgradeInstallKind) String() string {
	switch k {
	case upgradeInstallRaw:
		return "raw archive"
	case upgradeInstallDPKG:
		return "dpkg"
	case upgradeInstallRPM:
		return "rpm"
	case upgradeInstallPacman:
		return "pacman"
	case upgradeInstallNix:
		return "nix"
	case upgradeInstallHomebrew:
		return "homebrew"
	default:
		return "unknown"
	}
}

type upgradeOptions struct {
	Version           string
	IncludePrerelease bool
	DryRun            bool
	Yes               bool
	AllowDowngrade    bool
	CurrentVersion    string
	VersionSet        bool
}

type upgradeDeps struct {
	Executable      func() (string, error)
	EvalSymlinks    func(string) (string, error)
	Stat            func(string) (fs.FileInfo, error)
	CommandOutput   func(context.Context, string, ...string) ([]byte, error)
	HTTPClient      *http.Client
	APIBaseURL      string
	GOOS            string
	GOARCH          string
	CurrentVersion  string
	InstallBinary   func(string, []byte, fs.FileMode) error
	StdinIsTerminal func(io.Reader) bool
}

type upgradeManagedInstall struct {
	Kind    upgradeInstallKind
	Package string
	Path    string
}

type upgradeRelease struct {
	TagName    string         `json:"tag_name"`
	HTMLURL    string         `json:"html_url"`
	Draft      bool           `json:"draft"`
	Prerelease bool           `json:"prerelease"`
	Assets     []upgradeAsset `json:"assets"`
}

type upgradeAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

func BuildUpgradeCommand() *cobra.Command {
	return buildUpgradeCommand(defaultUpgradeDeps())
}

func buildUpgradeCommand(deps upgradeDeps) *cobra.Command {
	deps = normalizeUpgradeDeps(deps)
	opts := upgradeOptions{CurrentVersion: deps.CurrentVersion}
	cmd := &cobra.Command{
		Use:     "upgrade",
		Aliases: []string{"update"},
		Short:   "Upgrade Peasant",
		Long: "Upgrade Peasant from GitHub release artifacts. Raw archive installs are replaced " +
			"in place after verifying checksums.txt. Package-manager-owned installs are not " +
			"modified; the command prints the manager-owned upgrade path so package metadata " +
			"stays correct. Stable builds use the latest stable release by default; prerelease " +
			"builds look for prerelease updates too. Use --prerelease to opt in from a stable " +
			"build, or --version for an exact release. Use --allow-downgrade with --version " +
			"only for an intentional rollback to an older release. Use --yes only when " +
			"automation has already reviewed and accepted the printed replacement plan.",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			runOpts := opts
			runOpts.VersionSet = cmd.Flags().Changed("version")
			return runUpgradeCommand(ctx, cmd.OutOrStdout(), cmd.InOrStdin(), runOpts, deps)
		},
	}
	cmd.Flags().StringVar(&opts.Version, "version", "", "Install a specific release tag or version")
	cmd.Flags().BoolVar(&opts.IncludePrerelease, "prerelease", false, "Allow the newest prerelease when --version is not set")
	cmd.Flags().BoolVar(&opts.DryRun, "dry-run", false, "Print the upgrade plan without changing files")
	cmd.Flags().BoolVar(&opts.Yes, "yes", false, "Accept the raw archive replacement plan without an interactive prompt")
	cmd.Flags().BoolVar(&opts.AllowDowngrade, "allow-downgrade", false, "Allow installing an older release when paired with --version")
	return cmd
}

func defaultUpgradeDeps() upgradeDeps {
	return upgradeDeps{
		Executable:      os.Executable,
		EvalSymlinks:    filepath.EvalSymlinks,
		Stat:            os.Stat,
		CommandOutput:   defaultUpgradeCommandOutput,
		HTTPClient:      &http.Client{Timeout: upgradeHTTPTimeout},
		APIBaseURL:      upgradeAPIBaseURL,
		GOOS:            runtime.GOOS,
		GOARCH:          runtime.GOARCH,
		CurrentVersion:  defaults.Version.String(),
		InstallBinary:   installPeasantBinary,
		StdinIsTerminal: upgradeInputIsTerminal,
	}
}

func normalizeUpgradeDeps(deps upgradeDeps) upgradeDeps {
	defaults := defaultUpgradeDeps()
	if deps.Executable == nil {
		deps.Executable = defaults.Executable
	}
	if deps.EvalSymlinks == nil {
		deps.EvalSymlinks = defaults.EvalSymlinks
	}
	if deps.Stat == nil {
		deps.Stat = defaults.Stat
	}
	if deps.CommandOutput == nil {
		deps.CommandOutput = defaults.CommandOutput
	}
	if deps.HTTPClient == nil {
		deps.HTTPClient = defaults.HTTPClient
	}
	if strings.TrimSpace(deps.APIBaseURL) == "" {
		deps.APIBaseURL = defaults.APIBaseURL
	}
	if deps.GOOS == "" {
		deps.GOOS = defaults.GOOS
	}
	if deps.GOARCH == "" {
		deps.GOARCH = defaults.GOARCH
	}
	if deps.CurrentVersion == "" {
		deps.CurrentVersion = defaults.CurrentVersion
	}
	if deps.InstallBinary == nil {
		deps.InstallBinary = defaults.InstallBinary
	}
	if deps.StdinIsTerminal == nil {
		deps.StdinIsTerminal = defaults.StdinIsTerminal
	}
	return deps
}

func runUpgradeCommand(ctx context.Context, out io.Writer, in io.Reader, opts upgradeOptions, deps upgradeDeps) error {
	if out == nil {
		out = io.Discard
	}
	deps = normalizeUpgradeDeps(deps)
	if opts.VersionSet || strings.TrimSpace(opts.Version) != "" {
		version, err := cleanUpgradeRequestedVersion(opts.Version)
		if err != nil {
			return err
		}
		opts.Version = version
	}
	if opts.Version == "" && !opts.IncludePrerelease && isUpgradePrerelease(opts.CurrentVersion) {
		opts.IncludePrerelease = true
	}
	if opts.AllowDowngrade && opts.Version == "" {
		return allowDowngradeWithoutVersionError()
	}
	if opts.Version != "" {
		handled, err := handleUpgradeVersionOrder(out, opts, normalizeUpgradeTag(opts.Version))
		if err != nil || handled {
			return err
		}
	}

	executablePath, err := deps.Executable()
	if err != nil {
		return upgradeActionableError(
			"the current peasant executable path could not be resolved",
			err.Error(),
			"peasant upgrade",
			"before checking the installation channel",
			"Peasant cannot safely replace or advise for an unknown binary path",
			"run `peasant version` from the binary you want to upgrade, then retry from that same shell",
		)
	}
	resolvedPath := executablePath
	if evaluated, err := deps.EvalSymlinks(executablePath); err == nil && evaluated != "" {
		resolvedPath = evaluated
	}

	paths := dedupeUpgradePaths(executablePath, resolvedPath)
	if managed, ok := detectManagedPeasantInstall(ctx, deps, paths); ok {
		printManagedUpgradeAdvice(out, managed, opts, deps)
		return nil
	}

	release, err := resolveUpgradeRelease(ctx, deps, opts)
	if err != nil {
		return err
	}
	if opts.Version == "" {
		handled, err := handleUpgradeVersionOrder(out, opts, release.TagName)
		if err != nil || handled {
			return err
		}
	}

	archiveName, err := upgradeArchiveName(release.TagName, deps.GOOS, deps.GOARCH)
	if err != nil {
		return err
	}
	archiveAsset, ok := release.asset(archiveName)
	if !ok {
		return upgradeActionableError(
			"the target release does not contain an archive for this platform",
			fmt.Sprintf("release %s is missing asset %s", release.TagName, archiveName),
			"peasant upgrade",
			"after selecting the release artifact",
			"Peasant cannot install a binary whose published checksum and archive are absent",
			fmt.Sprintf("open %s and choose a release that includes %s", releaseURL(release), archiveName),
		)
	}
	checksumsAsset, ok := release.asset("checksums.txt")
	if !ok {
		return upgradeActionableError(
			"the target release does not contain checksums.txt",
			fmt.Sprintf("release %s has no checksums.txt asset", release.TagName),
			"peasant upgrade",
			"before downloading the archive",
			"Peasant will not install an archive it cannot verify",
			fmt.Sprintf("open %s and choose a release with checksums.txt", releaseURL(release)),
		)
	}

	printRawUpgradePlan(out, opts, deps, release, archiveName, archiveAsset, checksumsAsset, resolvedPath)
	if opts.DryRun {
		fmt.Fprintln(out, "dry run: no files were changed")
		return nil
	}
	confirmed, err := confirmRawUpgradePlan(in, out, opts, deps)
	if err != nil {
		return err
	}
	if !confirmed {
		return nil
	}

	checksums, err := downloadUpgradeBytes(ctx, deps.HTTPClient, checksumsAsset.BrowserDownloadURL, upgradeArchiveLimit)
	if err != nil {
		return upgradeActionableError(
			"checksums.txt could not be downloaded",
			err.Error(),
			"peasant upgrade",
			"before downloading the Peasant archive",
			"Peasant cannot verify the release artifact",
			"check network access to GitHub releases, then retry",
		)
	}
	expectedChecksum, err := checksumForUpgradeAsset(checksums, archiveName)
	if err != nil {
		return err
	}

	archiveBytes, err := downloadUpgradeBytes(ctx, deps.HTTPClient, archiveAsset.BrowserDownloadURL, upgradeArchiveLimit)
	if err != nil {
		return upgradeActionableError(
			"the Peasant archive could not be downloaded",
			err.Error(),
			"peasant upgrade",
			"after checksums.txt was downloaded",
			"the existing binary was left untouched",
			"check network access to GitHub releases, then retry",
		)
	}
	actualChecksum := fmt.Sprintf("%x", sha256.Sum256(archiveBytes))
	if !strings.EqualFold(actualChecksum, expectedChecksum) {
		return upgradeActionableError(
			"the downloaded archive checksum did not match checksums.txt",
			fmt.Sprintf("%s expected %s but got %s", archiveName, expectedChecksum, actualChecksum),
			"peasant upgrade",
			"before extracting or replacing the local binary",
			"the existing binary was left untouched because the release artifact could be corrupt or tampered with",
			"delete any local partial downloads, check the release page, and retry when GitHub serves matching bytes",
		)
	}

	binaryBytes, mode, err := extractPeasantBinary(archiveBytes)
	if err != nil {
		return err
	}
	mode = upgradeInstallMode(deps, resolvedPath, mode)
	if err := deps.InstallBinary(resolvedPath, binaryBytes, mode); err != nil {
		return upgradeActionableError(
			"the verified Peasant binary could not replace the current executable",
			err.Error(),
			"peasant upgrade",
			"after the release archive passed checksum verification",
			"the existing binary may still be in place; Peasant does not remove configuration, data, or state during upgrade",
			"if this is a raw archive install under /usr/local/bin, rerun with sufficient permission; if a package manager owns the file, use that package manager",
		)
	}
	fmt.Fprintf(out, "installed %s at %s\n", release.TagName, resolvedPath)
	fmt.Fprintln(out, "Run `peasant version` to verify the new binary.")
	return nil
}

func defaultUpgradeCommandOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	probeCtx, cancel := context.WithTimeout(ctx, upgradeProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, name, args...)
	return cmd.CombinedOutput()
}

func upgradeInputIsTerminal(in io.Reader) bool {
	file, ok := in.(*os.File)
	return ok && term.IsTerminal(int(file.Fd()))
}

func printRawUpgradePlan(out io.Writer, opts upgradeOptions, deps upgradeDeps, release upgradeRelease, archiveName string, archiveAsset, checksumsAsset upgradeAsset, resolvedPath string) {
	fmt.Fprintln(out, "Upgrade plan:")
	fmt.Fprintf(out, "  current version: %s\n", opts.CurrentVersion)
	fmt.Fprintf(out, "  target version:  %s\n", release.TagName)
	fmt.Fprintf(out, "  machine: %s/%s\n", deps.GOOS, deps.GOARCH)
	fmt.Fprintf(out, "  archive: %s\n", archiveName)
	fmt.Fprintf(out, "  checksums source: %s\n", checksumsAsset.BrowserDownloadURL)
	fmt.Fprintf(out, "  download source: %s\n", archiveAsset.BrowserDownloadURL)
	fmt.Fprintln(out, "  temporary download location: memory only; no archive file is written")
	fmt.Fprintf(out, "  current binary to replace: %s\n", resolvedPath)
}

func confirmRawUpgradePlan(in io.Reader, out io.Writer, opts upgradeOptions, deps upgradeDeps) (bool, error) {
	if opts.Yes {
		fmt.Fprintln(out, "confirmation: --yes set; continuing without prompt")
		return true, nil
	}
	if in == nil || !deps.StdinIsTerminal(in) {
		return false, nonInteractiveUpgradeConfirmationError()
	}
	fmt.Fprint(out, "continue? [y/N] ")
	response, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && err != io.EOF {
		return false, upgradeActionableError(
			"the upgrade confirmation response could not be read",
			err.Error(),
			"peasant upgrade",
			"after printing the raw archive replacement plan and before downloading files",
			"Peasant refused to replace the current binary and no files were changed",
			"rerun from an interactive terminal and answer yes, or rerun with --yes only if automation has already reviewed the printed plan",
		)
	}
	response = strings.ToLower(strings.TrimSpace(response))
	if response != "y" && response != "yes" {
		fmt.Fprintln(out, "upgrade cancelled: no files were changed")
		return false, nil
	}
	return true, nil
}

func detectManagedPeasantInstall(ctx context.Context, deps upgradeDeps, paths []string) (upgradeManagedInstall, bool) {
	for _, path := range paths {
		if strings.HasPrefix(filepath.Clean(path), "/nix/store/") {
			return upgradeManagedInstall{Kind: upgradeInstallNix, Path: path}, true
		}
	}
	if install, ok := detectHomebrewPeasantInstall(ctx, deps, paths); ok {
		return install, true
	}
	for _, path := range paths {
		if out, err := deps.CommandOutput(ctx, "dpkg-query", "-S", path); err == nil {
			pkg := strings.TrimSpace(strings.SplitN(string(out), ":", 2)[0])
			if pkg == "" {
				pkg = "peasant"
			}
			return upgradeManagedInstall{Kind: upgradeInstallDPKG, Package: pkg, Path: path}, true
		}
		if out, err := deps.CommandOutput(ctx, "rpm", "-qf", path); err == nil {
			pkg := strings.TrimSpace(string(out))
			if pkg == "" {
				pkg = "peasant"
			}
			return upgradeManagedInstall{Kind: upgradeInstallRPM, Package: pkg, Path: path}, true
		}
		if out, err := deps.CommandOutput(ctx, "pacman", "-Qo", path); err == nil {
			pkg := parsePacmanOwner(string(out))
			if pkg == "" {
				pkg = "peasant-bin"
			}
			return upgradeManagedInstall{Kind: upgradeInstallPacman, Package: pkg, Path: path}, true
		}
	}
	return upgradeManagedInstall{}, false
}

func detectHomebrewPeasantInstall(ctx context.Context, deps upgradeDeps, paths []string) (upgradeManagedInstall, bool) {
	if out, err := deps.CommandOutput(ctx, "brew", "list", "--cask", "peasant"); err == nil && outputMentionsUpgradePath(string(out), paths) {
		return upgradeManagedInstall{Kind: upgradeInstallHomebrew, Package: "peasant", Path: paths[0]}, true
	}
	if out, err := deps.CommandOutput(ctx, "brew", "list", "peasant"); err == nil && outputMentionsUpgradePath(string(out), paths) {
		return upgradeManagedInstall{Kind: upgradeInstallHomebrew, Package: "peasant", Path: paths[0]}, true
	}
	if _, err := deps.CommandOutput(ctx, "brew", "list", "--cask", "--versions", "peasant"); err != nil {
		return upgradeManagedInstall{}, false
	}
	prefixOut, err := deps.CommandOutput(ctx, "brew", "--prefix")
	if err != nil {
		return upgradeManagedInstall{}, false
	}
	prefix := strings.TrimSpace(string(prefixOut))
	if prefix == "" {
		return upgradeManagedInstall{}, false
	}
	for _, path := range paths {
		clean := filepath.Clean(path)
		if clean == prefix || strings.HasPrefix(clean, prefix+string(os.PathSeparator)) {
			return upgradeManagedInstall{Kind: upgradeInstallHomebrew, Package: "peasant", Path: path}, true
		}
	}
	return upgradeManagedInstall{}, false
}

func printManagedUpgradeAdvice(out io.Writer, install upgradeManagedInstall, opts upgradeOptions, deps upgradeDeps) {
	if install.Package == "" {
		install.Package = "peasant"
	}
	fmt.Fprintf(out, "Peasant appears to be managed by %s", install.Kind)
	if install.Package != "" {
		fmt.Fprintf(out, " package %q", install.Package)
	}
	if install.Path != "" {
		fmt.Fprintf(out, " at %s", install.Path)
	}
	fmt.Fprintln(out, ".")
	fmt.Fprintln(out, "No files were changed because direct replacement would leave package-manager ownership metadata stale.")
	fmt.Fprintln(out, "Use the manager-owned upgrade path instead:")

	switch install.Kind {
	case upgradeInstallDPKG:
		if opts.Version != "" {
			version := strings.TrimPrefix(normalizeUpgradeTag(opts.Version), "v")
			asset := fmt.Sprintf("peasant_%s_linux_%s.deb", version, deps.GOARCH)
			fmt.Fprintf(out, "  curl -fLO \"https://github.com/peasant-labs/peasant/releases/download/v%s/%s\"\n", version, asset)
			fmt.Fprintf(out, "  curl -fLO \"https://github.com/peasant-labs/peasant/releases/download/v%s/checksums.txt\"\n", version)
			fmt.Fprintln(out, "  sha256sum --ignore-missing -c checksums.txt")
			fmt.Fprintf(out, "  sudo apt install ./%s\n", asset)
			return
		}
		fmt.Fprintln(out, "  Download the newer .deb from https://github.com/peasant-labs/peasant/releases")
		fmt.Fprintln(out, "  Then run sudo apt install with the downloaded .deb path.")
		fmt.Fprintln(out, "  For exact curl commands, rerun with --version 0.5.0-rc2 or another release version.")
	case upgradeInstallRPM:
		if opts.Version != "" {
			version := strings.TrimPrefix(normalizeUpgradeTag(opts.Version), "v")
			asset := fmt.Sprintf("peasant_%s_linux_%s.rpm", version, deps.GOARCH)
			fmt.Fprintf(out, "  curl -fLO \"https://github.com/peasant-labs/peasant/releases/download/v%s/%s\"\n", version, asset)
			fmt.Fprintf(out, "  curl -fLO \"https://github.com/peasant-labs/peasant/releases/download/v%s/checksums.txt\"\n", version)
			fmt.Fprintln(out, "  sha256sum --ignore-missing -c checksums.txt")
			fmt.Fprintf(out, "  sudo dnf install ./%s\n", asset)
			return
		}
		fmt.Fprintln(out, "  Download the newer .rpm from https://github.com/peasant-labs/peasant/releases")
		fmt.Fprintln(out, "  Then run sudo dnf install with the downloaded .rpm path.")
		fmt.Fprintln(out, "  For exact curl commands, rerun with --version 0.5.0-rc2 or another release version.")
	case upgradeInstallPacman:
		fmt.Fprintf(out, "  yay -Syu %s\n", install.Package)
		fmt.Fprintf(out, "  paru -Syu %s\n", install.Package)
	case upgradeInstallNix:
		fmt.Fprintln(out, "  nix profile upgrade peasant")
		fmt.Fprintln(out, "  # If no profile entry exists: nix profile install github:peasant-labs/peasant#peasant")
	case upgradeInstallHomebrew:
		fmt.Fprintln(out, "  brew update && brew upgrade --cask peasant")
	default:
		fmt.Fprintf(out, "  Open %s and follow the install guide for your channel.\n", upgradeReleasePageURL)
	}
}

func resolveUpgradeRelease(ctx context.Context, deps upgradeDeps, opts upgradeOptions) (upgradeRelease, error) {
	base := strings.TrimRight(deps.APIBaseURL, "/")
	if opts.Version != "" {
		tag := normalizeUpgradeTag(opts.Version)
		var release upgradeRelease
		if err := getUpgradeJSON(ctx, deps.HTTPClient, base+"/releases/tags/"+url.PathEscape(tag), &release); err != nil {
			return upgradeRelease{}, upgradeActionableError(
				"the requested Peasant release could not be resolved",
				err.Error(),
				"peasant upgrade --version",
				"while reading the GitHub release by tag",
				"Peasant cannot choose an archive or checksum for an unknown release",
				fmt.Sprintf("check that %s contains tag %s, then retry", upgradeReleasePageURL, tag),
			)
		}
		return release, nil
	}
	if opts.IncludePrerelease {
		var releases []upgradeRelease
		if err := getUpgradeJSON(ctx, deps.HTTPClient, base+"/releases?per_page=100", &releases); err != nil {
			return upgradeRelease{}, upgradeActionableError(
				"Peasant releases could not be listed",
				err.Error(),
				"peasant upgrade --prerelease",
				"while selecting the newest release or prerelease",
				"Peasant cannot choose a target archive",
				"check network access to the GitHub API, then retry",
			)
		}
		for _, release := range releases {
			if !release.Draft {
				return release, nil
			}
		}
		return upgradeRelease{}, upgradeActionableError(
			"no published Peasant release was available",
			"the GitHub releases response contained no non-draft release",
			"peasant upgrade --prerelease",
			"while selecting the newest release or prerelease",
			"Peasant cannot choose a target archive",
			fmt.Sprintf("open %s and choose an explicit --version", upgradeReleasePageURL),
		)
	}
	var release upgradeRelease
	if err := getUpgradeJSON(ctx, deps.HTTPClient, base+"/releases/latest", &release); err != nil {
		return upgradeRelease{}, upgradeActionableError(
			"the latest stable Peasant release could not be resolved",
			err.Error(),
			"peasant upgrade",
			"while reading the GitHub latest-release endpoint",
			"Peasant cannot choose a target archive",
			"check network access to the GitHub API, or pass --version for a known tag",
		)
	}
	return release, nil
}

func getUpgradeJSON(ctx context.Context, client *http.Client, endpoint string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "peasant-upgrade")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("GET %s returned %s: %s", endpoint, resp.Status, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode %s response: %w", endpoint, err)
	}
	return nil
}

func downloadUpgradeBytes(ctx context.Context, client *http.Client, endpoint string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "peasant-upgrade")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("GET %s returned %s: %s", endpoint, resp.Status, strings.TrimSpace(string(body)))
	}
	limited := io.LimitReader(resp.Body, limit+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("download from %s exceeded %d bytes", endpoint, limit)
	}
	return data, nil
}

func (r upgradeRelease) asset(name string) (upgradeAsset, bool) {
	for _, asset := range r.Assets {
		if asset.Name == name {
			return asset, true
		}
	}
	return upgradeAsset{}, false
}

func upgradeArchiveName(tag, goos, goarch string) (string, error) {
	if goos != "linux" && goos != "darwin" {
		return "", upgradeActionableError(
			"this platform is not supported by Peasant release archives",
			fmt.Sprintf("GOOS %q is not one of linux or darwin", goos),
			"peasant upgrade",
			"while choosing the release asset",
			"Peasant cannot select a published archive for this host",
			fmt.Sprintf("open %s and follow the install guide for this platform", upgradeReleasePageURL),
		)
	}
	if goarch != "amd64" && goarch != "arm64" {
		return "", upgradeActionableError(
			"this CPU architecture is not supported by Peasant release archives",
			fmt.Sprintf("GOARCH %q is not one of amd64 or arm64", goarch),
			"peasant upgrade",
			"while choosing the release asset",
			"Peasant cannot select a published archive for this host",
			fmt.Sprintf("open %s and follow the install guide for this platform", upgradeReleasePageURL),
		)
	}
	version := strings.TrimPrefix(tag, "v")
	return fmt.Sprintf("peasant_%s_%s_%s.tar.gz", version, goos, goarch), nil
}

func checksumForUpgradeAsset(checksums []byte, assetName string) (string, error) {
	for _, line := range strings.Split(string(checksums), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || filepath.Base(fields[len(fields)-1]) != assetName {
			continue
		}
		hash := strings.TrimSpace(fields[0])
		if len(hash) != sha256.Size*2 {
			return "", upgradeActionableError(
				"checksums.txt contains an invalid checksum length",
				fmt.Sprintf("%s has checksum %q", assetName, hash),
				"peasant upgrade",
				"while verifying release metadata",
				"Peasant will not install an archive without a valid SHA-256 checksum",
				"check the release checksums.txt and retry after the release is repaired",
			)
		}
		return strings.ToLower(hash), nil
	}
	return "", upgradeActionableError(
		"checksums.txt does not name the selected archive",
		fmt.Sprintf("no checksum entry matched %s", assetName),
		"peasant upgrade",
		"while verifying release metadata",
		"Peasant will not install an archive it cannot verify",
		"check the release checksums.txt and retry after the release is repaired",
	)
}

func extractPeasantBinary(archiveBytes []byte) ([]byte, fs.FileMode, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archiveBytes))
	if err != nil {
		return nil, 0, upgradeActionableError(
			"the Peasant archive is not a readable gzip stream",
			err.Error(),
			"peasant upgrade",
			"after checksum verification",
			"the existing binary was left untouched",
			"download the release archive again or choose another release",
		)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, 0, upgradeActionableError(
				"the Peasant archive could not be read as tar",
				err.Error(),
				"peasant upgrade",
				"after checksum verification",
				"the existing binary was left untouched",
				"download the release archive again or choose another release",
			)
		}
		if header.Typeflag != tar.TypeReg || filepath.Base(header.Name) != "peasant" {
			continue
		}
		limited := io.LimitReader(tr, upgradeBinaryLimit+1)
		binary, err := io.ReadAll(limited)
		if err != nil {
			return nil, 0, upgradeActionableError(
				"the Peasant binary could not be extracted from the archive",
				err.Error(),
				"peasant upgrade",
				"after checksum verification",
				"the existing binary was left untouched",
				"download the release archive again or choose another release",
			)
		}
		if int64(len(binary)) > upgradeBinaryLimit {
			return nil, 0, upgradeActionableError(
				"the Peasant binary in the archive is too large",
				fmt.Sprintf("extracted binary exceeded %d bytes", upgradeBinaryLimit),
				"peasant upgrade",
				"after checksum verification",
				"the existing binary was left untouched",
				"check the release archive and retry after the release is repaired",
			)
		}
		mode := fs.FileMode(header.Mode).Perm()
		if mode == 0 {
			mode = 0o755
		}
		return binary, mode, nil
	}
	return nil, 0, upgradeActionableError(
		"the Peasant archive does not contain a peasant binary",
		"no regular file named peasant was found",
		"peasant upgrade",
		"after checksum verification",
		"the existing binary was left untouched",
		"check the release archive and retry after the release is repaired",
	)
}

func installPeasantBinary(path string, binary []byte, mode fs.FileMode) error {
	if mode.Perm() == 0 {
		mode = 0o755
	}
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".peasant-upgrade-*")
	if err != nil {
		return fmt.Errorf("create temporary binary beside %s: %w", path, err)
	}
	tempPath := temp.Name()
	keepTemp := false
	defer func() {
		if !keepTemp {
			_ = os.Remove(tempPath)
		}
	}()
	if _, err := temp.Write(binary); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write temporary binary %s: %w", tempPath, err)
	}
	if err := temp.Chmod(mode.Perm()); err != nil {
		_ = temp.Close()
		return fmt.Errorf("chmod temporary binary %s: %w", tempPath, err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close temporary binary %s: %w", tempPath, err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("rename verified binary over %s: %w", path, err)
	}
	keepTemp = true
	return nil
}

func upgradeInstallMode(deps upgradeDeps, path string, archiveMode fs.FileMode) fs.FileMode {
	if info, err := deps.Stat(path); err == nil {
		mode := info.Mode().Perm()
		if mode&0o111 != 0 {
			return mode
		}
	}
	if archiveMode.Perm() != 0 {
		return archiveMode.Perm()
	}
	return 0o755
}

func parsePacmanOwner(output string) string {
	marker := " is owned by "
	idx := strings.Index(output, marker)
	if idx < 0 {
		return ""
	}
	rest := strings.TrimSpace(output[idx+len(marker):])
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

func outputMentionsUpgradePath(output string, paths []string) bool {
	for _, path := range paths {
		if path != "" && strings.Contains(output, path) {
			return true
		}
	}
	return false
}

func dedupeUpgradePaths(paths ...string) []string {
	seen := make(map[string]struct{}, len(paths))
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		if strings.TrimSpace(path) == "" {
			continue
		}
		clean := filepath.Clean(path)
		if _, ok := seen[clean]; ok {
			continue
		}
		seen[clean] = struct{}{}
		out = append(out, clean)
	}
	return out
}

func normalizeUpgradeTag(version string) string {
	version = strings.TrimSpace(version)
	if strings.HasPrefix(version, "v") {
		return version
	}
	return "v" + version
}

func cleanUpgradeRequestedVersion(version string) (string, error) {
	version = strings.TrimSpace(version)
	if version == "" || version == "v" {
		return "", upgradeActionableError(
			"the requested Peasant release version is empty",
			"--version must name a release such as 0.5.0-rc2",
			"peasant upgrade --version",
			"before using the version in release URLs or package-manager advice",
			"Peasant cannot select a target release",
			"pass --version 0.5.0-rc2 or omit --version to use the latest stable release",
		)
	}
	for _, r := range version {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_' {
			continue
		}
		return "", upgradeActionableError(
			"the requested Peasant release version contains an unsafe character",
			fmt.Sprintf("--version %q contains %q; only letters, numbers, dots, hyphens, and underscores are allowed", version, r),
			"peasant upgrade --version",
			"before using the version in release URLs or package-manager advice",
			"Peasant will not print or request a release path with shell metacharacters",
			"pass a release version such as 0.5.0-rc2 or v0.5.0-rc2",
		)
	}
	return version, nil
}

func normalizeUpgradeVersion(version string) string {
	version = strings.TrimSpace(version)
	version = strings.TrimPrefix(version, "v")
	return version
}

func isUpgradePrerelease(version string) bool {
	version = normalizeUpgradeVersion(version)
	return version != "" && version != "dev" && strings.Contains(version, "-")
}

type upgradeVersionOrder int

const (
	upgradeVersionBefore upgradeVersionOrder = -1
	upgradeVersionSame   upgradeVersionOrder = 0
	upgradeVersionAfter  upgradeVersionOrder = 1
)

func (o upgradeVersionOrder) String() string {
	switch o {
	case upgradeVersionBefore:
		return "before"
	case upgradeVersionSame:
		return "same"
	case upgradeVersionAfter:
		return "after"
	default:
		return "unknown"
	}
}

type upgradeVersionPhase int

const (
	upgradeVersionPhaseDev upgradeVersionPhase = iota
	upgradeVersionPhaseRC
	upgradeVersionPhaseRelease
)

type parsedUpgradeVersion struct {
	major       int
	minor       int
	patch       int
	phase       upgradeVersionPhase
	rc          int
	dev         bool
	devDistance int
}

func compareUpgradeVersions(current, target string) (upgradeVersionOrder, error) {
	currentVersion, err := parseUpgradeVersion(current, "current")
	if err != nil {
		return upgradeVersionSame, err
	}
	targetVersion, err := parseUpgradeVersion(target, "target")
	if err != nil {
		return upgradeVersionSame, err
	}
	return currentVersion.compare(targetVersion), nil
}

func handleUpgradeVersionOrder(out io.Writer, opts upgradeOptions, targetTag string) (bool, error) {
	order, err := compareUpgradeVersions(opts.CurrentVersion, targetTag)
	if err != nil {
		return false, err
	}
	if order == upgradeVersionSame {
		fmt.Fprintf(out, "Peasant is already at %s; no files were changed.\n", targetTag)
		return true, nil
	}
	if order != upgradeVersionAfter {
		return false, nil
	}
	if !opts.AllowDowngrade {
		return false, downgradeUpgradeError(opts.CurrentVersion, targetTag)
	}
	fmt.Fprintf(out, "downgrade override: current %s sorts after target %s; continuing because --allow-downgrade was set.\n", opts.CurrentVersion, targetTag)
	return false, nil
}

func parseUpgradeVersion(version, label string) (parsedUpgradeVersion, error) {
	original := strings.TrimSpace(version)
	if original == "" {
		return parsedUpgradeVersion{}, unorderedUpgradeVersionError(label, version, "version is empty")
	}
	withoutPrefix := strings.TrimPrefix(original, "v")
	withoutBuild, _, _ := strings.Cut(withoutPrefix, "+")
	core, prerelease, hasPrerelease := strings.Cut(withoutBuild, "-")
	major, minor, patch, err := parseUpgradeVersionCore(core)
	if err != nil {
		return parsedUpgradeVersion{}, unorderedUpgradeVersionError(label, version, err.Error())
	}
	out := parsedUpgradeVersion{major: major, minor: minor, patch: patch, phase: upgradeVersionPhaseRelease}
	if !hasPrerelease {
		return out, nil
	}
	phase, rc, dev, devDistance, err := parseUpgradePrerelease(prerelease)
	if err != nil {
		return parsedUpgradeVersion{}, unorderedUpgradeVersionError(label, version, err.Error())
	}
	out.phase = phase
	out.rc = rc
	out.dev = dev
	out.devDistance = devDistance
	return out, nil
}

func parseUpgradeVersionCore(core string) (int, int, int, error) {
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return 0, 0, 0, fmt.Errorf("version core %q must be MAJOR.MINOR.PATCH", core)
	}
	major, err := parseUpgradeVersionNumber(parts[0], "MAJOR")
	if err != nil {
		return 0, 0, 0, err
	}
	minor, err := parseUpgradeVersionNumber(parts[1], "MINOR")
	if err != nil {
		return 0, 0, 0, err
	}
	patch, err := parseUpgradeVersionNumber(parts[2], "PATCH")
	if err != nil {
		return 0, 0, 0, err
	}
	return major, minor, patch, nil
}

func parseUpgradePrerelease(prerelease string) (upgradeVersionPhase, int, bool, int, error) {
	if strings.HasPrefix(prerelease, "dev.") {
		distancePart := strings.TrimPrefix(prerelease, "dev.")
		distance, err := parseUpgradeVersionNumber(distancePart, "development distance")
		if err != nil {
			return upgradeVersionPhaseDev, 0, false, 0, err
		}
		return upgradeVersionPhaseDev, 0, true, distance, nil
	}
	if !strings.HasPrefix(prerelease, "rc") {
		return upgradeVersionPhaseRelease, 0, false, 0, fmt.Errorf("prerelease %q is not rcN, rc.N, rcN-dev.N, or dev.N", prerelease)
	}
	rcPart := strings.TrimPrefix(prerelease, "rc")
	rcPart = strings.TrimPrefix(rcPart, ".")
	dev := false
	devDistance := 0
	if before, after, ok := strings.Cut(rcPart, "-dev."); ok {
		rcPart = before
		dev = true
		distance, err := parseUpgradeVersionNumber(after, "development distance")
		if err != nil {
			return upgradeVersionPhaseRC, 0, false, 0, err
		}
		devDistance = distance
	}
	if strings.ContainsAny(rcPart, ".-") {
		return upgradeVersionPhaseRC, 0, false, 0, fmt.Errorf("release candidate %q must contain one numeric rc component", prerelease)
	}
	rc, err := parseUpgradeVersionNumber(rcPart, "release candidate")
	if err != nil {
		return upgradeVersionPhaseRC, 0, false, 0, err
	}
	return upgradeVersionPhaseRC, rc, dev, devDistance, nil
}

func parseUpgradeVersionNumber(value, label string) (int, error) {
	if value == "" {
		return 0, fmt.Errorf("%s component is empty", label)
	}
	if len(value) > 1 && strings.HasPrefix(value, "0") {
		return 0, fmt.Errorf("%s component %q has a leading zero", label, value)
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("%s component %q is not numeric", label, value)
		}
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s component %q is not numeric: %w", label, value, err)
	}
	return parsed, nil
}

func (v parsedUpgradeVersion) compare(other parsedUpgradeVersion) upgradeVersionOrder {
	left := []int{v.major, v.minor, v.patch}
	right := []int{other.major, other.minor, other.patch}
	for i := range left {
		if left[i] < right[i] {
			return upgradeVersionBefore
		}
		if left[i] > right[i] {
			return upgradeVersionAfter
		}
	}
	if v.phase < other.phase {
		return upgradeVersionBefore
	}
	if v.phase > other.phase {
		return upgradeVersionAfter
	}
	if v.phase == upgradeVersionPhaseRC {
		if v.rc < other.rc {
			return upgradeVersionBefore
		}
		if v.rc > other.rc {
			return upgradeVersionAfter
		}
		if !v.dev && other.dev {
			return upgradeVersionBefore
		}
		if v.dev && !other.dev {
			return upgradeVersionAfter
		}
	}
	if v.devDistance < other.devDistance {
		return upgradeVersionBefore
	}
	if v.devDistance > other.devDistance {
		return upgradeVersionAfter
	}
	return upgradeVersionSame
}

func unorderedUpgradeVersionError(label, version, why string) error {
	return upgradeActionableError(
		fmt.Sprintf("the %s Peasant version could not be ordered", label),
		fmt.Sprintf("%s version %q is not a supported release, release candidate, or development build: %s", label, version, why),
		"peasant upgrade",
		"before comparing the current and target versions",
		"Peasant cannot prove whether the target is an upgrade or a downgrade, so no files were changed",
		"use a Peasant binary stamped with a release or ordered development version; to install an older release, download it manually from the release page",
	)
}

func downgradeUpgradeError(current, target string) error {
	return upgradeActionableError(
		"the target Peasant release is older than the current binary",
		fmt.Sprintf("current %s sorts after target %s", current, target),
		"peasant upgrade",
		"after selecting the target release and before downloading files",
		"Peasant refused the downgrade and no files were changed",
		fmt.Sprintf("choose a newer Peasant release, keep the current binary, or pass --version %s --allow-downgrade only if you intentionally need to roll back", target),
	)
}

func allowDowngradeWithoutVersionError() error {
	return upgradeActionableError(
		"the downgrade override needs an exact target version",
		"--allow-downgrade was not paired with --version <tag>",
		"peasant upgrade --allow-downgrade",
		"before checking the installation channel or selecting a release",
		"Peasant refused to continue and no files were changed",
		"rerun with --version <tag> --allow-downgrade if you intentionally need to roll back to an older release",
	)
}

func nonInteractiveUpgradeConfirmationError() error {
	return upgradeActionableError(
		"the raw archive replacement plan needs interactive confirmation",
		"standard input is not an interactive terminal and --yes was not set",
		"peasant upgrade",
		"after printing the raw archive replacement plan and before downloading files",
		"Peasant refused to replace the current binary and no files were changed",
		"rerun from an interactive terminal and answer yes, or rerun with --yes only if automation has already reviewed the printed plan",
	)
}

func releaseURL(release upgradeRelease) string {
	if release.HTMLURL != "" {
		return release.HTMLURL
	}
	if release.TagName != "" {
		return upgradeReleasePageURL + "/tag/" + release.TagName
	}
	return upgradeReleasePageURL
}

func upgradeActionableError(what, why, where, when, means, fix string) error {
	return fmt.Errorf("upgrade failed\nwhat: %s\nwhy: %s\nwhere: %s\nwhen: %s\nmeans: %s\nfix: %s", what, why, where, when, means, fix)
}
