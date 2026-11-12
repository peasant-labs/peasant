package api

import (
	_ "embed"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/peasant-labs/peasant/internal/onboarding"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/kickstart_errors.yaml
var kickstartErrorsYAML []byte

// kickstartCodeSource says where one error code is shown to happen: a case of a
// kickstart fixture that asserts it, a row of a table in the error fixture, or
// why no request reaches it. Exactly one of Case, Table, Unreachable is set;
// Fixture names the fixture file of a case and is empty for the error fixture.
type kickstartCodeSource struct {
	Fixture     string `yaml:"fixture"`
	Case        string `yaml:"case"`
	Table       string `yaml:"table"`
	Unreachable string `yaml:"unreachable"`
}

type kickstartCommitFailure struct {
	Kind string `yaml:"kind"`
	Code string `yaml:"code"`
}

type kickstartWriterRow struct {
	Failure string `yaml:"failure"`
	Code    string `yaml:"code"`
}

// TestKickstartErrors runs the error fixture against the mounted server.
func TestKickstartErrors(t *testing.T) {
	runKickstartScript(t, kickstartErrorsYAML)
}

// TestKickstartEveryErrorCodeIsAccountedFor holds the error fixture's manifest
// to the contract's closed set, in both directions: no code is left out, none is
// invented, and each one the manifest says a case drives is asserted by that
// case.
func TestKickstartEveryErrorCodeIsAccountedFor(t *testing.T) {
	t.Parallel()
	errors := decodeKickstartScript(t, kickstartErrorsYAML)
	fixtures := map[string]kickstartScript{
		"":         errors,
		"sessions": decodeKickstartScript(t, kickstartSessionsYAML),
		"patch":    decodeKickstartScript(t, kickstartPatchYAML),
		"tree":     decodeKickstartScript(t, kickstartTreeYAML),
		"commit":   decodeKickstartScript(t, kickstartCommitYAML),
	}

	var want, got []string
	for _, code := range schema.AllLocalKickstartErrorCodes {
		want = append(want, code.String())
	}
	for code := range errors.Codes {
		got = append(got, code)
	}
	sort.Strings(want)
	sort.Strings(got)
	if len(want) != len(got) {
		t.Fatalf("the manifest places codes %v, the contract's closed set is %v", got, want)
	}
	for index := range want {
		if want[index] != got[index] {
			t.Fatalf("the manifest places codes %v, the contract's closed set is %v", got, want)
		}
	}

	for code, source := range errors.Codes {
		t.Run(code, func(t *testing.T) {
			t.Parallel()
			set := 0
			for _, present := range []bool{source.Case != "", source.Table != "", source.Unreachable != ""} {
				if present {
					set++
				}
			}
			if set != 1 {
				t.Fatalf("code %s must name exactly one of a case, a table, or why it is unreachable", code)
			}
			switch {
			case source.Case != "":
				fixture, ok := fixtures[source.Fixture]
				if !ok {
					t.Fatalf("code %s names fixture %q, which is not a kickstart fixture", code, source.Fixture)
				}
				if !kickstartCaseAssertsCode(fixture, source.Case, code) {
					t.Errorf("code %s is placed in case %q, which does not assert it", code, source.Case)
				}
			case source.Table == "commitFailures":
				if !kickstartTableHasCode(errors.CommitFailures, code) {
					t.Errorf("code %s is placed in the commitFailures table, which has no row for it", code)
				}
			case source.Table == "writer":
				found := false
				for _, row := range errors.Writer {
					found = found || row.Code == code
				}
				if !found {
					t.Errorf("code %s is placed in the writer table, which has no row for it", code)
				}
			case source.Table != "":
				t.Errorf("code %s names table %q, which is not commitFailures or writer", code, source.Table)
			}
		})
	}
}

func kickstartTableHasCode(rows []kickstartCommitFailure, code string) bool {
	for _, row := range rows {
		if row.Code == code {
			return true
		}
	}
	return false
}

// kickstartCaseAssertsCode reports whether a step of the named case requires
// the answer's code to be code.
func kickstartCaseAssertsCode(script kickstartScript, name, code string) bool {
	for _, c := range script.Cases {
		if c.Name != name {
			continue
		}
		for _, step := range c.Steps {
			if got, ok := step.JSON["code"]; ok && got == code {
				return true
			}
		}
	}
	return false
}

// TestKickstartCommitFailuresAnswerTheirCodes runs every reason a save can fail
// through the mapping that answers it, and requires the code the fixture names
// and the status that code answers with.
func TestKickstartCommitFailuresAnswerTheirCodes(t *testing.T) {
	t.Parallel()
	script := decodeKickstartScript(t, kickstartErrorsYAML)
	kinds := map[string]onboarding.CommitFailure{
		"invalid": onboarding.CommitFailureInvalid,
		"read":    onboarding.CommitFailureRead,
		"lock":    onboarding.CommitFailureLock,
		"changed": onboarding.CommitFailureChanged,
		"write":   onboarding.CommitFailureWrite,
	}
	// Every kind the model has is in the table, and the table has no other.
	named := map[onboarding.CommitFailure]bool{}
	for _, row := range script.CommitFailures {
		kind, ok := kinds[row.Kind]
		if !ok {
			t.Fatalf("the commitFailures table has kind %q, which is not a commit failure", row.Kind)
		}
		named[kind] = true
	}
	for _, kind := range onboarding.AllCommitFailures {
		if !named[kind] {
			t.Errorf("commit failure %v has no row in the commitFailures table", kind)
		}
	}
	for _, row := range script.CommitFailures {
		t.Run(row.Kind, func(t *testing.T) {
			t.Parallel()
			failure := commitFailureResponse(&onboarding.CommitError{Kind: kinds[row.Kind], Err: errors.New("the fixture failure")})
			recorder := httptest.NewRecorder()
			writeKickstartError(recorder, failure)
			assertKickstartErrorAnswer(t, recorder, row.Code)
		})
	}
}

// TestKickstartWriterAnswersEveryCodeWithItsOwnStatus writes every code of the
// closed set through the one writer and requires the status the code answers
// with, a body the contract accepts, and the code itself. A failure no code
// names is answered as the defect it is.
func TestKickstartWriterAnswersEveryCodeWithItsOwnStatus(t *testing.T) {
	t.Parallel()
	for _, code := range schema.AllLocalKickstartErrorCodes {
		t.Run(code.String(), func(t *testing.T) {
			t.Parallel()
			recorder := httptest.NewRecorder()
			writeKickstartError(recorder, newKickstartError(code, "The fixture refusal."))
			assertKickstartErrorAnswer(t, recorder, code.String())
		})
	}
	script := decodeKickstartScript(t, kickstartErrorsYAML)
	for _, row := range script.Writer {
		t.Run(row.Failure, func(t *testing.T) {
			t.Parallel()
			recorder := httptest.NewRecorder()
			writeKickstartError(recorder, errors.New("a plain failure"))
			assertKickstartErrorAnswer(t, recorder, row.Code)
		})
	}
}

func assertKickstartErrorAnswer(t *testing.T, recorder *httptest.ResponseRecorder, wantCode string) {
	t.Helper()
	var response schema.LocalKickstartErrorResponse
	if err := response.UnmarshalJSON(recorder.Body.Bytes()); err != nil {
		t.Fatalf("the answer is not the contract's error body: %v; body %s", err, recorder.Body)
	}
	if err := response.Validate(); err != nil {
		t.Fatalf("the answer breaks the contract: %v", err)
	}
	if response.Code.String() != wantCode {
		t.Errorf("code = %s, want %s", response.Code, wantCode)
	}
	if recorder.Code != response.Code.HTTPStatus() || recorder.Code == 0 || recorder.Code == http.StatusOK {
		t.Errorf("status = %d, but code %s answers %d", recorder.Code, response.Code, response.Code.HTTPStatus())
	}
}
