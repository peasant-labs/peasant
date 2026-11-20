package executable_test

import (
	"os"
	"testing"

	"github.com/peasant-labs/peasant/internal/testkit/gitconfig"
)

// TestMain isolates child processes from host git configuration.
func TestMain(m *testing.M) { os.Exit(gitconfig.RunWithIsolatedGitConfig(m)) }
