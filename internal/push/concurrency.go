package push

import "fmt"

// DefaultConcurrencyForCPU returns the default upload concurrency for a host with
// numCPU logical CPUs: max(1, numCPU/2). Half the cores leaves headroom for the
// rest of the run (redaction, DB, the village process on a local dev box) while
// still parallelizing uploads, and it also sizes the HTTP connection pool.
//
// numCPU is an INJECTED parameter (not an inline runtime.NumCPU() call) so the
// default is host-deterministically testable: NumCPU=4 ⇒ 2, NumCPU=1 ⇒ 1 (the
// max guard floors it at 1 on a single-core host).
func DefaultConcurrencyForCPU(numCPU int) int {
	return max(1, numCPU/2)
}

// ResolveConcurrency resolves the effective upload concurrency from the CLI flag
// and the push.concurrency config value, with the CPU-derived default as the
// fallback. Precedence (highest first):
//
//  1. an explicitly-set --concurrency flag (flagSet == true),
//  2. push.concurrency from config (when > 0),
//  3. DefaultConcurrencyForCPU(numCPU).
//
// An explicitly-set flag of <= 0 is REJECTED with an actionable error: 0/negative
// parallelism is nonsensical (it sets both the upload parallelism and the HTTP
// connection-pool size), and the user almost certainly meant 1 (serial). A
// config value of <= 0 is treated as "unset" and falls through to the default
// rather than erroring, so a stray config key never blocks a push.
func ResolveConcurrency(flagSet bool, flagVal, cfgVal, numCPU int) (int, error) {
	if flagSet {
		if flagVal <= 0 {
			return 0, fmt.Errorf(
				"--concurrency must be >= 1 (got %d): it sets how many uploads run in parallel and the size of the HTTP connection pool; pass --concurrency 1 for serial uploads",
				flagVal)
		}
		return flagVal, nil
	}
	if cfgVal > 0 {
		return cfgVal, nil
	}
	return DefaultConcurrencyForCPU(numCPU), nil
}
