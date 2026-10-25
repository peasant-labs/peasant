package main

import (
	"strings"
	"testing"
)

// TestVerifyContentFailsOnContentWhoseSessionRowIsGone guards #682: content
// rows left behind by a deleted sessions row are orphans, and verify
// --content must count them and fail instead of reporting PASSED.
func TestVerifyContentFailsOnContentWhoseSessionRowIsGone(t *testing.T) {
	t.Parallel()
	seed := seedVerifyHarvest(t)
	seed.exec(t, `PRAGMA foreign_keys=OFF; DELETE FROM sessions WHERE session_id='`+seed.sessionID+`';`)

	out, err := seed.run(t, "verify", "--content")
	if err == nil || strings.Contains(out, "Status: PASSED") {
		t.Errorf("verify --content after deleting the sessions row: err = %v, want a failure and no PASSED:\n%s", err, out)
	}
	if strings.Contains(out, "orphan content: 0 bodies") {
		t.Errorf("verify --content did not count the ownerless bodies:\n%s", out)
	}
	if !strings.Contains(out, seed.sessionID) {
		t.Errorf("verify --content did not name the session whose row is gone:\n%s", out)
	}

	// The printed recovery must heal it.
	if out, err := seed.run(t, "--force", "--session", seed.sessionID); err != nil {
		t.Fatalf("recovery harvest: %v\n%s", err, out)
	}
	if out, err := seed.run(t, "verify", "--content"); err != nil || !strings.Contains(out, "Status: PASSED") {
		t.Errorf("verify --content after the printed recovery: err = %v, want PASSED:\n%s", err, out)
	}
}
