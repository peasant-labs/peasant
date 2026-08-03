//go:build e2e && !linux

package e2e

import "os/exec"

func lockVillageLaunchThread() {}

func setVillageProcDeath(cmd *exec.Cmd) {}
