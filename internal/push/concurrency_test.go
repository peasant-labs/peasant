package push_test

import (
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/push"
)

// TestDefaultConcurrencyForCPU verifies the injected-CPU default = max(1, NumCPU/2),
// host-deterministically (no inline runtime.NumCPU).
func TestDefaultConcurrencyForCPU(t *testing.T) {
	t.Parallel()
	cases := []struct {
		numCPU int
		want   int
	}{
		{1, 1},  // single core floors at 1
		{2, 1},  // 2/2 = 1
		{4, 2},  // 4/2 = 2
		{8, 4},  // 8/2 = 4
		{16, 8}, // 16/2 = 8
		{0, 1},  // defensive: floors at 1
	}
	for _, tc := range cases {
		if got := push.DefaultConcurrencyForCPU(tc.numCPU); got != tc.want {
			t.Errorf("DefaultConcurrencyForCPU(%d) = %d, want %d", tc.numCPU, got, tc.want)
		}
	}
}

// TestResolveConcurrency_Precedence verifies flag > config > CPU-default and that
// an explicit flag <= 0 is rejected with an actionable error.
func TestResolveConcurrency_Precedence(t *testing.T) {
	t.Parallel()

	// Flag set and valid → flag wins (over config and default).
	if got, err := push.ResolveConcurrency(true, 7, 3, 4); err != nil || got != 7 {
		t.Errorf("flag-set: got (%d, %v), want (7, nil)", got, err)
	}

	// Flag not set, config > 0 → config wins (over default).
	if got, err := push.ResolveConcurrency(false, 0, 3, 4); err != nil || got != 3 {
		t.Errorf("config: got (%d, %v), want (3, nil)", got, err)
	}

	// Flag not set, config <= 0 → CPU default max(1, NumCPU/2). NumCPU=4 ⇒ 2.
	if got, err := push.ResolveConcurrency(false, 0, 0, 4); err != nil || got != 2 {
		t.Errorf("default(NumCPU=4): got (%d, %v), want (2, nil)", got, err)
	}
	// NumCPU=1 ⇒ 1 (max guard).
	if got, err := push.ResolveConcurrency(false, 0, 0, 1); err != nil || got != 1 {
		t.Errorf("default(NumCPU=1): got (%d, %v), want (1, nil)", got, err)
	}

	// A negative config value is treated as unset → falls through to the default.
	if got, err := push.ResolveConcurrency(false, 0, -5, 8); err != nil || got != 4 {
		t.Errorf("negative-config: got (%d, %v), want (4, nil)", got, err)
	}
}

// TestResolveConcurrency_RejectsNonPositiveFlag verifies an explicitly-set
// --concurrency of 0 or negative is rejected with an actionable error.
func TestResolveConcurrency_RejectsNonPositiveFlag(t *testing.T) {
	t.Parallel()
	for _, bad := range []int{0, -1, -100} {
		got, err := push.ResolveConcurrency(true, bad, 0, 4)
		if err == nil {
			t.Errorf("ResolveConcurrency(flag=%d) = (%d, nil), want an error", bad, got)
			continue
		}
		// Actionable: states the constraint and the remedy.
		msg := err.Error()
		if !strings.Contains(msg, ">= 1") || !strings.Contains(msg, "--concurrency 1") {
			t.Errorf("error for flag=%d not actionable: %q", bad, msg)
		}
	}
}
