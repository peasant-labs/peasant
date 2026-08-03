//go:build e2e && linux

package e2e

import (
	"os/exec"
	"syscall"
)

func villageGroupKillTarget(pid int) (pgid int, ok bool) {
	if pid <= 0 {
		return 0, false
	}
	return pid, true
}

func stopVillageProcess(cmd *exec.Cmd) {
	if pgid, ok := villageGroupKillTarget(cmd.Process.Pid); ok {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	} else {
		_ = cmd.Process.Kill()
	}
	_, _ = cmd.Process.Wait()
}
