package main

import (
	"reflect"
	"testing"

	"github.com/peasant-labs/peasant/internal/push"
)

// TestAnnotationSelectionForRun_ChooserScopesToPublishedSessions pins the CLI
// door's annotation decision, which the interactive chooser cannot reach in a
// test without a terminal. A run the chooser narrowed publishes only the
// annotations of the sessions the village now holds, keeping the label flags;
// any other run keeps the selection built from configuration and flags.
func TestAnnotationSelectionForRun_ChooserScopesToPublishedSessions(t *testing.T) {
	t.Parallel()
	labels := push.AnnotationSelection{IDs: map[string]bool{"chosen-label": true}}
	result := &push.PushResult{Sessions: []push.SessionPushResult{
		{SessionID: "published", Status: push.PushStatusNew},
		{SessionID: "failed", Status: push.PushStatusError},
	}}

	narrowed := annotationSelectionForRun(labels, true, result)
	if !reflect.DeepEqual(narrowed, labels.WithinPublishedSessions(result)) || !narrowed.SessionsOnly || !narrowed.SessionIDs["published"] || narrowed.SessionIDs["failed"] || !narrowed.IDs["chosen-label"] {
		t.Fatalf("a chooser-narrowed run must publish only the published session's annotations with the chosen labels; got %+v", narrowed)
	}
	if unchanged := annotationSelectionForRun(labels, false, result); !reflect.DeepEqual(unchanged, labels) {
		t.Fatalf("a run the chooser did not narrow must keep its selection; got %+v", unchanged)
	}
}
