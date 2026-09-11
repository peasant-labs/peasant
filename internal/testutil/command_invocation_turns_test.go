package testutil_test

import (
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
)

func TestLoadCommandInvocationTurnFixture_AcceptsTheCorpus(t *testing.T) {
	t.Parallel()
	fixture, err := testutil.LoadCommandInvocationTurnFixture()
	if err != nil {
		t.Fatalf("LoadCommandInvocationTurnFixture: %v", err)
	}
	if len(fixture.Cases) < len(fixture.RequiredNames) {
		t.Fatalf("corpus has %d cases for %d required names", len(fixture.Cases), len(fixture.RequiredNames))
	}
	for _, testCase := range fixture.Cases {
		if testCase.StoredName == "" {
			t.Errorf("case %q has no stored command name", testCase.Name)
		}
		if !strings.Contains(testCase.StoredExtra, "command_name") {
			t.Errorf("case %q stored extra %q carries no command_name key", testCase.Name, testCase.StoredExtra)
		}
		if testCase.ExpectedCommand && testCase.ExpectedName == "" {
			t.Errorf("case %q expects a command but names none", testCase.Name)
		}
		if !testCase.ExpectedCommand && (testCase.ExpectedName != "" || testCase.ExpectedArgs != "") {
			t.Errorf("case %q expects no command but declares one", testCase.Name)
		}
	}
}

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

func TestCommandInvocationTurnFixture_RequiredNamesAreAllPresent(t *testing.T) {
	t.Parallel()
	fixture, err := testutil.LoadCommandInvocationTurnFixture()
	if err != nil {
		t.Fatalf("LoadCommandInvocationTurnFixture: %v", err)
	}
	present := make(map[string]bool, len(fixture.Cases))
	for _, testCase := range fixture.Cases {
		present[testCase.Name] = true
	}
	for _, required := range fixture.RequiredNames {
		if !present[required] {
			t.Errorf("required case %q is missing from the corpus", required)
		}
	}
}
