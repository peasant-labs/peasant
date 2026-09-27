package defaults

// EnvVar is a typed environment variable name.
type EnvVar string

func (v EnvVar) String() string { return string(v) }

const (
	EnvXDGConfigHome EnvVar = "XDG_CONFIG_HOME"
	EnvXDGDataHome   EnvVar = "XDG_DATA_HOME"
	EnvXDGStateHome  EnvVar = "XDG_STATE_HOME"
	EnvGoPrivate     EnvVar = "GOPRIVATE"

	// EnvPeasantBin names the seam that supplies a prebuilt peasant binary
	// (plus optional arguments) in place of building one. Shared by the e2e
	// harness and the CLI test suite, so it lives here rather than being
	// re-declared per package.
	EnvPeasantBin EnvVar = "PEASANT_BIN"
)
