//go:build e2e && !linux

package e2e

import "os/exec"

func stopVillageProcess(cmd *exec.Cmd) {
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
}
