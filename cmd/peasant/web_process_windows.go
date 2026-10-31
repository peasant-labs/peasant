//go:build windows

package main

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"golang.org/x/sys/windows"
)

// isPeasantWebServer checks that the PID runs this peasant executable and
// listens on the port. Windows offers no portable way to read another
// process's command line, so the port is confirmed from the TCP table
// (`netstat -ano -p tcp`) instead.
func isPeasantWebServer(pid, port int) (bool, error) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		if err == windows.ERROR_INVALID_PARAMETER {
			return false, nil
		}
		return false, fmt.Errorf("%w %d: %v", errProcessProbe, pid, err)
	}
	defer windows.CloseHandle(handle)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(handle, 0, &buf[0], &size); err != nil {
		return false, fmt.Errorf("%w %d: %v", errProcessProbe, pid, err)
	}
	if !sameExecutablePath(windows.UTF16ToString(buf[:size])) {
		return false, nil
	}
	out, err := exec.Command("netstat", "-ano", "-p", "tcp").Output()
	if err != nil {
		return false, fmt.Errorf("%w %d: netstat: %v", errProcessProbe, pid, err)
	}
	return listensOn(string(out), pid, port), nil
}

// listensOn reports whether netstat output has a LISTENING row on port owned
// by pid.
func listensOn(netstat string, pid, port int) bool {
	suffix, owner := ":"+strconv.Itoa(port), strconv.Itoa(pid)
	for _, line := range strings.Split(netstat, "\n") {
		f := strings.Fields(line)
		if len(f) != 5 {
			continue
		}
		if strings.HasSuffix(f[1], suffix) && strings.EqualFold(f[3], "LISTENING") && f[4] == owner {
			return true
		}
	}
	return false
}
