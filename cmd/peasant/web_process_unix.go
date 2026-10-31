//go:build unix && !linux

package main

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// isPeasantWebServer asks ps for the process's executable and command line
// (macOS and the BSDs have no /proc). -ww stops ps truncating the line to the
// terminal width. ps exits 1 with no output when the PID does not exist; that
// is not a match. Any other failure is a probe error.
func isPeasantWebServer(pid, port int) (bool, error) {
	// comm and args are read separately: comm is the executable path, which
	// may contain spaces, so it cannot be split off a joined line.
	comm, err := exec.Command("ps", "-ww", "-o", "comm=", "-p", strconv.Itoa(pid)).Output()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && len(strings.TrimSpace(string(comm))) == 0 {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%w %d: %v", errProcessProbe, pid, err)
	}
	args, err := exec.Command("ps", "-ww", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
	if errors.As(err, &exitErr) && len(strings.TrimSpace(string(args))) == 0 {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%w %d: %v", errProcessProbe, pid, err)
	}
	// args repeats argv[0] and the arguments; the server's own arguments
	// contain no spaces.
	executable := strings.TrimSpace(string(comm))
	if !sameExecutablePath(executable) {
		return false, nil
	}
	line := strings.TrimSpace(string(args))
	if !strings.HasPrefix(line, executable+" ") {
		return false, nil
	}
	argv := append([]string{executable}, strings.Fields(strings.TrimPrefix(line, executable+" "))...)
	return isWebServerCommandLine(argv, port), nil
}
