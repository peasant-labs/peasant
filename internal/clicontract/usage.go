package clicontract

// UsageError is a refusal caused by how the command was invoked: a flag
// combination, an input list that cannot be read, or a confirmation that
// cannot be asked. Nothing was attempted. The command exits with status 2
// (defaults.ExitUsage) so a caller can tell a fixable invocation from a run
// that failed.
type UsageError struct {
	msg string
}

// NewUsageError returns a usage error with the given message. The message
// should say what went wrong, why, and how to fix it.
func NewUsageError(msg string) *UsageError {
	return &UsageError{msg: msg}
}

// Error implements error.
func (e *UsageError) Error() string {
	return e.msg
}
