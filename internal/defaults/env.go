package defaults

// EnvVar is a typed environment variable name.
type EnvVar string

func (v EnvVar) String() string { return string(v) }

const (
	EnvXDGConfigHome EnvVar = "XDG_CONFIG_HOME"
	EnvXDGDataHome   EnvVar = "XDG_DATA_HOME"
	EnvXDGStateHome  EnvVar = "XDG_STATE_HOME"
	EnvGoPrivate     EnvVar = "GOPRIVATE"
)
