//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// These exercise the Windows-only displace-and-install path. They cannot hold a
// real running image, so they pin the parts that do not need one: the new bytes
// land at the target, no sidecar is left behind, a sidecar stranded by an
// earlier upgrade is swept, and a missing target is not an error.

func TestReplaceExecutableInstallsAndLeavesNoSidecar(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "peasant.exe")
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	temp := filepath.Join(dir, ".peasant-upgrade-new")
	if err := os.WriteFile(temp, []byte("new"), 0o644); err != nil {
		t.Fatalf("seed replacement: %v", err)
	}
	if err := replaceExecutable(temp, target); err != nil {
		t.Fatalf("replaceExecutable: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(got) != "new" {
		t.Fatalf("target = %q, want %q", got, "new")
	}
	if _, err := os.Stat(target + upgradeSidecarSuffix); !os.IsNotExist(err) {
		t.Fatalf("sidecar should not survive an upgrade whose process is not running it; stat error = %v", err)
	}
}

func TestReplaceExecutableSweepsSidecarStrandedByEarlierUpgrade(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "peasant.exe")
	stranded := target + upgradeSidecarSuffix
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	if err := os.WriteFile(stranded, []byte("stranded by an earlier upgrade"), 0o644); err != nil {
		t.Fatalf("seed stranded sidecar: %v", err)
	}
	temp := filepath.Join(dir, ".peasant-upgrade-new")
	if err := os.WriteFile(temp, []byte("new"), 0o644); err != nil {
		t.Fatalf("seed replacement: %v", err)
	}
	if err := replaceExecutable(temp, target); err != nil {
		t.Fatalf("replaceExecutable: %v", err)
	}
	if _, err := os.Stat(stranded); !os.IsNotExist(err) {
		t.Fatalf("a stranded sidecar must be swept rather than accumulate; stat error = %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(got) != "new" {
		t.Fatalf("target = %q, want %q", got, "new")
	}
}

func TestReplaceExecutableInstallsWhenTargetIsAbsent(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "peasant.exe")
	temp := filepath.Join(dir, ".peasant-upgrade-new")
	if err := os.WriteFile(temp, []byte("new"), 0o644); err != nil {
		t.Fatalf("seed replacement: %v", err)
	}
	if err := replaceExecutable(temp, target); err != nil {
		t.Fatalf("replaceExecutable with no existing target: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(got) != "new" {
		t.Fatalf("target = %q, want %q", got, "new")
	}
}

func TestSweepUpgradeSidecarRemovesStrandedSidecar(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "peasant.exe")
	sidecar := exe + upgradeSidecarSuffix
	if err := os.WriteFile(sidecar, []byte("stranded by an earlier upgrade"), 0o644); err != nil {
		t.Fatalf("seed stranded sidecar: %v", err)
	}
	sweepUpgradeSidecarAt(exe)
	if _, err := os.Stat(sidecar); !os.IsNotExist(err) {
		t.Fatalf("sweepUpgradeSidecarAt left %s; stat error = %v", sidecar, err)
	}
}

func TestSweepUpgradeSidecarIsANoOpWhenNoSidecarExists(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "peasant.exe")
	if err := os.WriteFile(exe, []byte("current"), 0o644); err != nil {
		t.Fatalf("seed executable: %v", err)
	}
	sweepUpgradeSidecarAt(exe)
	got, err := os.ReadFile(exe)
	if err != nil {
		t.Fatalf("read executable: %v", err)
	}
	if string(got) != "current" {
		t.Fatalf("executable = %q, want %q", got, "current")
	}
}
