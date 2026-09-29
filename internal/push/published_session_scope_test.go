package push_test

import (
	_ "embed"
	"slices"
	"testing"

	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/published_session_scope.yaml
var publishedSessionScopeFixture []byte

// allPushStatuses enumerates every status the pipeline can report, from the
// type's own String cases, so a new named status is listed without an edit
// here.
func allPushStatuses() []push.PushStatus {
	var statuses []push.PushStatus
	for status := push.PushStatus(0); status.String() != "unknown"; status++ {
		statuses = append(statuses, status)
	}
	return statuses
}

// TestWithinPublishedSessions_KeepsOnlySessionsTheVillageHolds scopes an
// annotation push to one run holding a session in every status, and requires
// exactly the corpus's included statuses to carry annotations.
func TestWithinPublishedSessions_KeepsOnlySessionsTheVillageHolds(t *testing.T) {
	t.Parallel()
	var corpus struct {
		Included []string `yaml:"included"`
		Excluded []string `yaml:"excluded"`
	}
	if err := testutil.DecodeFixtureYAML(publishedSessionScopeFixture, &corpus); err != nil {
		t.Fatal(err)
	}
	listed := append(slices.Clone(corpus.Included), corpus.Excluded...)
	result := &push.PushResult{}
	var names []string
	for _, status := range allPushStatuses() {
		names = append(names, status.String())
		result.Sessions = append(result.Sessions, push.SessionPushResult{SessionID: "session-" + status.String(), Status: status})
	}
	slices.Sort(listed)
	slices.Sort(names)
	if !slices.Equal(listed, names) {
		t.Fatalf("testdata/published_session_scope.yaml lists statuses %v; every status %v must be included or excluded exactly once", listed, names)
	}

	labels := push.AnnotationSelection{IDs: map[string]bool{"chosen-label": true}}
	scope := labels.WithinPublishedSessions(result)
	if !scope.SessionsOnly || !scope.IDs["chosen-label"] {
		t.Fatalf("scope = %+v; it must keep the chosen labels and admit only annotations that name a session", scope)
	}
	for _, status := range corpus.Included {
		if !scope.SessionIDs["session-"+status] {
			t.Errorf("a %s session must carry its annotations; scope = %v", status, scope.SessionIDs)
		}
	}
	for _, status := range corpus.Excluded {
		if scope.SessionIDs["session-"+status] {
			t.Errorf("a %s session has nothing on the Village to annotate; scope = %v", status, scope.SessionIDs)
		}
	}
	narrowed := push.AnnotationSelection{SessionIDs: map[string]bool{"session-new": true}}.WithinPublishedSessions(result)
	if len(narrowed.SessionIDs) != 1 || !narrowed.SessionIDs["session-new"] {
		t.Errorf("a selection that already names sessions must keep only those it names; got %v", narrowed.SessionIDs)
	}
	if empty := (push.AnnotationSelection{}).WithinPublishedSessions(nil); !empty.SessionsOnly || empty.SessionIDs == nil || len(empty.SessionIDs) != 0 {
		t.Errorf("a run with no result must scope to no session at all; got %+v", empty)
	}
}
