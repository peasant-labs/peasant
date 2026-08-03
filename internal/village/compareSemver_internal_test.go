package village

import (
	"testing"

	"github.com/peasant-labs/schema"
)

// compareSemver is package-internal (unexported, no external consumer), so its
// test must live in-package (package village, not village_test) to reach it.
func TestCompareSemver(t *testing.T) {
	cases := []struct {
		a, b schema.PushContractVersion
		want int
	}{
		{"1.2.3", "1.2.3", 0},
		{"1.2.0", "1.3.0", -1},
		{"2.0.0", "1.9.9", 1},
		{"0.10.0", "0.9.0", 1}, // numeric, not lexical
		{"0.0.1", "0.0.2", -1},
	}
	for _, c := range cases {
		if got := compareSemver(c.a, c.b); got != c.want {
			t.Errorf("compareSemver(%q,%q): got %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
