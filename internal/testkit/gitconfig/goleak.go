package gitconfig

import "go.uber.org/goleak"

// WithGoleak wraps the test runner with goleak's TestMain verification. Its
// cleanup captures the exit code instead of exiting so the caller can return it.
func WithGoleak(m interface{ Run() int }) interface{ Run() int } {
	return goleakRunner{m: m}
}

type goleakRunner struct{ m interface{ Run() int } }

func (r goleakRunner) Run() (code int) {
	goleak.VerifyTestMain(r.m, goleak.Cleanup(func(result int) { code = result }))
	return code
}
