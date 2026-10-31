package defaults

import "strconv"

// ExitCode is the process status peasant exits with. It is a closed set: a
// caller that branches on a status has to be able to read the meaning of every
// value from one place.
//
// The set is deliberately small. Two distinctions are worth a status of their
// own, both for a caller that cannot read prose. A generated Git hook needs to
// know whether a failed command sent anything to the village at all. Without
// it, a hook reporting "whatever finished is on the village and is recorded as
// published" says exactly the same thing after an expired login, where nothing
// was ever sent. A program driving the CLI needs to know whether it invoked a
// command wrongly (ExitUsage), which retrying the same call will not fix.
type ExitCode int

const (
	// ExitOK is a successful run.
	ExitOK ExitCode = 0
	// ExitFailure is any failure that does not carry a more specific meaning.
	ExitFailure ExitCode = 1
	// ExitUsage is a refusal caused by how the command was invoked: a flag
	// combination, an input list it cannot read, or a confirmation it cannot
	// ask for. Nothing was attempted. Cobra's own flag-parse errors still exit
	// with ExitFailure; this status is for the refusals a command raises
	// itself through internal/clicontract.
	ExitUsage ExitCode = 2
	// ExitNothingAttempted is a command that failed BEFORE it made a single
	// request to the village: an expired or missing login, an unreadable
	// config, a store that would not open, a repository scope that would not
	// resolve. Nothing was published, and nothing was recorded as published.
	//
	// It is kept distinct from 2, ExitUsage, the refusal of a command line
	// that asks for something the command will not do. village push reports
	// its own refusals with this status, usage refusals included, because its
	// callers branch on whether anything reached the village.
	ExitNothingAttempted ExitCode = 3
)

// Int returns the status in the form os.Exit takes.
func (c ExitCode) Int() int { return int(c) }

// String renders the status for messages and for embedding in a generated hook.
func (c ExitCode) String() string { return strconv.Itoa(int(c)) }
