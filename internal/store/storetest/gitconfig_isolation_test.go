package storetest

import (
	"os"
	"testing"

	"github.com/peasant-labs/peasant/internal/testkit/gitconfig"
)

func TestMain(m *testing.M) { os.Exit(gitconfig.RunWithIsolatedGitConfig(m)) }
