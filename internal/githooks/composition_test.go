package githooks_test

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/githooks"
)

//go:embed testdata/manual_composition.yaml
var manualCompositionFixtureData []byte

const manualCompositionFixturePath = "internal/githooks/testdata/manual_composition.yaml"

// snippetPlacement is where the by-hand section goes inside the user's hook. The
// set is closed because the snippet's own instruction names exactly these two,
// and both have to be safe to follow literally.
type snippetPlacement string

const (
	// placementAppend puts the section after every line the user wrote.
	placementAppend snippetPlacement = "append"
	// placementBeforeFinalExit puts the section directly above a final exit.
	placementBeforeFinalExit snippetPlacement = "before-final-exit"
)

var allSnippetPlacements = [...]snippetPlacement{placementAppend, placementBeforeFinalExit}

type manualCompositionDocument struct {
	ExpectedCaseCount int                     `yaml:"expectedCaseCount"`
	Cases             []manualCompositionCase `yaml:"cases"`
}

type manualCompositionCase struct {
	Name           string           `yaml:"name"`
	Event          githooks.Event   `yaml:"event"`
	HostLines      []string         `yaml:"hostLines"`
	Placement      snippetPlacement `yaml:"placement"`
	UploadExitCode int              `yaml:"uploadExitCode"`
	ExpectWarning  bool             `yaml:"expectWarning"`
	ExpectedExit   int              `yaml:"expectedExit"`
}

// loadManualCompositionFixture decodes and fully validates the corpus.
func loadManualCompositionFixture(data []byte) (manualCompositionDocument, error) {
	document, err := decodeFixtureDocument[manualCompositionDocument](data, manualCompositionFixturePath)
	if err != nil {
		return document, err
	}
	if err := fixtureCountGuard(manualCompositionFixturePath, document.ExpectedCaseCount, len(document.Cases)); err != nil {
		return document, err
	}
	names := make([]string, 0, len(document.Cases))
	for _, testCase := range document.Cases {
		names = append(names, testCase.Name)
	}
	if err := fixtureUniqueNames(manualCompositionFixturePath, names); err != nil {
		return document, err
	}
	for index, testCase := range document.Cases {
		if err := testCase.Event.Validate(); err != nil {
			return document, fixtureCaseError(manualCompositionFixturePath, index,
				fmt.Sprintf("unsupported event %q", testCase.Event), "fix=use a managed event")
		}
		if len(testCase.HostLines) == 0 {
			return document, fixtureCaseError(manualCompositionFixturePath, index, "hostLines is empty",
				"fix=write the hook the user composed, so the section is proven to compose with something")
		}
		if !containsValue(allSnippetPlacements[:], testCase.Placement) {
			return document, fixtureCaseError(manualCompositionFixturePath, index,
				fmt.Sprintf("unsupported placement %q", testCase.Placement),
				"fix=use append or before-final-exit, the two placements the snippet itself names")
		}
		endsWithExit := strings.HasPrefix(strings.TrimSpace(testCase.HostLines[len(testCase.HostLines)-1]), "exit")
		if testCase.Placement == placementBeforeFinalExit && !endsWithExit {
			return document, fixtureCaseError(manualCompositionFixturePath, index,
				"placement before-final-exit needs a final exit line to sit above",
				"fix=end hostLines with an exit, or use placement append")
		}
		if testCase.Placement == placementAppend && hasExitLine(testCase.HostLines) {
			return document, fixtureCaseError(manualCompositionFixturePath, index,
				"an appended case must not contain an exit line",
				"fix=drop the exit, or use placement before-final-exit; an appended section below an exit never runs and would prove nothing")
		}
		if testCase.UploadExitCode < 0 || testCase.UploadExitCode > 125 {
			return document, fixtureCaseError(manualCompositionFixturePath, index,
				fmt.Sprintf("uploadExitCode %d is not a usable exit status", testCase.UploadExitCode),
				"fix=use a status between 0 and 125")
		}
		if testCase.ExpectedExit < 0 || testCase.ExpectedExit > 125 {
			return document, fixtureCaseError(manualCompositionFixturePath, index,
				fmt.Sprintf("expectedExit %d is not a usable exit status", testCase.ExpectedExit),
				"fix=use a status between 0 and 125")
		}
		if testCase.ExpectWarning != (testCase.UploadExitCode != 0) {
			return document, fixtureCaseError(manualCompositionFixturePath, index,
				fmt.Sprintf("expectWarning=%v contradicts uploadExitCode=%d", testCase.ExpectWarning, testCase.UploadExitCode),
				"fix=expect a warning exactly when the upload fails")
		}
	}
	return document, nil
}

func hasExitLine(lines []string) bool {
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "exit") {
			return true
		}
	}
	return false
}

// --- loader guards ----------------------------------------------------------

