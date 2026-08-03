//go:build e2e && linux

package e2e

import "testing"

func TestVillageGroupKillTargetRefusesInvalidPID(t *testing.T) {
	for _, pid := range []int{0, -1} {
		if pgid, ok := villageGroupKillTarget(pid); ok {
			t.Fatalf("villageGroupKillTarget(%d) = (%d, true), want ok=false", pid, pgid)
		}
	}

	if pgid, ok := villageGroupKillTarget(1234); !ok || pgid != 1234 {
		t.Fatalf("villageGroupKillTarget(1234) = (%d, %t), want (1234, true)", pgid, ok)
	}
}
