package push_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/peasant-labs/peasant/internal/push"
)

func TestClassifyPushError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want push.PushErrorCategory
	}{
		{"nil", nil, push.CategoryOther},
		{"no model", fmt.Errorf("session x: %w", push.ErrNoModel), push.CategoryNoModel},
		{"metadata missing", fmt.Errorf("read metadata /p: %w: %w", push.ErrMetadataMissing, errors.New("no such file")), push.CategoryMetadataMissing},
		{"village rejected", fmt.Errorf("upload: %w: %w", push.ErrVillageRejected, errors.New("village returned 422")), push.CategoryVillageRejected},
		{"network sentinel", fmt.Errorf("upload: %w: %w", push.ErrNetwork, errors.New("boom")), push.CategoryNetwork},
		{"network via connection error", errors.New("execute request: dial tcp: connection refused"), push.CategoryNetwork},
		{"unknown", errors.New("something else"), push.CategoryOther},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := push.ClassifyPushError(tc.err); got != tc.want {
				t.Errorf("ClassifyPushError(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestSummarizePushErrors_CountsAndDeterministicOrder(t *testing.T) {
	t.Parallel()
	result := &push.PushResult{
		Sessions: []push.SessionPushResult{
			{SessionID: "ok", Status: push.PushStatusNew},
			{SessionID: "n1", Status: push.PushStatusError, Error: fmt.Errorf("upload: %w: x", push.ErrNetwork)},
			{SessionID: "n2", Status: push.PushStatusError, Error: fmt.Errorf("upload: %w: y", push.ErrNetwork)},
			{SessionID: "m1", Status: push.PushStatusError, Error: fmt.Errorf("s: %w", push.ErrNoModel)},
			{SessionID: "m2", Status: push.PushStatusError, Error: fmt.Errorf("s: %w", push.ErrNoModel)},
			{SessionID: "v1", Status: push.PushStatusError, Error: fmt.Errorf("upload: %w: z", push.ErrVillageRejected)},
		},
	}

	rows := push.SummarizePushErrors(result)

	// no-model (2) and network (2) tie on count → deterministic tie-break by
	// category declaration order puts no-model before network. village (1) last.
	if len(rows) != 3 {
		t.Fatalf("expected 3 categories, got %d: %+v", len(rows), rows)
	}
	if rows[0].Category != push.CategoryNoModel || rows[0].Count != 2 {
		t.Errorf("row[0] = %+v, want no-model count 2", rows[0])
	}
	if rows[1].Category != push.CategoryNetwork || rows[1].Count != 2 {
		t.Errorf("row[1] = %+v, want network count 2", rows[1])
	}
	if rows[2].Category != push.CategoryVillageRejected || rows[2].Count != 1 {
		t.Errorf("row[2] = %+v, want village-rejected count 1", rows[2])
	}
	if rows[0].Example == "" {
		t.Error("expected a non-empty Example for the no-model row")
	}
}

func TestSummarizePushErrors_NoErrors(t *testing.T) {
	t.Parallel()
	result := &push.PushResult{
		Sessions: []push.SessionPushResult{{SessionID: "ok", Status: push.PushStatusNew}},
	}
	if rows := push.SummarizePushErrors(result); rows != nil {
		t.Errorf("expected nil for no errors, got %+v", rows)
	}
	if rows := push.SummarizePushErrors(nil); rows != nil {
		t.Errorf("expected nil for nil result, got %+v", rows)
	}
}
