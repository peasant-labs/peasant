package browser

import (
	"errors"
	"runtime"
	"strings"
	"testing"
)

// startRecord captures one start() invocation so tests can assert on the exact
// command the opener would have run.
type startRecord struct {
	name string
	args []string
}

// newProbe builds an opener whose OS touchpoints are fully injected:
//   - present: set of executables that lookPath resolves (value = resolved path)
//   - env: $BROWSER value
//   - startErr: error returned by start for a given resolved path (nil = success)
//
// It returns the opener plus a pointer to the slice of recorded start calls.
func newProbe(goos string, env string, present map[string]string, startErr map[string]error) (*opener, *[]startRecord) {
	var calls []startRecord
	o := &opener{
		goos:   goos,
		getenv: func(k string) string { return map[string]string{"BROWSER": env}[k] },
		lookPath: func(bin string) (string, error) {
			if p, ok := present[bin]; ok {
				return p, nil
			}
			return "", errors.New("not found")
		},
		start: func(name string, args ...string) error {
			calls = append(calls, startRecord{name: name, args: args})
			if startErr != nil {
				if err, ok := startErr[name]; ok {
					return err
				}
			}
			return nil
		},
	}
	return o, &calls
}

func TestOpen_PlatformChains(t *testing.T) {
	tests := []struct {
		name      string
		goos      string
		env       string
		present   map[string]string
		startErr  map[string]error
		wantErr   bool
		wantStart *startRecord // expected single successful launch (nil ⇒ none expected)
		errSubstr []string     // substrings the failure error must contain
	}{
		{
			name:      "linux xdg-open success",
			goos:      "linux",
			present:   map[string]string{"xdg-open": "/usr/bin/xdg-open"},
			wantStart: &startRecord{name: "/usr/bin/xdg-open", args: []string{"https://x.test"}},
		},
		{
			name:      "linux falls back to wslview when xdg-open missing",
			goos:      "linux",
			present:   map[string]string{"wslview": "/usr/bin/wslview"},
			wantStart: &startRecord{name: "/usr/bin/wslview", args: []string{"https://x.test"}},
		},
		{
			name:      "BROWSER env takes precedence over xdg-open",
			goos:      "linux",
			env:       "firefox",
			present:   map[string]string{"firefox": "/usr/bin/firefox", "xdg-open": "/usr/bin/xdg-open"},
			wantStart: &startRecord{name: "/usr/bin/firefox", args: []string{"https://x.test"}},
		},
		{
			name:      "BROWSER env with %s placeholder substitutes URL",
			goos:      "linux",
			env:       "chromium --app=%s",
			present:   map[string]string{"chromium": "/usr/bin/chromium"},
			wantStart: &startRecord{name: "/usr/bin/chromium", args: []string{"--app=https://x.test"}},
		},
		{
			name:      "BROWSER colon list skips missing then uses second",
			goos:      "linux",
			env:       "missingbrowser:firefox",
			present:   map[string]string{"firefox": "/usr/bin/firefox"},
			wantStart: &startRecord{name: "/usr/bin/firefox", args: []string{"https://x.test"}},
		},
		{
			name:      "darwin uses open",
			goos:      "darwin",
			present:   map[string]string{"open": "/usr/bin/open"},
			wantStart: &startRecord{name: "/usr/bin/open", args: []string{"https://x.test"}},
		},
		{
			name:      "windows uses cmd start",
			goos:      "windows",
			present:   map[string]string{"cmd": `C:\Windows\System32\cmd.exe`},
			wantStart: &startRecord{name: `C:\Windows\System32\cmd.exe`, args: []string{"/c", "start", "https://x.test"}},
		},
		{
			name:      "linux all launchers missing returns actionable error",
			goos:      "linux",
			present:   map[string]string{},
			wantErr:   true,
			errSubstr: []string{"failed to open browser", "xdg-open", "wslview", "to fix"},
		},
		{
			name:      "start failure falls through to next launcher",
			goos:      "linux",
			present:   map[string]string{"xdg-open": "/usr/bin/xdg-open", "wslview": "/usr/bin/wslview"},
			startErr:  map[string]error{"/usr/bin/xdg-open": errors.New("exec format error")},
			wantStart: &startRecord{name: "/usr/bin/wslview", args: []string{"https://x.test"}},
		},
		{
			name:      "start failure on every launcher reports each attempt",
			goos:      "linux",
			present:   map[string]string{"xdg-open": "/usr/bin/xdg-open", "wslview": "/usr/bin/wslview"},
			startErr:  map[string]error{"/usr/bin/xdg-open": errors.New("boom1"), "/usr/bin/wslview": errors.New("boom2")},
			wantErr:   true,
			errSubstr: []string{"boom1", "boom2", "all 2 launch method(s) failed"},
		},
	}

	const url = "https://x.test"
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o, calls := newProbe(tt.goos, tt.env, tt.present, tt.startErr)
			err := o.open(url)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (calls=%v)", *calls)
				}
				for _, sub := range tt.errSubstr {
					if !strings.Contains(err.Error(), sub) {
						t.Errorf("error %q missing substring %q", err.Error(), sub)
					}
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantStart == nil {
				return
			}
			// The successful launch is always the last recorded start call.
			if len(*calls) == 0 {
				t.Fatalf("expected a start call, got none")
			}
			got := (*calls)[len(*calls)-1]
			if got.name != tt.wantStart.name {
				t.Errorf("start name = %q, want %q", got.name, tt.wantStart.name)
			}
			if strings.Join(got.args, "\x00") != strings.Join(tt.wantStart.args, "\x00") {
				t.Errorf("start args = %v, want %v", got.args, tt.wantStart.args)
			}
		})
	}
}

// TestOpen_NoWorkingLauncher covers a host where the default arm's launchers
// (xdg-open, wslview) are all absent: the chain is non-empty but nothing starts,
// so open() must return the actionable all-attempts-failed error naming the URL.
func TestOpen_NoWorkingLauncher(t *testing.T) {
	o := &opener{
		goos:     "linux",
		getenv:   func(string) string { return "" },
		lookPath: func(string) (string, error) { return "", errors.New("nope") },
		start:    func(string, ...string) error { return nil },
	}
	err := o.open("https://x.test")
	if err == nil {
		t.Fatal("expected error on host with no working launcher")
	}
	if !strings.Contains(err.Error(), "https://x.test") {
		t.Errorf("error should name the URL: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "to fix") {
		t.Errorf("error should be actionable: %q", err.Error())
	}
}

// TestPackageOpen_Wiring asserts the exported Open delegates to a default opener
// wired to the real OS, WITHOUT launching anything (no side effects): every
// touchpoint is non-nil and goos mirrors the runtime. The DI table above is the
// behavioral suite; this only guards the production wiring.
func TestPackageOpen_Wiring(t *testing.T) {
	if defaultOpener == nil {
		t.Fatal("defaultOpener must be initialized")
	}
	if defaultOpener.goos != runtime.GOOS {
		t.Errorf("defaultOpener.goos = %q, want runtime.GOOS %q", defaultOpener.goos, runtime.GOOS)
	}
	if defaultOpener.getenv == nil || defaultOpener.lookPath == nil || defaultOpener.start == nil {
		t.Error("defaultOpener must wire getenv, lookPath, and start to real OS functions")
	}
}
