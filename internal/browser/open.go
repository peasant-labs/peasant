// Package browser opens URLs in the user's default web browser.
//
// Open walks a platform-specific, ordered chain of launchers and returns nil as
// soon as one starts successfully. When every launcher fails it returns a single
// actionable error that names each attempt and tells the caller how to recover.
//
// All OS interaction (GOOS, environment, PATH lookup, process start) is injected
// through the unexported opener fields, so the launch chain can be exercised in
// tests without spawning a real browser.
package browser

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// Open launches url in the user's default web browser.
//
// It returns nil as soon as one launcher starts successfully, or an actionable
// error enumerating every attempted launcher (and how to fix the failure) when
// all of them fail. Callers MUST surface the returned error — opening the
// browser is best-effort, but a silent failure leaves the user staring at a
// window that never appears.
func Open(url string) error {
	return defaultOpener.open(url)
}

// launcher is one candidate command in a platform's launch chain.
type launcher struct {
	label string   // human-facing name used in error messages, e.g. `$BROWSER (firefox)`
	bin   string   // executable resolved via lookPath
	args  []string // arguments passed to the resolved executable
}

// opener resolves and starts browser launchers. Every OS touchpoint is a field
// so tests can drive the chain deterministically.
type opener struct {
	goos     string
	getenv   func(string) string
	lookPath func(string) (string, error)
	start    func(name string, args ...string) error
}

// defaultOpener wires opener to the real OS for production use.
var defaultOpener = &opener{
	goos:     runtime.GOOS,
	getenv:   os.Getenv,
	lookPath: exec.LookPath,
	start: func(name string, args ...string) error {
		// Fire-and-forget: the browser process is detached from peasant.
		return exec.Command(name, args...).Start()
	},
}

// candidates returns the ordered launch chain for the opener's platform.
//
//   - linux / *bsd / other (the default arm, which also covers WSL): the
//     freedesktop $BROWSER entries (if any) first, then xdg-open, then wslview.
//   - darwin: open
//   - windows: cmd /c start
func (o *opener) candidates(url string) []launcher {
	switch o.goos {
	case "darwin":
		return []launcher{{label: "open", bin: "open", args: []string{url}}}
	case "windows":
		return []launcher{{label: "cmd /c start", bin: "cmd", args: []string{"/c", "start", url}}}
	default:
		cands := browserEnvLaunchers(o.getenv("BROWSER"), url)
		cands = append(cands,
			launcher{label: "xdg-open", bin: "xdg-open", args: []string{url}},
			launcher{label: "wslview", bin: "wslview", args: []string{url}},
		)
		return cands
	}
}

// browserEnvLaunchers parses the freedesktop $BROWSER variable into launchers.
//
// $BROWSER is a colon-separated list of commands tried in order. Within a
// command, a "%s" token is replaced by the URL; a command with no "%s" gets the
// URL appended as a trailing argument. Empty entries are skipped.
func browserEnvLaunchers(env, url string) []launcher {
	env = strings.TrimSpace(env)
	if env == "" {
		return nil
	}
	var out []launcher
	for entry := range strings.SplitSeq(env, ":") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		fields := strings.Fields(entry)
		bin := fields[0]
		var args []string
		substituted := false
		for _, f := range fields[1:] {
			if strings.Contains(f, "%s") {
				args = append(args, strings.ReplaceAll(f, "%s", url))
				substituted = true
			} else {
				args = append(args, f)
			}
		}
		if !substituted {
			args = append(args, url)
		}
		out = append(out, launcher{
			label: fmt.Sprintf("$BROWSER (%s)", bin),
			bin:   bin,
			args:  args,
		})
	}
	return out
}

// open tries each candidate launcher in order, returning nil on the first
// success and an actionable, attempt-by-attempt error if all of them fail.
func (o *opener) open(url string) error {
	// candidates() always returns at least one launcher on every platform (the
	// default arm appends xdg-open + wslview), so a non-empty chain is an
	// invariant here; the all-attempts-failed error below is the only failure mode.
	cands := o.candidates(url)

	attempts := make([]string, 0, len(cands))
	for _, c := range cands {
		path, err := o.lookPath(c.bin)
		if err != nil {
			attempts = append(attempts, fmt.Sprintf("%s — executable %q not found in PATH", c.label, c.bin))
			continue
		}
		if err := o.start(path, c.args...); err != nil {
			attempts = append(attempts, fmt.Sprintf("%s — failed to start %q: %v", c.label, path, err))
			continue
		}
		return nil
	}

	return fmt.Errorf(
		"failed to open browser for %q: all %d launch method(s) failed on GOOS=%s:\n  - %s\n"+
			"to fix: install a browser launcher (xdg-utils provides xdg-open; wslu provides wslview on WSL) "+
			"or set $BROWSER to a working browser command",
		url, len(cands), o.goos, strings.Join(attempts, "\n  - "))
}
