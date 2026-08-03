package defaults

import (
	"runtime/debug"
	"testing"
)

// TestResolveVersion covers the version-selection logic, in particular the
// `go install module@version` fallback branch: when the ldflags-injected version
// is the "dev" placeholder,
// resolveVersion must use debug.BuildInfo.Main.Version when it carries a real
// value, and otherwise keep "dev".
func TestResolveVersion(t *testing.T) {
	// buildInfo returns a fake readBuildInfo that reports mainVersion with ok=true.
	buildInfo := func(mainVersion string) func() (*debug.BuildInfo, bool) {
		return func() (*debug.BuildInfo, bool) {
			return &debug.BuildInfo{Main: debug.Module{Version: mainVersion}}, true
		}
	}
	noBuildInfo := func() (*debug.BuildInfo, bool) { return nil, false }
	nilBuildInfo := func() (*debug.BuildInfo, bool) { return nil, true }

	tests := []struct {
		name          string
		ldflags       string
		readBuildInfo func() (*debug.BuildInfo, bool)
		want          AppVersion
	}{
		{
			name:          "ldflags real version wins over build info",
			ldflags:       "0.1.0",
			readBuildInfo: buildInfo("v9.9.9"),
			want:          AppVersion("0.1.0"),
		},
		{
			name:          "dev placeholder falls back to module version (go install @version)",
			ldflags:       string(devVersion),
			readBuildInfo: buildInfo("v1.2.3"),
			want:          AppVersion("v1.2.3"),
		},
		{
			name:          "dev placeholder + (devel) sentinel stays dev",
			ldflags:       string(devVersion),
			readBuildInfo: buildInfo("(devel)"),
			want:          devVersion,
		},
		{
			name:          "dev placeholder + empty module version stays dev",
			ldflags:       string(devVersion),
			readBuildInfo: buildInfo(""),
			want:          devVersion,
		},
		{
			name:          "dev placeholder + no build info stays dev",
			ldflags:       string(devVersion),
			readBuildInfo: noBuildInfo,
			want:          devVersion,
		},
		{
			name:          "dev placeholder + nil build info (ok=true) stays dev",
			ldflags:       string(devVersion),
			readBuildInfo: nilBuildInfo,
			want:          devVersion,
		},
		{
			name:          "empty ldflags also falls back to module version",
			ldflags:       "",
			readBuildInfo: buildInfo("v2.0.0"),
			want:          AppVersion("v2.0.0"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveVersion(tc.ldflags, tc.readBuildInfo)
			if got != tc.want {
				t.Errorf("resolveVersion(%q, ...) = %q, want %q", tc.ldflags, got, tc.want)
			}
		})
	}
}
