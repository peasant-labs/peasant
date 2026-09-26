//go:build windows

package main

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// backgroundProcAttr returns the SysProcAttr that detaches the backgrounded
// `web start` process from the controlling console. Windows has no Setsid
// field; CREATE_NEW_PROCESS_GROUP puts the child in its own process group so
// it does not receive the parent console's Ctrl+C, and DETACHED_PROCESS gives
// it no console at all.
func backgroundProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS,
	}
}
