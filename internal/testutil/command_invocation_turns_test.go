package testutil_test

import (
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
)

// TestCommandInvocationTurnFixture_StoredExtraOmitsAbsentArguments asserts the
// one transformation the loader performs for callers: the Extra JSON every
// surface sends through production must omit command_args entirely when the
// harness recorded none, exactly as the indexers write it.
func TestCommandInvocationTurnFixture_StoredExtraOmitsAbsentArguments(t *testing.T) {
	t.Parallel()
	fixture, err := testutil.LoadCommandInvocationTurnFixture()
	if err != nil {
		t.Fatalf("LoadCommandInvocationTurnFixture: %v", err)
	}
	for _, testCase := range fixture.Cases {
		hasArgsKey := strings.Contains(testCase.StoredExtra, "command_args")
		if hasArgsKey != (testCase.StoredArgs != "") {
			t.Errorf("case %q stored extra %q command_args presence = %v, want %v",
				testCase.Name, testCase.StoredExtra, hasArgsKey, testCase.StoredArgs != "")
		}
	}
}