func TestLoadManualCompositionFixture_RejectsUnreachableAppendedSection(t *testing.T) {
	t.Parallel()
	_, err := loadManualCompositionFixture([]byte(`expectedCaseCount: 1
cases:
  - name: appended-below-an-exit
    event: post-commit
    hostLines: ["true", "exit 0"]
    placement: append
    uploadExitCode: 0
    expectWarning: false
    expectedExit: 0
`))
	if err == nil || !strings.Contains(err.Error(), "must not contain an exit line") {
		t.Fatalf("error = %v, want rejection of an appended case whose section could never run", err)
	}
}

func TestLoadManualCompositionFixture_RejectsPlacementWithoutAFinalExit(t *testing.T) {
	t.Parallel()
	_, err := loadManualCompositionFixture([]byte(`expectedCaseCount: 1
cases:
  - name: before-a-missing-exit
    event: post-commit
    hostLines: ["true"]
    placement: before-final-exit
    uploadExitCode: 0
    expectWarning: false
    expectedExit: 0
`))
	if err == nil || !strings.Contains(err.Error(), "needs a final exit line") {
		t.Fatalf("error = %v, want rejection of a placement with nothing to sit above", err)
	}
}

func TestLoadManualCompositionFixture_RejectsWarningWithoutFailure(t *testing.T) {
	t.Parallel()
	_, err := loadManualCompositionFixture([]byte(`expectedCaseCount: 1
cases:
  - name: warning-without-a-failure
    event: post-commit
    hostLines: ["true"]
    placement: append
    uploadExitCode: 0
    expectWarning: true
    expectedExit: 0
`))
	if err == nil || !strings.Contains(err.Error(), "contradicts uploadExitCode") {
		t.Fatalf("error = %v, want warning/exit-code contradiction rejection", err)
	}
}

// --- the corpus -------------------------------------------------------------

// TestManualSnippet_ComposesWithoutChangingTheHooksStatus runs the real snippet
// inside a real hook, executed the way git executes one, for every placement the
// snippet's own instruction offers.
//
// The before-final-exit cases are the ones that matter: a snippet that ended by
// exiting would satisfy every appended case while silently swallowing the host
// hook's own `exit 1`, so a pre-push secret scanner or policy gate would become
// a no-op and the push would proceed.
func TestManualSnippet_ComposesWithoutChangingTheHooksStatus(t *testing.T) {
	t.Parallel()
	document, err := loadManualCompositionFixture(manualCompositionFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range document.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			repo := disposableRepo(t)
			slot := hookPath(t, repo, testCase.Event.String())
			snippet, snippetErr := githooks.ManualSnippet(testCase.Event, repo, slot, githooks.Binding{})
			if snippetErr != nil {
				t.Fatalf("render manual snippet: %v", snippetErr)
			}

			composed := composeHook(testCase, snippet)
			assertShellParses(t, testCase.Name, composed)
			hook := filepath.Join(t.TempDir(), "composed-"+testCase.Event.String())
			if writeErr := os.WriteFile(hook, []byte(composed), 0o755); writeErr != nil {
				t.Fatalf("write composed hook: %v", writeErr)
			}

			binDir, log := stubPeasant(t, testCase.UploadExitCode)
			stdin := ""
			if testCase.Event == githooks.EventPrePush {
				stdin = "refs/heads/main abc refs/heads/main def\n"
			}
			_, stderr, code := runHookStatus(t, repo, hook, binDir, stdin)

			if code != testCase.ExpectedExit {
				t.Errorf("composed hook exited %d, want %d\n--- hook ---\n%s\n--- stderr ---\n%s",
					code, testCase.ExpectedExit, composed, stderr)
			}
			if recorded, want := recordedArgs(t, log), wantRepositoryArgv(repo); !slices.Equal(recorded, want) {
				t.Errorf("the by-hand section did not run the upload: peasant saw %#v, want %#v", recorded, want)
			}
			warned := strings.Contains(stderr, "village upload failed for")
			if warned != testCase.ExpectWarning {
				t.Errorf("upload warning printed = %v, want %v; stderr:\n%s", warned, testCase.ExpectWarning, stderr)
			}
		})
	}
}

// composeHook builds the hook the user would end up with after following the
// snippet's placement instruction.
func composeHook(testCase manualCompositionCase, snippet string) string {
	var body strings.Builder
	body.WriteString("#!/bin/sh\n")
	switch testCase.Placement {
	case placementBeforeFinalExit:
		final := len(testCase.HostLines) - 1
		for _, line := range testCase.HostLines[:final] {
			body.WriteString(line + "\n")
		}
		body.WriteString(snippet)
		body.WriteString(testCase.HostLines[final] + "\n")
	default:
		for _, line := range testCase.HostLines {
			body.WriteString(line + "\n")
		}
		body.WriteString(snippet)
	}
	return body.String()
}

// runHookStatus runs a hook and reports the exit status git would observe.
func runHookStatus(t *testing.T, repo, hook, binDir, stdin string) (string, string, int) {
	t.Helper()
	stdout, stderr, err := runHook(t, repo, hook, binDir, stdin)
	if err == nil {
		return stdout, stderr, 0
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("running %s failed before it could report a status: %v", hook, err)
	}
	return stdout, stderr, exitErr.ExitCode()
}
