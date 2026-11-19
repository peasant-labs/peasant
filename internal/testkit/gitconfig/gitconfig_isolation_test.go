package gitconfig

import (
	"fmt"
	"os"
	"testing"
)

// Isolate directly so a broken runner helper cannot skip this package's tests.
func TestMain(m *testing.M) {
	if err := isolateGitConfig(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(m.Run())
}
