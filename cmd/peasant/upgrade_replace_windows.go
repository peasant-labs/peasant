//go:build windows

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// upgradeSidecarSuffix names the executable a Windows upgrade displaces. The
// name is deterministic so a later upgrade can find and sweep it.
const upgradeSidecarSuffix = ".old"

// replaceExecutable installs the verified binary written at tempPath as path.
//
// Windows refuses to replace the image of a running process: renaming over a
// live .exe fails with "Access is denied". It does allow that image to be
// RENAMED, which frees the original path without disturbing the process still
// executing from it. So the upgrade moves the current executable aside and
// writes the new binary into the path it vacated.
//
// The displaced file cannot be deleted while the process running it is alive,
// so a failed cleanup is not an upgrade failure — the sidecar is swept by the
// next upgrade instead. If the new binary cannot be put in place, the displaced
// executable is renamed back so the install stays all-or-nothing.
func replaceExecutable(tempPath, path string) error {
	sidecar := path + upgradeSidecarSuffix
	// Sweep a sidecar an earlier upgrade could not remove. The process holding
	// it has since exited, so this normally succeeds; when it does not, the
	// rename below reports the real problem.
	_ = os.Remove(sidecar)

	displaced := false
	if err := os.Rename(path, sidecar); err == nil {
		displaced = true
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("move the running executable %s aside to %s: %w", path, sidecar, err)
	}

	if err := os.Rename(tempPath, path); err != nil {
		if displaced {
			if restoreErr := os.Rename(sidecar, path); restoreErr != nil {
				return fmt.Errorf(
					"install the verified binary as %s: %w; the previous executable is still at %s and has to be renamed back by hand: %v",
					path, err, sidecar, restoreErr,
				)
			}
		}
		return fmt.Errorf("rename verified binary over %s: %w", path, err)
	}

	// Expected to fail whenever this process is still running from the
	// displaced image, which is the normal case for a self-upgrade.
	_ = os.Remove(sidecar)
	return nil
}

// sweepUpgradeSidecar removes a sidecar an earlier upgrade could not delete, so
// the displaced executable does not linger until the next upgrade. Best-effort:
// the running process cannot remove its own displaced image, and any error is
// ignored. It runs once per invocation, before any command does work.
func sweepUpgradeSidecar() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	sweepUpgradeSidecarAt(exe)
}

// sweepUpgradeSidecarAt removes the sidecar beside exe, best-effort.
func sweepUpgradeSidecarAt(exe string) {
	_ = os.Remove(exe + upgradeSidecarSuffix)
}
