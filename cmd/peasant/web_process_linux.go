//go:build linux

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// isPeasantWebServer reads the process's executable and command line from
// /proc. A process that is gone is not a match; any other read failure is a
// probe error, so the caller keeps the PID file.
func isPeasantWebServer(pid, port int) (bool, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%w %d: %v", errProcessProbe, pid, err)
	}
	var args []string
	for _, field := range bytes.Split(bytes.TrimRight(raw, "\x00"), []byte{0}) {
		args = append(args, string(field))
	}
	if !isWebServerCommandLine(args, port) {
		return false, nil
	}
	exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return false, fmt.Errorf("%w %d: %v", errProcessProbe, pid, err)
	}
	return sameExecutablePath(exe), nil
}
