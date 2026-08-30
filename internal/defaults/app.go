package defaults

import "runtime/debug"

// AppVersion is a typed application version string.
type AppVersion string

func (v AppVersion) String() string { return string(v) }

// devVersion is the placeholder used when no version was injected at build time.
const devVersion AppVersion = "dev"

// version is the raw string set by -ldflags:
//
//	-X github.com/peasant-labs/peasant/internal/defaults.version=0.1.0
//
// It MUST stay a plain string var so the linker's -X can write to it. When the
// ldflags are absent (e.g. `go install module@version` or a bare `go build`),
// it keeps the "dev" default and resolveVersion falls back to the module
// version recorded in the build info.
var version = string(devVersion)

// Version is the typed application version, resolved at package init.
var Version AppVersion

func init() {
	Version = resolveVersion(version, debug.ReadBuildInfo)
}

// resolveVersion selects the application version, preferring the ldflags-injected
// value and falling back to Go module build info.
//
//   - If the ldflags-injected version is a real value (not the "dev"
//     placeholder), use it. Release builds, explicit `make build VERSION=<tag>`
//     runs, and default Makefile source builds all inject this value.
//   - Otherwise the binary was built without -X (notably `go install
//     github.com/peasant-labs/peasant/cmd/peasant@v1.2.3`). Go records the
//     module version in debug.BuildInfo.Main.Version, so use that when present.
//   - If build info is unavailable, or reports the no-version sentinel "(devel)"
//     (a from-source build with no module version) or empty, keep "dev".
//
// readBuildInfo is injected (rather than calling debug.ReadBuildInfo directly)
// so the fallback branch is unit-testable without an actual module build.
func resolveVersion(ldflagsVersion string, readBuildInfo func() (*debug.BuildInfo, bool)) AppVersion {
	if v := AppVersion(ldflagsVersion); v != "" && v != devVersion {
		return v
	}
	if info, ok := readBuildInfo(); ok && info != nil {
		// "(devel)" is the Go toolchain's sentinel for a build with no resolved
		// module version (source build / `go run`); treat it as no-version.
		if mv := info.Main.Version; mv != "" && mv != "(devel)" {
			return AppVersion(mv)
		}
	}
	return devVersion
}
