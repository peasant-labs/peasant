//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
)

const (
	releaseE2EFullStack = releaseE2EMode("full-stack")
)

type releaseE2EMode string

type releaseDistro struct {
	name            string
	mode            releaseE2EMode
	image           string
	artifactPattern string
	install         func(t *testing.T, container, artifact string)
}

func TestReleasePerDistro(t *testing.T) {
	assertReleaseE2EWorkflowContract(t)
	requirePodman(t)

	distDir := releaseE2EDistDir(t)
	wrapper := releaseE2EWrapperPath(t)
	bins := resolveVillageBinaries(t)

	setenv(t, envDatabaseURL, "")
	setenv(t, envS3Endpoint, "")
	setenv(t, envS3Bucket, "")
	setenv(t, envVillageURL, "")

	xdgRoot := filepath.Join(t.TempDir(), "xdg")
	for _, dir := range []string{"config", "data", "state"} {
		if err := os.MkdirAll(filepath.Join(xdgRoot, dir), 0o755); err != nil {
			t.Fatalf("release-e2e: mkdir %s: %v", dir, err)
		}
	}
	setenv(t, defaults.EnvXDGStateHome, filepath.Join(xdgRoot, "state"))

	stack := provisionHarnessStack(t, bins)
	if stack.external {
		t.Fatalf("release-e2e: provision-once stack unexpectedly used external mode; unset DATABASE_URL/S3_ENDPOINT/S3_BUCKET/VILLAGE_URL before starting the release driver")
	}
	seedConfigHome := filepath.Join(xdgRoot, "seed-config")
	if err := os.MkdirAll(seedConfigHome, 0o755); err != nil {
		t.Fatalf("release-e2e: mkdir seed config home: %v", err)
	}
	_ = mintDemoCredentials(t, bins.setupDemo, stack.dsn, stack.villageURL, seedConfigHome)
	assertSeededBaselineBeforePush(t, harnessOptions{assert: true}, stack)

	distros := releaseE2EDistros()
	for i, distro := range distros {
		if distro.mode == "" {
			t.Fatalf("release-e2e: distro %s has no mode declaration; set mode explicitly so fallback cannot be silent", distro.name)
		}
		if distro.mode != releaseE2EFullStack {
			t.Fatalf("release-e2e: distro %s mode %q; this driver only accepts full-stack coverage", distro.name, distro.mode)
		}

		passed := t.Run(distro.name, func(t *testing.T) {
			artifact := mustFindReleaseArtifact(t, distDir, distro.artifactPattern)
			container := startReleaseDistroContainer(t, distro, xdgRoot, distDir, peasantRepoRootForReleaseE2E(t))
			distro.install(t, container, artifact)
			runPodman(t, "exec", "-i", container, "/usr/bin/peasant", "version")

			setExternalStackEnv(t, stack)
			assertExternalStackEngaged(t, stack)
			setenv(t, envPodmanContainer, container)
			setenv(t, envPeasantBin, wrapper)
			t.Logf("%s: %s asserted for %s", distro.name, externalStackEngagedMarker, stack.villageURL)

			runSkipGateHarness(t, harnessOptions{assert: true})
			assertPushReachedWarmStack(t, stack, distro.name)
		})
		if !passed {
			t.Fatalf("release-e2e: distro %s failed; aborting before warm-stack refresh so the primary failure remains visible", distro.name)
		}

		if i < len(distros)-1 {
			_ = stack.refresh(t, bins, seedConfigHome)
		}
	}
}

func releaseE2EDistros() []releaseDistro {
	return []releaseDistro{
		{
			name:            "ubuntu-22.04",
			mode:            releaseE2EFullStack,
			image:           "docker.io/library/ubuntu:22.04",
			artifactPattern: "peasant_*_linux_amd64.deb",
			install:         installDeb,
		},
		{
			name:            "ubuntu-24.04",
			mode:            releaseE2EFullStack,
			image:           "docker.io/library/ubuntu:24.04",
			artifactPattern: "peasant_*_linux_amd64.deb",
			install:         installDeb,
		},
		{
			name:            "fedora-latest",
			mode:            releaseE2EFullStack,
			image:           "docker.io/library/fedora:latest",
			artifactPattern: "peasant_*_linux_amd64.rpm",
			install:         installFedoraRPM,
		},
		{
			name:            "opensuse-leap",
			mode:            releaseE2EFullStack,
			image:           "docker.io/opensuse/leap:latest",
			artifactPattern: "peasant_*_linux_amd64.rpm",
			install:         installOpenSUSERPM,
		},
		{
			name:            "archlinux-base-devel",
			mode:            releaseE2EFullStack,
			image:           "docker.io/library/archlinux:base-devel",
			artifactPattern: "peasant_*_linux_amd64.tar.gz",
			install:         installTarBinary,
		},
	}
}

func releaseE2EDistDir(t *testing.T) string {
	t.Helper()
	dist := strings.TrimSpace(getenv(envReleaseE2EDist))
	if dist == "" {
		dist = filepath.Join(peasantRepoRootForReleaseE2E(t), "dist")
	}
	if fi, err := os.Stat(dist); err != nil || !fi.IsDir() {
		t.Skipf("release-e2e: dist artifacts not found at %s; run goreleaser snapshot first or set RELEASE_E2E_DIST", dist)
	}
	return dist
}

