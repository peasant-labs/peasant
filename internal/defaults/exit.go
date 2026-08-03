package defaults

import "strconv"

// ExitCode is the process status peasant exits with. It is a closed set: a
// caller that branches on a status has to be able to read the meaning of every
// value from one place.
//
// The set is deliberately small. Only one distinction is currently worth a
// status of its own, and it exists because a generated Git hook cannot read
// prose: whether a failed command sent anything to the village at all. Without
// it, a hook reporting "whatever finished is on the village and is recorded as
// published" says exactly the same thing after an expired login, where nothing
// was ever sent.
type ExitCode int

const (
	// ExitOK is a successful run.
	ExitOK ExitCode = 0
	// ExitFailure is any failure that does not carry a more specific meaning.
	ExitFailure ExitCode = 1
	// ExitNothingAttempted is a command that failed BEFORE it made a single
	// request to the village: an expired or missing login, an unreadable
	// config, a store that would not open, a repository scope that would not
	// resolve. Nothing was published, and nothing was recorded as published.
	//
	// It is 3 rather than 2 because 2 is conventionally a usage error, which
	// Cobra already handles before a command runs.
	ExitNothingAttempted ExitCode = 3
)

// Int returns the status in the form os.Exit takes.
func (c ExitCode) Int() int { return int(c) }

// String renders the status for messages and for embedding in a generated hook.
func (c ExitCode) String() string { return strconv.Itoa(int(c)) }
