//go:build unix

package main

import "syscall"

// backgroundProcAttr returns the SysProcAttr that detaches the backgrounded
// `web start` process from the controlling terminal.
func backgroundProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Setsid: true, // detach from terminal
	}
}
