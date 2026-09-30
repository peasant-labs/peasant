package testwait

import (
	"testing"
	"time"
)

func TestReceiveReturnsSentValue(t *testing.T) {
	ch := make(chan string, 1)
	ch <- "ready"

	if got := Receive(t, ch, "the readiness signal"); got != "ready" {
		t.Fatalf("Receive = %q, want %q", got, "ready")
	}
}

func TestUntilReturnsWhenConditionFlips(t *testing.T) {
	checks := 0
	Until(t, "the condition to flip", func() bool {
		checks++
		return checks >= 3
	})
	if checks < 3 {
		t.Fatalf("Until returned after %d checks, want at least 3", checks)
	}
}

func TestBoundRule(t *testing.T) {
	now := time.Now()

	at, _ := bound(now, time.Time{}, false)
	if want := now.Add(time.Minute); !at.Equal(want) {
		t.Fatalf("bound with no deadline = %s, want the one-minute fallback %s", at, want)
	}

	far := now.Add(10 * time.Minute)
	at, _ = bound(now, far, true)
	if want := far.Add(-5 * time.Second); !at.Equal(want) {
		t.Fatalf("bound with a far deadline = %s, want %s", at, want)
	}

	at, _ = bound(now, now.Add(-time.Second), true)
	if !at.Equal(now) {
		t.Fatalf("bound with an expired deadline = %s, want the clamped now %s", at, now)
	}
}
