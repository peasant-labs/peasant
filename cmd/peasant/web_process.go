package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// errProcessProbe means the platform probe could not tell what a PID is (ps
// failed, the process table could not be read). `web stop` then keeps the PID
// file: deleting it could orphan a server that is really running.
var errProcessProbe = errors.New("cannot inspect the process")

// isWebServerCommandLine reports whether args is the command line `web start`
// gives the detached server it spawns (`<exe> web start --foreground --port N
// …`, see webServerSpawn.args) for this port. The executable itself is checked
// separately (sameExecutablePath), so an unrelated program started with the same
// trailing arguments is not a match.
func isWebServerCommandLine(args []string, port int) bool {
	if len(args) < 4 {
		return false
	}
	if args[1] != "web" || args[2] != "start" || args[3] != "--foreground" {
		return false
	}
	want := strconv.Itoa(port)
	sawPort := false
	for i := 4; i < len(args); i++ {
		if args[i] == "--port" {
			if sawPort || i+1 >= len(args) || args[i+1] != want {
				return false
			}
			sawPort = true
			i++
		} else if strings.HasPrefix(args[i], "--port=") {
			if sawPort || strings.TrimPrefix(args[i], "--port=") != want {
				return false
			}
			sawPort = true
		} else if strings.HasPrefix(args[i], "--foreground=") {
			return false
		}
	}
	return sawPort
}

// sameExecutablePath reports whether path is exactly the executable this
// process runs, after resolving symlinks. It is for probes that read the
// exact path (/proc/<pid>/exe): there a file-name match is not enough, since
// another program copied under the same name would pass. An upgrade replaces
// the file in place, so Linux's " (deleted)" suffix still names the same path.
func sameExecutablePath(path string) bool {
	self, err := os.Executable()
	path = strings.TrimSuffix(path, " (deleted)")
	if err != nil || !filepath.IsAbs(path) {
		return false
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(path, self)
	}
	return path == self
}
