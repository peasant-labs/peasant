package push

import (
	"os"
	"testing"
	"time"
)

// TestMain pins the process time zone to UTC for this package. The wizard
// renders session start times in local time, and the golden renders must not
// depend on the zone of the machine that runs the tests.
func TestMain(m *testing.M) {
	time.Local = time.UTC
	os.Exit(m.Run())
}
