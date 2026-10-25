package commandpr_test

import (
	"reflect"
	"testing"

	"github.com/peasant-labs/peasant/internal/commandpr"
	"github.com/peasant-labs/schema"
)

// TestDetectFromRecordedToolCalls runs the whole corpus through the public
// detection entry point: a stored tool call's arguments and result are the
// only evidence, exactly as the local read endpoints supply them.
func TestDetectFromRecordedToolCalls(t *testing.T) {
	fixture := loadDetectionFixture(t)
	for _, testCase := range fixture.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			calls := make([]schema.ToolCallDetail, len(testCase.Calls))
			for index, call := range testCase.Calls {
				calls[index] = schema.ToolCallDetail{
					Name:      "Bash",
					Arguments: call.Arguments,
					Result:    call.Result,
					IsError:   call.IsError,
				}
			}

			got := commandpr.Detect(calls)
			want := expectedPullRequests(testCase.Expected)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("Detect() = %+v, want %+v", got, want)
			}
		})
	}
}

// TestDetectPullRequestURLEdgeCases pins the URL extraction rules through the
// same public entry point: a real invocation plus each result text, so the
// edge cases exercise production extraction rather than a private helper.
func TestDetectPullRequestURLEdgeCases(t *testing.T) {
	fixture := loadDetectionFixture(t)
	for _, urlCase := range fixture.URLs {
		t.Run(urlCase.Name, func(t *testing.T) {
			t.Parallel()
			calls := []schema.ToolCallDetail{{
				Name:      "Bash",
				Arguments: `{"command":"gh pr create --fill"}`,
				Result:    urlCase.Text,
			}}

			var want []commandpr.PullRequest
			if urlCase.Expected != (pullRequestRow{}) {
				want = expectedPullRequests([]pullRequestRow{urlCase.Expected})
			}
			got := commandpr.Detect(calls)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("Detect() = %+v, want %+v", got, want)
			}
		})
	}
}

// TestDetectEmptyInput pins the empty case the endpoint relies on: no tool
// calls is no detection, never an error or a one-row placeholder.
func TestDetectEmptyInput(t *testing.T) {
	t.Parallel()
	if got := commandpr.Detect(nil); got != nil {
		t.Fatalf("Detect(nil) = %+v, want no rows", got)
	}
	if got := commandpr.Detect([]schema.ToolCallDetail{}); got != nil {
		t.Fatalf("Detect(empty) = %+v, want no rows", got)
	}
}
