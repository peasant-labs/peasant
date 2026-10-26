package collectiveobservations

import (
	"fmt"
	"os"
	"time"
)

const (
	// EnvFetchTimeout configures the normal CLI/Local API metadata deadline.
	EnvFetchTimeout     = "PEASANT_COLLECTIVE_FETCH_TIMEOUT"
	DefaultFetchTimeout = 2 * time.Minute
	MinFetchTimeout     = 100 * time.Millisecond
	MaxFetchTimeout     = 10 * time.Minute
)

// RuntimeFetchTimeout validates process configuration before startup or local
// Store access. It changes no HTTP contract and never silently accepts an
// invalid override. An absent variable preserves the normal two-minute limit.
func RuntimeFetchTimeout() (time.Duration, error) {
	raw, present := os.LookupEnv(EnvFetchTimeout)
	if !present {
		return DefaultFetchTimeout, nil
	}
	duration, err := time.ParseDuration(raw)
	if err != nil || duration < MinFetchTimeout || duration > MaxFetchTimeout {
		return 0, fmt.Errorf("configure collective metadata deadline: %s must be a duration from %s through %s; startup/action was not attempted; unset it for %s or set a bounded duration such as 2s", EnvFetchTimeout, MinFetchTimeout, MaxFetchTimeout, DefaultFetchTimeout)
	}
	return duration, nil
}
