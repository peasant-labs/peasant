package testutil

import (
	"testing"

	"github.com/peasant-labs/peasant/internal/testkit/gitconfig"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	if err := gitconfig.IsolateGitConfig(); err != nil {
		panic(err)
	}
	goleak.VerifyTestMain(m)
}