func releaseE2EWrapperPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(peasantRepoRootForReleaseE2E(t), "scripts", "release-e2e", "podman-peasant")
	if fi, err := os.Stat(path); err != nil || fi.IsDir() {
		t.Fatalf("release-e2e: PEASANT_BIN wrapper missing at %s: %v", path, err)
	}
	return path
}

func peasantRepoRootForReleaseE2E(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("release-e2e: getwd: %v", err)
	}
	root := peasantRepoRoot(wd)
	if root == "" {
		t.Fatalf("release-e2e: cannot locate peasant repository root from %s", wd)
	}
	return root
}

func mustFindReleaseArtifact(t *testing.T, distDir, pattern string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(distDir, pattern))
	if err != nil {
		t.Fatalf("release-e2e: invalid artifact glob %q: %v", pattern, err)
	}
	if len(matches) != 1 {
		t.Fatalf("release-e2e: artifact glob %s matched %d files under %s, want exactly 1: %v", pattern, len(matches), distDir, matches)
	}
	return matches[0]
}

func startReleaseDistroContainer(t *testing.T, distro releaseDistro, xdgRoot, distDir, repoRoot string) string {
	t.Helper()
	name := uniqueName("release-" + strings.NewReplacer(".", "-", "/", "-").Replace(distro.name))
	args := []string{
		"run", "-d",
		"--name", name,
		"--network", "host",
		"-v", xdgRoot + ":" + xdgRoot + ":rw",
		"-v", distDir + ":" + distDir + ":ro",
		"-v", repoRoot + ":" + repoRoot + ":ro",
		distro.image,
		"sleep", "infinity",
	}
	out, err := exec.Command("podman", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("release-e2e: start distro container %s (%s): %v\n%s", distro.name, distro.image, err, out)
	}
	t.Cleanup(func() { _ = exec.Command("podman", "rm", "-fv", name).Run() })
	return name
}

func installDeb(t *testing.T, container, artifact string) {
	t.Helper()
	runPodman(t, "exec", "-i", container, "dpkg", "-i", artifact)
}

func installFedoraRPM(t *testing.T, container, artifact string) {
	t.Helper()
	runPodman(t, "exec", "-i", container, "dnf", "install", "-y", artifact)
}

func installOpenSUSERPM(t *testing.T, container, artifact string) {
	t.Helper()
	runPodman(t, "exec", "-i", container, "zypper", "--non-interactive", "modifyrepo", "--disable", "--all")
	runPodman(t, "exec", "-i", container, "zypper", "--non-interactive", "--no-refresh", "install", "--allow-unsigned-rpm", artifact)
}

func installTarBinary(t *testing.T, container, artifact string) {
	t.Helper()
	runPodman(t, "exec", "-i", container, "sh", "-c",
		`tmp=$(mktemp -d) && tar -xzf "$1" -C "$tmp" && install -Dm755 "$tmp/peasant" /usr/bin/peasant`,
		"sh", artifact)
}

func runPodman(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("podman", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("release-e2e: podman %s failed\nwhat: podman command returned non-zero\nwhy: %v\nwhere: internal/e2e/release_perdistro_test.go\nwhen: running installed-package e2e orchestration\nmeans: the distro did not prove full-stack installed-binary coverage\nfix: inspect the podman output and the release artifact for this distro\noutput:\n%s",
			strings.Join(args, " "), err, out)
	}
	return string(out)
}

func setExternalStackEnv(t *testing.T, stack harnessStack) {
	t.Helper()
	setenv(t, envDatabaseURL, stack.dsn)
	setenv(t, envS3Endpoint, stack.minioEndpoint)
	setenv(t, envS3Bucket, stack.bucket)
	setenv(t, envVillageURL, stack.villageURL)
}

func assertExternalStackEngaged(t *testing.T, stack harnessStack) {
	t.Helper()
	cfg, err := validateExternalStackConfig(externalStackConfig{
		dsn:           stack.dsn,
		minioEndpoint: stack.minioEndpoint,
		bucket:        stack.bucket,
		villageURL:    stack.villageURL,
	})
	if err != nil {
		t.Fatalf("release-e2e: external-stack config rejected before distro run: %v", err)
	}
	if !cfg.engaged {
		t.Fatalf("release-e2e: %s marker cannot be asserted because injected stack config did not engage", externalStackEngagedMarker)
	}
}

func assertPushReachedWarmStack(t *testing.T, stack harnessStack, distro string) {
	t.Helper()
	transcripts := villageTableCount(t, stack.db, "transcripts")
	if transcripts < ExpectedPushTranscriptCount {
		t.Fatalf("release-e2e: %s push#1 did not reach the warm village stack: village transcripts = %d, want at least %d", distro, transcripts, ExpectedPushTranscriptCount)
	}
	objects := transcriptBucketObjectCount(t, stack.minioEndpoint, stack.bucket)
	if objects < ExpectedPushTranscriptCount {
		t.Fatalf("release-e2e: %s push#1 did not write transcripts to the warm S3 bucket: objects = %d, want at least %d", distro, objects, ExpectedPushTranscriptCount)
	}
}
